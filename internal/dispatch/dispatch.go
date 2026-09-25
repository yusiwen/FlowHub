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
	"sync"
	"sync/atomic"
	"time"

	"github.com/yusiwen/flowhub/internal/opencode"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/runtimes"
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
	// Runtimes is the enrolled runtime inventory. When it is nil or empty the
	// dispatcher falls back to Client, which is the environment-configured runtime
	// named "default".
	Runtimes *runtimes.Inventory
	// WorktreeBase is the fallback directory for task worktrees, used when a
	// routing entry does not declare its own. Entries normally declare one,
	// because the base has to be outside the repository it belongs to.
	WorktreeBase string
	// Agent is the opencode agent name; empty uses the server default.
	Agent string
	// Deadline bounds one turn.
	Deadline time.Duration
	// FirstResponse bounds how long a turn may take to produce its first assistant
	// message before it is failed. Zero leaves the runner's own default.
	FirstResponse time.Duration
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

	// managers caches one worktree manager per base directory, and clients one
	// opencode client per runtime. One worker per runtime touches them, so they are
	// guarded rather than worker-local.
	cacheMu  sync.Mutex
	managers map[string]*worktree.Manager
	clients  map[string]*opencode.Client

	// prepMu guards prepLocks, which serializes workspace preparation per repository:
	// `git worktree add` is not safe to run twice at once in one clone, and two
	// runtimes may well share one (a second name for the same host, or a
	// misconfiguration that must not corrupt the clone).
	prepMu    sync.Mutex
	prepLocks map[string]*sync.Mutex

	queue   chan *store.Record
	queued  atomic.Int64
	handled atomic.Int64
	dropped atomic.Int64
	ignored atomic.Int64

	// queueMu guards the per-runtime queues and their counters. The intake loop
	// creates a queue (and starts its worker) the first time a runtime is used, and
	// /healthz reads the counters from an HTTP handler.
	queueMu   sync.Mutex
	queues    map[string]chan routed
	workers   map[string]bool
	busyPerRT map[string]int
	lastError map[string]string

	// loadMu guards inFlight, which the selection policy reads from the worker
	// goroutine and /healthz reads from an HTTP handler.
	loadMu   sync.Mutex
	inFlight map[string]int
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
		opts:      opts,
		log:       opts.Log,
		prepLocks: map[string]*sync.Mutex{},
		managers:  map[string]*worktree.Manager{},
		clients:   map[string]*opencode.Client{},
		inFlight:  map[string]int{},
		queue:     make(chan *store.Record, opts.QueueSize),
		queues:    map[string]chan routed{},
		workers:   map[string]bool{},
		busyPerRT: map[string]int{},
		lastError: map[string]string{},
	}, nil
}

// Dispatch queues a delivery. It never blocks: the webhook request path is
// synchronous on the publisher's side, so a full queue drops work instead.
//
// A nil receiver does nothing. This is not decoration: a typed nil
// (*Dispatcher)(nil) held in the webhook's Dispatcher interface is not == nil,
// so the receiving path can reach this method with no dispatcher behind it, and
// panicking there would cost the publisher its response.
func (d *Dispatcher) Dispatch(rec *store.Record) {
	if d == nil || rec == nil || !rec.Accepted {
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

// routed is one delivery on its way to a runtime: the intake loop decided *which*
// runtime, and that runtime's worker runs the turn.
type routed struct {
	record  *store.Record
	binding runtimeBinding
}

// Run routes deliveries to their runtime's worker until the context is cancelled.
//
// The unit of serialization is the runtime, not the process. The reason one worker
// was needed at all — a prompt sent to a busy session is silently swallowed
// (opencode issue #46842) — is a property of a *session*, and a session belongs to
// one runtime for life. So each runtime gets its own queue and its own single
// worker, and different runtimes run turns in parallel. `max_concurrent` stays 1
// until per-task locking exists; the queue is where raising it would go.
//
// The intake loop itself only decides the runtime (and probes it, so it is off the
// webhook path but still serialized); it never runs a turn.
func (d *Dispatcher) Run(ctx context.Context) {
	d.log.Info("dispatcher started",
		"queue_size", d.opts.QueueSize, "deadline", d.opts.Deadline,
		"agent", d.opts.Agent, "analyze_on_create", !d.opts.Rules.SkipAnalyzeOnCreate,
		"trigger", d.opts.Rules.Trigger)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			d.log.Info("dispatcher stopped", "queued", d.queued.Load(), "handled", d.handled.Load())
			return
		case rec := <-d.queue:
			d.queued.Add(-1)
			item, ok := d.routeOne(ctx, rec)
			if !ok {
				continue
			}
			d.enqueue(ctx, item, &workers)
		}
	}
}

