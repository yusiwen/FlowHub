// Package dispatch turns an accepted webhook delivery into an opencode turn.
//
// It is the only place that joins the three halves of FlowHub: the routing table
// (which repository), the task registry (which session) and the opencode runner
// (which turn). Everything here runs on a worker goroutine, never on the webhook
// request path, because the publisher of the webhook waits for our 202.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yusiwen/flowhub/internal/opencode"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/store"
	"github.com/yusiwen/flowhub/internal/webhook"
	"github.com/yusiwen/flowhub/internal/worktree"
)

// AllowedMCPTools are the tools the agent may call without asking. They are
// listed in the session ruleset as well, because a request that never asks costs
// nothing while one that asks waits for the next poll tick.
//
// Everything absent from this list stays gated: the ruleset asks and the arbiter
// denies. The measured inventory includes trackers writers (`youtrack_update_issue`),
// web crawlers and cross-session memory, none of which an unattended turn needs.
var AllowedMCPTools = []string{
	"youtrack_get_issue",
	"youtrack_get_issue_comments",
	"youtrack_search_issues",
	"youtrack_get_project",
	"youtrack_get_issue_fields_schema",
	"youtrack_find_projects",
	"youtrack_get_current_user",
	"youtrack_add_issue_comment",
}

// Options configures a Dispatcher.
type Options struct {
	Client   *opencode.Client
	Registry *registry.Registry
	Projects *projectmap.Map
	Rules    rules.Policy
	Log      *slog.Logger
	// WorktreeBase is the fallback directory for task worktrees, used when a
	// routing entry does not declare its own. Entries normally declare one,
	// because the base has to be outside the repository it belongs to.
	WorktreeBase string
	// Agent is the opencode agent name; empty uses the server default.
	Agent string
	// Deadline bounds one turn.
	Deadline time.Duration
	// QueueSize bounds the deliveries waiting for the worker.
	QueueSize int
	// PauseFile disables dispatch while it exists.
	PauseFile string
	// AttachmentsDir is where downloads must be written, relative to the worktree.
	AttachmentsDir string
	// MaxCostPerTask stops a task whose accumulated cost passes this value.
	// Zero disables the check.
	MaxCostPerTask float64
}

// Dispatcher consumes accepted deliveries.
type Dispatcher struct {
	opts Options
	log  *slog.Logger

	// managers caches one worktree manager per base directory. Only the worker
	// goroutine touches it, which is why it needs no lock.
	managers map[string]*worktree.Manager

	queue   chan *store.Record
	queued  atomic.Int64
	handled atomic.Int64
	dropped atomic.Int64
	ignored atomic.Int64
}