// handle runs one delivery end to end on the calling goroutine.
//
// Production goes through Run — intake, then the runtime's own queue and worker —
// while this is the same two steps without a scheduler in between, which is what
// the pipeline tests want. Both call exactly the same code.
func (d *Dispatcher) handle(ctx context.Context, rec *store.Record) {
	item, ok := d.routeOne(ctx, rec)
	if !ok {
		return
	}
	d.runRouted(ctx, item)
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
		"in_flight":   d.inFlightSnapshot(),
		"runtimes":    d.runtimeSnapshot(),
	}
}

// runtimeSnapshot is the per-runtime view: how much work is waiting for it, how
// many turns it is running, and the last thing that went wrong on it. One HTTP call
// answers "which host is stuck" instead of a log dig.
func (d *Dispatcher) runtimeSnapshot() map[string]map[string]any {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	out := make(map[string]map[string]any, len(d.queues))
	for name, queue := range d.queues {
		// Read the counter directly: busyFor would take this same lock again, and a
		// mutex is not reentrant.
		entry := map[string]any{
			"queued":    len(queue),
			"busy":      d.busyPerRT[name],
			"in_flight": d.inFlightFor(name),
		}
		if message, ok := d.lastError[name]; ok {
			entry["last_error"] = message
		}
		out[name] = entry
	}
	return out
}

// enterTurn records that a turn is running on a runtime and returns the function
// that clears it. The spread policy ranks by these counters, so with one worker
// per runtime (ADR 0001 step 5) they say which host is busy right now.
func (d *Dispatcher) enterTurn(runtime string) func() {
	if runtime == "" {
		return func() {}
	}
	d.loadMu.Lock()
	d.inFlight[runtime]++
	d.loadMu.Unlock()
	return func() {
		d.loadMu.Lock()
		d.inFlight[runtime]--
		if d.inFlight[runtime] <= 0 {
			delete(d.inFlight, runtime)
		}
		d.loadMu.Unlock()
	}
}

// inFlightFor reports how many turns are running on a runtime right now.
func (d *Dispatcher) inFlightFor(runtime string) int {
	d.loadMu.Lock()
	defer d.loadMu.Unlock()
	return d.inFlight[runtime]
}

// inFlightSnapshot copies the counters for /healthz.
func (d *Dispatcher) inFlightSnapshot() map[string]int {
	d.loadMu.Lock()
	defer d.loadMu.Unlock()
	out := make(map[string]int, len(d.inFlight))
	for name, count := range d.inFlight {
		out[name] = count
	}
	return out
}

// routeOne decides which runtime takes a delivery, and refuses it when none can.
//
// It is deliberately thin: the *rules* decision is made by the worker, against the
// registry as it is when the turn actually starts. That ordering is what keeps a
// second delivery for a task queueing behind the first turn instead of racing it
// with a stale view of the task.
func (d *Dispatcher) routeOne(ctx context.Context, rec *store.Record) (routed, bool) {
	if d.Paused() {
		d.ignored.Add(1)
		d.log.Warn("dispatch is paused; delivery ignored",
			"pause_file", d.opts.PauseFile, "issue", rec.IssueID, "event", rec.Event)
		return routed{}, false
	}

	task, _ := d.opts.Registry.Get(rec.IssueID)
	if name := strings.TrimSpace(task.Runtime); name != "" {
		binding, ok := d.bindingFor(name)
		if !ok {
			// The binding is absolute: a task whose host is gone is refused, not
			// re-homed, because a new session elsewhere loses the plan.
			d.ignored.Add(1)
			d.log.Warn("no runtime can serve this delivery; it is ignored",
				"issue", rec.IssueID, "project", rec.ProjectKey,
				"error", fmt.Sprintf("task is bound to runtime %q, which is no longer configured", name))
			return routed{}, false
		}
		return routed{record: rec, binding: binding}, true
	}

	match, ok := d.opts.Projects.Match(rec.ProjectKey, rec.IssueID)
	if !ok {
		// Fail closed, and fail here rather than in the worker: an unroutable project
		// must not take up a runtime's queue slot.
		d.ignored.Add(1)
		d.log.Warn("no repository is mapped for this project; delivery ignored",
			"issue", rec.IssueID, "project", rec.ProjectKey)
		return routed{}, false
	}
	binding, err := d.pickRuntime(ctx, task, match.Entry)
	if err != nil {
		d.ignored.Add(1)
		d.log.Warn("no runtime can serve this delivery; it is ignored",
			"issue", rec.IssueID, "project", rec.ProjectKey, "error", err)
		return routed{}, false
	}
	return routed{record: rec, binding: binding}, true
}

// queueFor returns a runtime's queue, creating it the first time the runtime is
// used. The second result says whether it was created, which is the caller's signal
// to start that runtime's single worker.
func (d *Dispatcher) queueFor(name string) (chan routed, bool) {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	if queue, ok := d.queues[name]; ok {
		return queue, false
	}
	queue := make(chan routed, d.opts.QueueSize)
	d.queues[name] = queue
	return queue, true
}

// enqueue hands a delivery to its runtime's worker, starting that worker on first
// use. A full per-runtime queue drops the delivery, exactly like the intake queue:
// blocking here would push back into the webhook request path.
//
// Workers are never stopped except by the context. An idle goroutine per known
// runtime costs nothing next to the bookkeeping of stopping one safely, and a
// runtime that is revoked and re-enrolled keeps its queue rather than losing work
// that was already routed to it.
func (d *Dispatcher) enqueue(ctx context.Context, item routed, workers *sync.WaitGroup) {
	name := item.binding.Name
	queue, created := d.queueFor(name)
	if created {
		d.log.Info("runtime worker started", "runtime", name, "queue_size", d.opts.QueueSize)
		workers.Add(1)
		go func() {
			defer workers.Done()
			d.workRuntime(ctx, name, queue)
		}()
	}
	// The load is counted from the moment the delivery is committed to this runtime,
	// not from the moment its worker picks it up. The difference matters under a
	// burst: two deliveries that arrive together are routed back to back, and by the
	// time the second is ranked the first may already have been dequeued — with its
	// turn still preparing, so neither "queued" nor "in flight" would show it. That
	// is how a spread of two tasks landed on one host.
	d.countBusy(name, 1)
	select {
	case queue <- item:
	default:
		d.countBusy(name, -1)
		d.dropped.Add(1)
		d.log.Error("the runtime's queue is full, dropping delivery",
			"runtime", name, "issue", item.record.IssueID, "queue_size", d.opts.QueueSize)
	}
}

// workRuntime is one runtime's single worker: it runs the turns routed to that
// runtime, one at a time, which is the granularity that matters because a session
// belongs to one runtime for life.
func (d *Dispatcher) workRuntime(ctx context.Context, name string, queue chan routed) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-queue:
			d.runRouted(ctx, item)
			d.countBusy(name, -1)
			d.handled.Add(1)
		}
	}
}