// New builds a dispatcher. It does not start working until Run is called.
func New(opts Options) (*Dispatcher, error) {
	if opts.Client == nil {
		return nil, errors.New("dispatch: an opencode client is required")
	}
	if opts.Registry == nil {
		return nil, errors.New("dispatch: a registry is required")
	}

	if opts.Projects == nil {
		return nil, errors.New("dispatch: a routing table is required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 32
	}
	if opts.Deadline <= 0 {
		opts.Deadline = 10 * time.Minute
	}
	if opts.AttachmentsDir == "" {
		opts.AttachmentsDir = opencode.DefaultAttachmentPathPrefix
	}
	opts.Rules = opts.Rules.Defaults()

	return &Dispatcher{
		opts:     opts,
		log:      opts.Log,
		managers: map[string]*worktree.Manager{},
		queue:    make(chan *store.Record, opts.QueueSize),
	}, nil
}

// Dispatch queues a delivery. It never blocks: the webhook request path is
// synchronous on the publisher's side, so a full queue drops work instead.
func (d *Dispatcher) Dispatch(rec *store.Record) {
	if rec == nil || !rec.Accepted {
		return
	}
	select {
	case d.queue <- rec:
		d.queued.Add(1)
	default:
		d.dropped.Add(1)
		d.log.Error("dispatch queue is full, dropping delivery",
			"event", rec.Event, "issue", rec.IssueID, "queue_size", d.opts.QueueSize)
	}
}

// Run processes deliveries until the context is cancelled.
//
// One worker is deliberate: the task model is "one turn at a time", and a busy
// session silently swallows a prompt (opencode issue #46842), so more workers
// would need per-task locking before they could be safe.
func (d *Dispatcher) Run(ctx context.Context) {
	d.log.Info("dispatcher started",
		"queue_size", d.opts.QueueSize, "deadline", d.opts.Deadline,
		"agent", d.opts.Agent, "analyze_on_create", !d.opts.Rules.SkipAnalyzeOnCreate,
		"trigger", d.opts.Rules.Trigger)
	for {
		select {
		case <-ctx.Done():
			d.log.Info("dispatcher stopped", "queued", d.queued.Load(), "handled", d.handled.Load())
			return
		case rec := <-d.queue:
			d.queued.Add(-1)
			d.handle(ctx, rec)
			d.handled.Add(1)
		}
	}
}

// Paused reports whether the pause file exists. The file is the kill switch: it
// needs no API, no restart and no credentials.
func (d *Dispatcher) Paused() bool {
	if d.opts.PauseFile == "" {
		return false
	}
	_, err := os.Stat(d.opts.PauseFile)
	return err == nil
}

// Snapshot reports the counters for /healthz.
func (d *Dispatcher) Snapshot() map[string]any {
	return map[string]any{
		"queued":      d.queued.Load(),
		"handled":     d.handled.Load(),
		"ignored":     d.ignored.Load(),
		"dropped":     d.dropped.Load(),
		"queue_depth": len(d.queue),
		"paused":      d.Paused(),
		"agent":       d.opts.Agent,
	}
}

func (d *Dispatcher) handle(ctx context.Context, rec *store.Record) {
	if d.Paused() {
		d.ignored.Add(1)
		d.log.Warn("dispatch is paused; delivery ignored",
			"pause_file", d.opts.PauseFile, "issue", rec.IssueID, "event", rec.Event)
		return
	}

	payload, _, err := webhook.Parse([]byte(rec.RawBody))
	if err != nil {
		// A delivery that reached this point has already been parsed once; a
		// failure here means the audit record was tampered with.
		d.log.Error("cannot re-parse an accepted delivery", "issue", rec.IssueID, "error", err)
		return
	}

	delivery := describe(rec, payload)
	task, known := d.opts.Registry.Get(rec.IssueID)
	taskView := rules.TaskView{Known: known}
	if known {
		taskView.Plan = task.Plan
		taskView.Turns = task.Turns
		taskView.LastReply = task.LastReply
	}

	decision := d.opts.Rules.Decide(delivery, taskView)
	if decision.Action == rules.ActionIgnore {
		d.ignored.Add(1)
		d.log.Info("delivery not dispatched",
			"issue", rec.IssueID, "event", rec.Event, "reason", decision.Reason)
		return
	}
	if d.opts.MaxCostPerTask > 0 && known && task.Cost >= d.opts.MaxCostPerTask {
		d.ignored.Add(1)
		d.log.Warn("task exceeded its cost budget; a human must take over",
			"issue", rec.IssueID, "cost", task.Cost, "budget", d.opts.MaxCostPerTask)
		return
	}

	match, ok := d.opts.Projects.Match(rec.ProjectKey, rec.IssueID)
	if !ok {
		// Fail closed. Guessing a repository is the one mistake this design
		// refuses to make.
		d.ignored.Add(1)
		d.log.Warn("no repository is mapped for this project; delivery ignored",
			"issue", rec.IssueID, "project", rec.ProjectKey)
		return
	}

	task, err = d.ensureTask(ctx, rec, match.Entry, task, known)
	if err != nil {
		d.log.Error("cannot prepare the task", "issue", rec.IssueID, "error", err)
		return
	}

	d.runTurn(ctx, rec, delivery, task, match, decision)
}

// ensureTask creates the registry row and the worktree if they do not exist yet,
// and refuses to continue when the mapping changed under a running task.
func (d *Dispatcher) ensureTask(ctx context.Context, rec *store.Record, entry *projectmap.Entry, task registry.Task, known bool) (registry.Task, error) {
	if known && task.Repo != "" && task.Repo != entry.Repo.Path {
		return registry.Task{}, fmt.Errorf("task %s is bound to %s but the mapping now points at %s; refusing to reuse the session",
			rec.IssueID, task.Repo, entry.Repo.Path)
	}

	if !known {
		created, _, err := d.opts.Registry.Ensure(rec.IssueID, func(t *registry.Task) {
			t.Repo = entry.Repo.Path
			t.Agent = d.opts.Agent
			t.State = registry.StateAnalyzing
			t.Plan = registry.PlanNone
		})
		if err != nil {
			return registry.Task{}, err
		}
		task = created
	}

	if task.Worktree != "" {
		return task, nil
	}
	if entry.Repo.DefaultBranch == "" {
		return registry.Task{}, fmt.Errorf("project %s has no default_branch configured, so no worktree can be based on it", entry.YouTrackKey)
	}
	base := strings.TrimSpace(entry.Worktrees)
	if base == "" {
		base = strings.TrimSpace(d.opts.WorktreeBase)
	}
	if base == "" {
		return registry.Task{}, fmt.Errorf("project %s declares no worktrees directory and no fallback is configured", entry.YouTrackKey)
	}
	manager, err := d.managerFor(base)
	if err != nil {
		return registry.Task{}, err
	}
	prepared, err := manager.Prepare(ctx, worktree.Request{
		Repo:          entry.Repo.Path,
		TaskKey:       rec.IssueID,
		DefaultBranch: entry.Repo.DefaultBranch,
	})
	if err != nil {
		return registry.Task{}, err
	}
	return d.opts.Registry.Update(rec.IssueID, func(t *registry.Task) {
		t.Worktree = prepared.Path
		t.Repo = prepared.Repo
	})
}

// managerFor returns the worktree manager for a base directory, creating it on
// first use. The base comes from the routing entry, because it has to sit outside
// the repository that entry points at.
func (d *Dispatcher) managerFor(base string) (*worktree.Manager, error) {
	if manager, ok := d.managers[base]; ok {
		return manager, nil
	}
	manager, err := worktree.New(worktree.Options{Base: base})
	if err != nil {
		return nil, err
	}
	d.managers[base] = manager
	return manager, nil
}

// runTurn delivers one prompt and records what came back.
func (d *Dispatcher) runTurn(ctx context.Context, rec *store.Record, delivery rules.Delivery, task registry.Task, match projectmap.Match, decision rules.Decision) {
	phase := opencode.PhaseAnalysis
	if decision.Action == rules.ActionExecute {
		phase = opencode.PhaseExecution
	}

	prompt := d.opts.Rules.Prompt(decision.Action, delivery, rules.PromptContext{
		Worktree:       task.Worktree,
		AttachmentsDir: d.opts.AttachmentsDir,
		Repository:     match.Entry.Repo.Path,
		Author:         rec.PrimaryActor,
	})
	if prompt == "" {
		d.log.Error("no prompt was built for the action", "action", decision.Action, "issue", rec.IssueID)
		return
	}

	arbiter := opencode.NewAnalysisArbiter()
	if phase == opencode.PhaseExecution {
		arbiter = opencode.NewExecutionArbiter()
	}

	// The ruleset is the first permission layer and its ORDER is load bearing:
	// the session ruleset is an array evaluated last-match-wins, so the catch-all
	// has to come first and the specific entries after it.
	ruleset := sessionRuleset()

	runner := opencode.NewRunner(d.opts.Client, arbiter, d.log)
	result, err := runner.Run(ctx, opencode.Task{
		Directory: task.Worktree,
		Prompt:    prompt,
		Agent:     d.opts.Agent,
		Title:     rec.IssueID,
		SessionID: task.SessionID,
		Ruleset:   ruleset,
		Metadata:  map[string]any{"task_key": rec.IssueID, "action": string(decision.Action)},
		Deadline:  d.opts.Deadline,
	})

	// Record the turn even when it failed: the audit is the only place the
	// operator can see what happened.
	replied := false
	for _, call := range result.Tools {
		if call.Name == "youtrack_add_issue_comment" && call.Status == "completed" {
			replied = true
		}
	}
	attrs := []any{
		"issue", rec.IssueID, "action", decision.Action, "phase", phase,
		"session", result.SessionID, "finished", result.Finished, "timed_out", result.TimedOut,
		"replied", replied, "cost", result.Cost, "tokens", result.Tokens.Total,
		"elapsed", result.Elapsed.Round(time.Millisecond), "permissions", len(result.Permissions),
	}
	if err != nil {
		d.log.Error("turn failed", append(attrs, "error", err)...)
	} else if !result.Finished {
		d.log.Warn("turn did not finish", attrs...)
	} else {
		d.log.Info("turn finished", attrs...)
	}
	for _, answered := range result.Permissions {
		d.log.Info("permission decision",
			"issue", rec.IssueID, "permission", answered.Permission,
			"command", answered.Command, "reply", answered.Reply, "reason", answered.Reason)
	}
	if result.Finished && !replied {
		d.log.Error("the turn finished without posting a comment; the issue has no reply",
			"issue", rec.IssueID, "session", result.SessionID,
			"hint", "check the youtrack_add_issue_comment call and the MCP server")
	}

	state := registry.StateAwaitingInput
	switch {
	case err != nil || (!result.Finished && !result.TimedOut):
		state = registry.StateFailed
	case result.TimedOut:
		// A timeout is not a failure: the session may still be running.
		state = registry.StateExecuting
	case decision.Action == rules.ActionExecute:
		state = registry.StateDone
	}
	plan := task.Plan
	if decision.Action == rules.ActionAnalyze || decision.Action == rules.ActionPlan {
		// The turn produced analysis or a plan; a human decides what happens next.
		plan = registry.PlanDraft
	}

	if _, err := d.opts.Registry.Update(rec.IssueID, func(t *registry.Task) {
		if result.SessionID != "" {
			t.SessionID = result.SessionID
		}
		if task.Worktree != "" {
			t.Worktree = task.Worktree
		}
		t.Repo = match.Entry.Repo.Path
		t.State = state
		t.Plan = plan
		t.Turns++
		t.Cost += result.Cost
		t.LastReply = result.Text
	}); err != nil {
		d.log.Error("cannot record the turn in the registry", "issue", rec.IssueID, "error", err)
	}
}

// sessionRuleset is the first permission layer.
//
// The catch-all comes first on purpose: the session ruleset is an array evaluated
// last-match-wins, so a trailing catch-all would override every specific entry
// (measured: a trailing "*": ask made an allowed tool ask again). Everything the
// specific entries do not permit still reaches the arbiter as an `ask`.
func sessionRuleset() []opencode.PermissionRule {
	ruleset := []opencode.PermissionRule{
		{Permission: "*", Pattern: "*", Action: "ask"},
		{Permission: "read", Pattern: "*", Action: "allow"},
		{Permission: "glob", Pattern: "*", Action: "allow"},
		{Permission: "grep", Pattern: "*", Action: "allow"},
		{Permission: "list", Pattern: "*", Action: "allow"},
		{Permission: "todowrite", Pattern: "*", Action: "allow"},
		// Gated so the phase can decide: "allow" would bypass the arbiter and
		// "deny" would remove the tool from the analysis turn altogether.
		{Permission: "edit", Pattern: "*", Action: "ask"},
		{Permission: "bash", Pattern: "*", Action: "ask"},
		// Never, in either phase.
		{Permission: "external_directory", Pattern: "*", Action: "deny"},
		{Permission: "webfetch", Pattern: "*", Action: "deny"},
		{Permission: "websearch", Pattern: "*", Action: "deny"},
	}
	for _, tool := range AllowedMCPTools {
		ruleset = append(ruleset, opencode.PermissionRule{Permission: tool, Pattern: "*", Action: "allow"})
	}
	return ruleset
}

// describe turns an audit record plus its payload into the rules' input.
func describe(rec *store.Record, payload *webhook.Payload) rules.Delivery {
	delivery := rules.Delivery{
		Event:       rec.Event,
		IssueID:     rec.IssueID,
		ProjectKey:  rec.ProjectKey,
		Summary:     payload.Summary,
		Description: payload.Description,
		Actor:       rec.PrimaryActor,
		States:      map[string]string{},
	}
	if len(payload.Comments) > 0 {
		// The newest comment is the one that triggered a comment event.
		delivery.CommentText = payload.Comments[len(payload.Comments)-1].Text
	}
	for _, field := range payload.ChangedFields {
		if field.Name == "" {
			continue
		}
		// The value is polymorphic: only the enum shape carries a name.
		var enum struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(field.Value, &enum); err == nil && enum.Name != "" {
			delivery.States[field.Name] = enum.Name
		}
	}
	return delivery
}