// busyFor reports how much work is committed to a runtime right now: deliveries
// waiting in its queue, being prepared, or running a turn. The spread policy reads
// it, so two deliveries that arrive together do not both land on the host that
// happens to sort first.
func (d *Dispatcher) busyFor(name string) int {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	return d.busyPerRT[name]
}

func (d *Dispatcher) countBusy(name string, delta int) {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	d.busyPerRT[name] += delta
	if d.busyPerRT[name] <= 0 {
		delete(d.busyPerRT, name)
	}
}

// setRuntimeError and clearRuntimeError keep the per-runtime view /healthz reports,
// so "which host is stuck" is one call rather than a log dig.
func (d *Dispatcher) setRuntimeError(name, message string) {
	if name == "" {
		return
	}
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	d.lastError[name] = message
}

func (d *Dispatcher) clearRuntimeError(name string) {
	if name == "" {
		return
	}
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	delete(d.lastError, name)
}

// runRouted runs the turn for a delivery the router has already assigned. It reads
// the task afresh: the routing decision may be seconds old, and the rules depend on
// what previous turns recorded.
func (d *Dispatcher) runRouted(ctx context.Context, item routed) {
	rec, binding := item.record, item.binding
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
	if !authorAllowed(match.Entry.Authors, rec.PrimaryActor) {
		// A per-project allowlist is a restriction, so an actor it does not name
		// gets no work — including the no-actor events (issueDeleted carries no
		// actor), which can therefore never pass this gate.
		d.ignored.Add(1)
		d.log.Warn("actor is not on the project's author allowlist; delivery ignored",
			"issue", rec.IssueID, "project", rec.ProjectKey, "actor", rec.PrimaryActor,
			"authors", strings.Join(match.Entry.Authors, ","))
		return
	}

	// The registry is authoritative about the binding: the router chose this runtime
	// from the same record, but a task may never move between runtimes, so it is
	// checked here too rather than trusted.
	if name := strings.TrimSpace(task.Runtime); name != "" && name != binding.Name {
		d.ignored.Add(1)
		d.log.Error("refusing to move a bound task to another runtime",
			"issue", rec.IssueID, "bound", name, "chosen", binding.Name)
		return
	}

	task, err = d.ensureTask(ctx, rec, match.Entry, task, known, binding)
	if err != nil {
		d.log.Error("cannot prepare the task", "issue", rec.IssueID, "error", err)
		return
	}

	d.runTurn(ctx, rec, delivery, task, match, decision, binding)
}

// ensureTask creates the registry row and the worktree if they do not exist yet,
// and refuses to continue when the mapping changed under a running task.
func (d *Dispatcher) ensureTask(ctx context.Context, rec *store.Record, entry *projectmap.Entry, task registry.Task, known bool, binding runtimeBinding) (registry.Task, error) {
	if known && task.Repo != "" && task.Repo != entry.Repo.Path {
		return registry.Task{}, fmt.Errorf("task %s is bound to %s but the mapping now points at %s; refusing to reuse the session",
			rec.IssueID, task.Repo, entry.Repo.Path)
	}

	if !known {
		created, _, err := d.opts.Registry.Ensure(rec.IssueID, func(t *registry.Task) {
			t.Repo = entry.Repo.Path
			t.Runtime = binding.Name
			t.Agent = runtimeAgent(entry, binding, d.opts.Agent)
			t.State = registry.StateAnalyzing
			t.Plan = registry.PlanNone
		})
		if err != nil {
			return registry.Task{}, err
		}
		task = created
	}

	// An existing row written before runtimes were recorded still gets its
	// binding, so the removal check has something to compare.
	if task.Runtime == "" {
		if task, err := d.opts.Registry.Update(rec.IssueID, func(t *registry.Task) {
			t.Runtime = binding.Name
		}); err == nil {
			task.Runtime = binding.Name
		}
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

	// The baseline is resolved once, when the task is created, and never again: a
	// long-running task's patches stay reviewable against a fixed commit even if the
	// origin moves. Resolving it through the origin is what makes two hosts with
	// clones fetched at different times agree on where the task starts.
	baseCommit := strings.TrimSpace(task.BaseCommit)
	if baseCommit == "" {
		resolved, err := manager.Resolve(ctx, entry.Repo.Path, entry.Repo.DefaultBranch)
		if err != nil {
			return registry.Task{}, err
		}
		baseCommit = resolved.Commit
		d.log.Info("task baseline resolved",
			"issue", rec.IssueID, "commit", shortCommit(resolved.Commit), "ref", resolved.Ref,
			"source", resolved.Source, "repo", entry.Repo.Path)
		if resolved.Source == "local" {
			// Said out loud, because "local" is not a shared truth: it is the answer
			// on this host only, and a second host would resolve its own.
			d.log.Warn("the repository has no origin remote, so the baseline is this clone's local ref; two hosts would not agree on it",
				"issue", rec.IssueID, "repo", entry.Repo.Path)
		}
	}

	lock := d.prepareLock(entry.Repo.Path)
	lock.Lock()
	prepared, err := manager.Prepare(ctx, worktree.Request{
		Repo:          entry.Repo.Path,
		TaskKey:       rec.IssueID,
		DefaultBranch: entry.Repo.DefaultBranch,
		BaseCommit:    baseCommit,
	})
	lock.Unlock()
	if err != nil {
		return registry.Task{}, err
	}
	return d.opts.Registry.Update(rec.IssueID, func(t *registry.Task) {
		t.Worktree = prepared.Path
		t.Repo = prepared.Repo
		if t.BaseCommit == "" {
			t.BaseCommit = baseCommit
		}
	})
}

// runtimeAgent resolves which agent *profile* runs a turn: the routing entry may
// name one, then the runtime's enrolled profile, then the process-wide default.
//
// The runtime's product name is deliberately not consulted. On an opencode host,
// asking for the agent "opencode" makes the server fall back to its own default
// agent, which drops the step budget and the permission block the installed
// profile carries — a silent widening of what an unattended turn may do.
func runtimeAgent(entry *projectmap.Entry, binding runtimeBinding, fallback string) string {
	if entry != nil {
		if agent := strings.TrimSpace(entry.Agent); agent != "" {
			return agent
		}
	}
	if profile := strings.TrimSpace(binding.AgentProfile); profile != "" {
		return profile
	}
	return fallback
}

// authorAllowed applies a routing entry's author allowlist. An empty list allows
// everyone the entry matches; a non-empty list allows only the logins it names.
func authorAllowed(allowed []string, actor string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, want := range allowed {
		if strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(actor)) {
			return true
		}
	}
	return false
}

// Problems lists routing entries that cannot be dispatched to. It is checked at
// startup because a misconfiguration should stop the process, not fail one issue
// at a time after the operator believes the hub is running.
func Problems(projects *projectmap.Map, fallbackWorktreeBase string) []string {
	if projects == nil {
		return nil
	}
	var problems []string
	for _, entry := range projects.Entries() {
		if !entry.IsEnabled() {
			// A disabled entry is never matched and is not validated, so it may
			// legitimately point at a checkout this host does not have.
			continue
		}
		label := entry.YouTrackKey
		if entry.Repo.DefaultBranch == "" {
			problems = append(problems, fmt.Sprintf("%s: repo.default_branch is empty, so no task worktree can be based on it", label))
		}
		if strings.TrimSpace(entry.Worktrees) == "" && strings.TrimSpace(fallbackWorktreeBase) == "" {
			problems = append(problems, fmt.Sprintf("%s: neither the entry's worktrees directory nor FLOWHUB_WORKTREE_BASE is set, so a task worktree has nowhere to live", label))
		}
	}
	return problems
}

// managerFor returns the worktree manager for a base directory, creating it on
// first use. The base comes from the routing entry, because it has to sit outside
// the repository that entry points at.
func (d *Dispatcher) managerFor(base string) (*worktree.Manager, error) {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
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

// prepareLock returns the mutex that serializes workspace preparation for one
// repository. Preparation is the one step that touches a shared resource — the
// clone's own git bookkeeping — so it is the one step that cannot run twice at once
// in the same repository, whatever the runtime. The turns themselves stay parallel.
func (d *Dispatcher) prepareLock(repo string) *sync.Mutex {
	d.prepMu.Lock()
	defer d.prepMu.Unlock()
	if lock, ok := d.prepLocks[repo]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	d.prepLocks[repo] = lock
	return lock
}

// runTurn delivers one prompt and records what came back.
func (d *Dispatcher) runTurn(ctx context.Context, rec *store.Record, delivery rules.Delivery, task registry.Task, match projectmap.Match, decision rules.Decision, binding runtimeBinding) {
	phase := opencode.PhaseAnalysis
	if decision.Action == rules.ActionExecute {
		phase = opencode.PhaseExecution
	}

	prompt := d.opts.Rules.Prompt(decision.Action, delivery, rules.PromptContext{
		Worktree:       task.Worktree,
		AttachmentsDir: d.opts.AttachmentsDir,
		Repository:     match.Entry.Repo.Path,
		Author:         rec.PrimaryActor,
		// The sign-off states what the agent was reacting to; it comes from the
		// decision, so the reply cannot invent a reason for itself.
		Basis: d.opts.Rules.Basis(decision, delivery),
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

	// The routing entry owns the agent when it names one: which agent runs is a
	// permission decision (that agent's own rules and tools), so it belongs next
	// to the repository, not only in the environment.
	agent := runtimeAgent(match.Entry, binding, d.opts.Agent)

	// The model is pinned for the same reason the agent is. The enrolment check
	// verified the model the installed profile pins, but the agent server resolves
	// an agent name against a list it cached when it started: a server running since
	// before the profile was installed answers with the old model id, so without
	// pinning, the check passes and every turn dies on a model its provider dropped.
	// That happened on 2026-09-25. The routing entry still wins when it names one.
	model := strings.TrimSpace(match.Entry.Model)
	if model == "" {
		model = binding.modelFor(agent)
	}

	defer d.enterTurn(binding.Name)()

	runner := opencode.NewRunner(binding.Client, arbiter, d.log)
	runner.FirstResponse = d.opts.FirstResponse
	result, err := runner.Run(ctx, opencode.Task{
		Directory: task.Worktree,
		Prompt:    prompt,
		Agent:     agent,
		Model:     model,
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
		"issue", rec.IssueID, "action", decision.Action, "phase", phase, "runtime", binding.Name,
		"session", result.SessionID, "finished", result.Finished, "timed_out", result.TimedOut,
		"replied", replied, "cost", result.Cost, "tokens", result.Tokens.Total,
		"elapsed", result.Elapsed.Round(time.Millisecond), "permissions", len(result.Permissions),
	}
	switch {
	case err != nil:
		d.log.Error("turn failed", append(attrs, "error", err)...)
		d.setRuntimeError(binding.Name, err.Error())
	case !result.Finished:
		d.log.Warn("turn did not finish", attrs...)
		d.setRuntimeError(binding.Name, "the last turn did not finish")
	default:
		d.log.Info("turn finished", attrs...)
		d.clearRuntimeError(binding.Name)
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
		// Only a turn that produced text replaces the recorded reply. A turn that
		// ends without a final message — a tool-only loop, a crash — must not erase
		// the last thing the agent said: that text is what recognises our own comment
		// coming back as a webhook when the marker is lost.
		if strings.TrimSpace(result.Text) != "" {
			t.LastReply = result.Text
		}
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

// shortCommit renders a commit id for a log line: long enough to be unambiguous in
// a small repository, short enough that the interesting fields stay visible.
func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}
