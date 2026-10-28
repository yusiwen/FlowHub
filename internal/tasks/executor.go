package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/runner"
	"github.com/yusiwen/flowhub/internal/source"
	"github.com/yusiwen/flowhub/internal/store"
	"github.com/yusiwen/flowhub/internal/workspace"
)

// Options configures the extracted task service.
type Options struct {
	Registry     *registry.Registry
	Source       source.Source
	Policy       rules.Policy
	Log          *slog.Logger
	WorktreeBase string
	NewWorkspace func(base string) (workspace.Workspace, error)
	Capacity     *runner.Capacity
	// RuntimeError and RuntimeOK let dispatch keep its per-runtime health view
	// without the task service importing dispatch.
	RuntimeError func(name, message string)
	RuntimeOK    func(name string)
}

// Executor owns the task lifecycle: ensure, one turn, grant, revoke.
type Executor struct {
	opts       Options
	log        *slog.Logger
	workspaces *runner.Workspaces
	capacity   *runner.Capacity

	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex
}

func New(opts Options) (*Executor, error) {
	if opts.Registry == nil {
		return nil, errors.New("tasks: a registry is required")
	}
	if opts.Source == nil {
		return nil, errors.New("tasks: a source is required")
	}
	if opts.NewWorkspace == nil {
		return nil, errors.New("tasks: a workspace provider factory is required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Policy.Trigger == "" {
		opts.Policy = opts.Source.Policy().Defaults()
	}
	if opts.Capacity == nil {
		opts.Capacity = runner.NewCapacity()
	}
	return &Executor{
		opts:       opts,
		log:        opts.Log,
		workspaces: runner.NewWorkspaces(opts.NewWorkspace),
		capacity:   opts.Capacity,
		taskLocks:  map[string]*sync.Mutex{},
	}, nil
}

func (e *Executor) taskLock(sourceName, key string) *sync.Mutex {
	id := sourceName + "\x00" + key
	e.taskMu.Lock()
	defer e.taskMu.Unlock()
	if lock, ok := e.taskLocks[id]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	e.taskLocks[id] = lock
	return lock
}

func (e *Executor) enterTurn(ctx context.Context, runtime string) func() {
	release, err := e.capacity.Acquire(ctx, runtime)
	if err != nil {
		return func() {}
	}
	return release
}

func (e *Executor) setRuntimeError(name, message string) {
	if e.opts.RuntimeError != nil {
		e.opts.RuntimeError(name, message)
	}
}

func (e *Executor) clearRuntimeError(name string) {
	if e.opts.RuntimeOK != nil {
		e.opts.RuntimeOK(name)
	}
}

func (e *Executor) Ensure(ctx context.Context, request EnsureRequest) (registry.Task, error) {
	task, known := e.opts.Registry.Get(e.opts.Source.Name(), request.Record.IssueID)
	task, err := e.ensureTask(ctx, request.Record, request.Entry, task, known, request.Host, request.Host.Agent)
	if err != nil {
		e.failPreparation(request.Record, request.Host, err)
		return registry.Task{}, err
	}
	return task, nil
}

func (e *Executor) Turn(ctx context.Context, request TurnRequest) (TurnResult, error) {
	action := request.Action
	if string(action) == "last" {
		action = lastAction(request.Task)
	}
	switch action {
	case rules.ActionAnalyze, rules.ActionPlan, rules.ActionExecute:
	default:
		return TurnResult{}, fmt.Errorf("task %s has no action to continue (recorded %q)", request.Record.IssueID, request.Task.LastAction)
	}
	trigger := request.Trigger
	if trigger == "" {
		trigger = rules.Trigger("flow")
	}
	decision := rules.Decision{
		Action:  action,
		Trigger: trigger,
		Reason:  request.Basis,
	}
	result, err := e.runTurn(ctx, request.Record, request.Delivery, request.Task,
		projectmap.Match{Entry: request.Entry}, decision, request.Host)
	if updated, ok := e.opts.Registry.Get(e.opts.Source.Name(), request.Record.IssueID); ok {
		result.Task = updated
	}
	return result, err
}

func (e *Executor) Grant(_ context.Context, request GrantRequest) (GrantResult, error) {
	task := request.Task
	if len(request.Commands) > 0 {
		allowed := map[string]bool{}
		for _, refusal := range task.Refusals {
			allowed[strings.TrimSpace(refusal.Command)] = true
		}
		granted := grantedCommands(task)
		known := map[string]bool{}
		for _, command := range granted {
			known[command] = true
		}
		for _, command := range request.Commands {
			command = strings.TrimSpace(command)
			if !allowed[command] {
				return GrantResult{}, fmt.Errorf("command %q was not refused by the most recent turn", command)
			}
			if !known[command] {
				known[command] = true
				granted = append(granted, command)
			}
		}
		updated, err := e.opts.Registry.Update(e.opts.Source.Name(), request.Record.IssueID, func(t *registry.Task) {
			t.Grant = &registry.Grant{Commands: granted, By: request.Record.PrimaryActor, At: time.Now().UTC()}
		})
		if err != nil {
			return GrantResult{}, err
		}
		return GrantResult{Task: updated, Action: lastAction(updated)}, nil
	}
	action, updated := e.grantPermit(request.Record, task, rules.Decision{
		Trigger: rules.Trigger(request.Trigger), Reason: "flow permit",
	})
	return GrantResult{Task: updated, Action: action.Action}, nil
}

func (e *Executor) Revoke(_ context.Context, request RevokeRequest) error {
	e.revokeGrant(request.Record, request.Task)
	return nil
}

func lastAction(task registry.Task) rules.Action {
	action := rules.Action(strings.TrimSpace(task.LastAction))
	switch action {
	case rules.ActionAnalyze, rules.ActionPlan, rules.ActionExecute:
		return action
	default:
		return rules.ActionAnalyze
	}
}

func (e *Executor) ensureTask(ctx context.Context, rec *store.Record, entry *projectmap.Entry, task registry.Task, known bool, host Host, agentName string) (registry.Task, error) {
	lease := e.taskLock(e.opts.Source.Name(), rec.IssueID)
	lease.Lock()
	defer lease.Unlock()
	if known && task.Repo != "" && task.Repo != entry.Repo.Path {
		return registry.Task{}, fmt.Errorf("task %s is bound to %s but the mapping now points at %s; refusing to reuse the session",
			rec.IssueID, task.Repo, entry.Repo.Path)
	}

	if !known {
		created, _, err := e.opts.Registry.Ensure(e.opts.Source.Name(), rec.IssueID, func(t *registry.Task) {
			t.Repo = entry.Repo.Path
			t.Runtime = host.Name
			t.Agent = agentName
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
		if task, err := e.opts.Registry.Update(e.opts.Source.Name(), rec.IssueID, func(t *registry.Task) {
			t.Runtime = host.Name
		}); err == nil {
			task.Runtime = host.Name
		}
	}
	if task.Worktree != "" {
		return task, nil
	}
	if entry.Repo.DefaultBranch == "" {
		return registry.Task{}, fmt.Errorf("project %s has no default_branch configured, so no worktree can be based on it", entry.Label())
	}
	base := strings.TrimSpace(entry.Worktrees)
	if base == "" {
		base = strings.TrimSpace(e.opts.WorktreeBase)
	}
	if base == "" {
		return registry.Task{}, fmt.Errorf("project %s declares no worktrees directory and no fallback is configured", entry.Label())
	}
	spaces, err := e.workspaces.For(base)
	if err != nil {
		return registry.Task{}, err
	}
	req := workspace.Request{
		TaskKey:       rec.IssueID,
		Repo:          entry.Repo.Path,
		Remote:        entry.Repo.Remote,
		DefaultBranch: entry.Repo.DefaultBranch,
		BaseRef:       entry.Repo.DefaultBranch,
		BaseCommit:    strings.TrimSpace(task.BaseCommit),
	}

	// The baseline is resolved once, when the task is created, and never again: a
	// long-running task's patches stay reviewable against a fixed commit even if the
	// origin moves. Resolving it through the provider is what makes two hosts with
	// clones fetched at different times agree on where the task starts.
	if req.BaseCommit == "" {
		resolved, err := spaces.Resolve(ctx, req)
		if err != nil {
			return registry.Task{}, err
		}
		req.BaseCommit = resolved.Commit
		e.log.Info("task baseline resolved",
			"issue", rec.IssueID, "commit", shortCommit(resolved.Commit), "ref", resolved.Ref,
			"source", resolved.Source, "repo", entry.Repo.Path)
		if resolved.Source == "local" {
			// Said out loud, because "local" is not a shared truth: it is the answer
			// on this host only, and a second host would resolve its own.
			e.log.Warn("the repository has no origin remote, so the baseline is this clone's local ref; two hosts would not agree on it",
				"issue", rec.IssueID, "repo", entry.Repo.Path)
		}
	}

	lock := e.workspaces.Lock(entry.Repo.Path)
	lock.Lock()
	prepared, err := spaces.Prepare(ctx, req)
	lock.Unlock()
	if err != nil {
		return registry.Task{}, err
	}
	return e.opts.Registry.Update(e.opts.Source.Name(), rec.IssueID, func(t *registry.Task) {
		t.Worktree = prepared.Path
		t.Repo = prepared.Repo
		if t.BaseCommit == "" {
			t.BaseCommit = req.BaseCommit
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

func (e *Executor) runTurn(ctx context.Context, rec *store.Record, delivery event.Event, task registry.Task, match projectmap.Match, decision rules.Decision, host Host) (TurnResult, error) {
	lease := e.taskLock(e.opts.Source.Name(), rec.IssueID)
	lease.Lock()
	defer lease.Unlock()
	phase := agent.PhaseAnalysis
	runningState := registry.StateAnalyzing
	switch decision.Action {
	case rules.ActionPlan:
		runningState = registry.StatePlanning
	case rules.ActionExecute:
		phase = agent.PhaseExecution
		runningState = registry.StateExecuting
	}
	// Persist the running state before the prompt. Recovery only inspects
	// analyzing/planning/executing rows, so a SIGKILL during a plan or execute turn
	// used to leave awaiting_input behind and the restart treated it as finished.
	if task.State != runningState {
		if updated, err := e.opts.Registry.Update(e.opts.Source.Name(), rec.IssueID, func(t *registry.Task) {
			t.State = runningState
		}); err != nil {
			e.log.Warn("could not record the running task state", "issue", rec.IssueID, "state", runningState, "error", err)
		} else {
			task = updated
		}
	}

	// The tool policy is data owned by the source: which tools may run unattended,
	// which call counts as the reply, and where a download may land. How any of that
	// becomes a permission request is the runtime's business, and the shell policy
	// stays inside the runtime, because a source must not be able to widen it.
	tools := e.opts.Source.Tools()

	// The context is built once and reused by the follow-up turn: same issue, same
	// worktree, same basis. Only what the prompt *says* differs.
	promptCtx := rules.PromptContext{
		// Whatever a human has authorised for this task travels with every turn it
		// runs, not only the permit turn: the authorisation lasts until the task ends
		// or is revoked, and a later turn that hits the same refusal must not need the
		// human to say it again.
		Permitted: grantedCommands(task),
		Worktree:  task.Worktree,
		// The prompt and the arbiter read one value: telling the agent to write
		// somewhere the runtime would reject is worse than saying nothing.
		AttachmentsDir: tools.Download.Prefix,
		Repository:     match.Entry.Repo.Path,
		Author:         rec.PrimaryActor,
		// The sign-off states what the agent was reacting to; it comes from the
		// decision, so the reply cannot invent a reason for itself.
		Basis: e.opts.Policy.Basis(decision, delivery),
		// The routing entry's own instructions, read from its prompt_file when the
		// configuration was loaded.
		Instructions: match.Entry.PromptExtra,
	}
	prompt := e.opts.Source.Prompt(decision.Action, &delivery, promptCtx)
	if prompt == "" {
		e.log.Error("no prompt was built for the action", "action", decision.Action, "issue", rec.IssueID)
		return TurnResult{Action: decision.Action}, nil
	}

	// The routing entry owns the agent when it names one: which agent runs is a
	// permission decision (that agent's own rules and tools), so it belongs next
	// to the repository, not only in the environment.
	agentName := host.Agent

	// The model is pinned for the same reason the agent is. The enrolment check
	// verified the model the installed profile pins, but the agent server resolves
	// an agent name against a list it cached when it started: a server running since
	// before the profile was installed answers with the old model id, so without
	// pinning, the check passes and every turn dies on a model its provider dropped.
	// That happened on 2026-09-25. The routing entry still wins when it names one.
	// Same order as the agent, for the same reason: the model a host reported for a
	// profile is the one the capability check verified against the provider's
	// catalogue, so the file's pin applies only where there is no attestation.
	model := host.Model

	defer e.enterTurn(ctx, host.Name)()

	// Announce the turn before it starts. Overlap is a property of the log rather than
	// something to infer from two completion lines, and it is the line that shows a
	// runtime is using the breadth it was configured with.
	e.log.Info("turn started",
		"issue", rec.IssueID, "action", decision.Action, "phase", phase,
		"runtime", host.Name, "agent", agentName, "model", model,
		"session", task.SessionID, "max_concurrent", host.MaxConcurrent)

	deadline := host.Deadline
	runnerHost := runner.Host{
		Name:          host.Name,
		Runtime:       host.Runtime,
		Agent:         agentName,
		Model:         model,
		Deadline:      deadline,
		MaxConcurrent: host.MaxConcurrent,
	}
	turn := runner.Turn{
		Directory:    task.Worktree,
		Prompt:       prompt,
		Title:        rec.IssueID,
		SessionID:    task.SessionID,
		Phase:        phase,
		AllowedTools: tools.Allowed,
		ReplyTools:   tools.Reply,
		Downloads: agent.Downloads{
			Hosts:  tools.Download.Hosts,
			Prefix: tools.Download.Prefix,
		},
		// The runtime turns these into an exception to its own allowlist, after its
		// deny list and its phase rule: a human's authorisation can make an
		// unrecognised command runnable, never a forbidden one.
		Granted:  grantedCommands(task),
		Metadata: map[string]any{"task_key": rec.IssueID, "action": string(decision.Action)},
		Deadline: deadline,
	}
	if decision.Action == rules.ActionPlan {
		// A plan must read the live discussion before it posts. Phase 1 enforces
		// this after the turn; Phase 2 puts the same requirement in front of the
		// reply tool, so the model cannot post the stale plan and then correct it.
		turn.RequireReadBeforeReply = append([]string(nil), tools.PlanRead...)
	}

	result, err := runnerHost.Run(ctx, turn)

	// The contract is one comment per turn, and a turn that posts none has told the
	// maintainer nothing at all. Left alone that is invisible from the outside: the
	// issue stays silent while the registry records a reply that does not exist
	// (issue #27), and the next delivery reads that row as "a plan was posted".
	//
	// So the dispatcher asks once more, in the *same* session, with a prompt whose
	// only job is the comment. One ask, not a retry loop: a model that cannot comply
	// twice is failing, and the state below says so.
	replied := anyToolCompleted(result.Tools, tools.Reply)
	// A plan is allowed to be wrong, but it is not allowed to be uninformed. A plan
	// turn that posted its reply without reading the discussion is the failure this
	// gate exists for: the model reuses its session's earlier analysis and asks the
	// maintainer questions the thread already answered. The prompt asks for the read;
	// this check is what makes it a contract.
	planRead := planReadSatisfied(result.Tools, tools.PlanRead, tools.Reply)
	readSeen := anyToolCompleted(result.Tools, tools.PlanRead)
	needReadNudge := decision.Action == rules.ActionPlan && len(tools.PlanRead) > 0 &&
		!planRead && (replied || !readSeen)
	silent := silentFinish(result, err, replied)
	clean := err == nil && result.Finished && !result.TimedOut && result.SessionID != ""
	if silent || (clean && needReadNudge) {
		if silent {
			e.log.Warn("the turn finished without posting a comment; asking once more",
				"issue", rec.IssueID, "session", result.SessionID,
				"reply_tools", strings.Join(tools.Reply, "/"))
			promptCtx.Nudge = true
		}
		if needReadNudge {
			e.log.Warn("the plan turn finished without reading the issue discussion; asking once more",
				"issue", rec.IssueID, "session", result.SessionID,
				"read_tools", strings.Join(tools.PlanRead, "/"))
			promptCtx.NudgeRead = true
		}
		if nudgePrompt := e.opts.Source.Prompt(decision.Action, &delivery, promptCtx); nudgePrompt != "" {
			nudge := turn
			nudge.Prompt = nudgePrompt
			nudge.SessionID = result.SessionID
			// The follow-up may only read and post one comment, and one delivery must
			// not be able to hold a worker for two full turn budgets.
			nudge.Deadline = nudgeDeadline(deadline)
			nudged, nudgeErr := runnerHost.Run(ctx, nudge)
			e.log.Info("follow-up turn finished",
				"issue", rec.IssueID, "session", nudged.SessionID,
				"finished", nudged.Finished, "timed_out", nudged.TimedOut,
				"replied", anyToolCompleted(nudged.Tools, tools.Reply),
				"read", anyToolCompleted(nudged.Tools, tools.PlanRead),
				"cost", nudged.Cost, "tokens", nudged.Tokens.Total,
				"elapsed", nudged.Elapsed.Round(time.Millisecond))
			result = mergeTurns(result, nudged, nudgeErr)
			err = nudgeErr
			replied = anyToolCompleted(result.Tools, tools.Reply)
			planRead = planReadSatisfied(result.Tools, tools.PlanRead, tools.Reply)
		}
	}

	// The read gate is deliberately independent of the runner's error: a plan that
	// was posted without the discussion must not become a draft merely because the
	// follow-up failed afterwards. The task stays without a plan, and the flow run
	// fails, so the next `/opencode start` plans again instead of executing it.
	var planErr error
	if decision.Action == rules.ActionPlan && len(tools.PlanRead) > 0 && replied && !planRead {
		planErr = fmt.Errorf("plan turn posted without reading the issue discussion (%s); the task has no plan until a turn reads it", strings.Join(tools.PlanRead, "/"))
		if err != nil {
			planErr = fmt.Errorf("%w; follow-up: %v", planErr, err)
		}
		// This is a task-contract failure, not a host failure: the runtime answered.
		// It must not flip the runtime's health, which is what stops new work from
		// being scheduled there.
		e.log.Error("plan turn posted without reading the issue discussion",
			"issue", rec.IssueID, "session", result.SessionID,
			"read_tools", strings.Join(tools.PlanRead, "/"))
	}

	// Record the turn even when it failed: the audit is the only place the
	// operator can see what happened.
	attrs := []any{
		"issue", rec.IssueID, "action", decision.Action, "phase", phase, "runtime", host.Name,
		"session", result.SessionID, "finished", result.Finished, "timed_out", result.TimedOut,
		"replied", replied, "cost", result.Cost, "tokens", result.Tokens.Total,
		"elapsed", result.Elapsed.Round(time.Millisecond), "permissions", len(result.Permissions),
	}
	switch {
	case err != nil:
		e.log.Error("turn failed", append(attrs, "error", err)...)
		e.setRuntimeError(host.Name, err.Error())
	case !result.Finished:
		e.log.Warn("turn did not finish", attrs...)
		e.setRuntimeError(host.Name, "the last turn did not finish")
	default:
		e.log.Info("turn finished", attrs...)
		e.clearRuntimeError(host.Name)
	}
	for _, answered := range result.Permissions {
		e.log.Info("permission decision",
			"issue", rec.IssueID, "permission", answered.Permission,
			"command", answered.Command, "reply", answered.Reply, "reason", answered.Reason)
	}
	if result.Finished && !replied {
		e.log.Error("the turn finished without posting a comment; the issue has no reply",
			"issue", rec.IssueID, "session", result.SessionID,
			"hint", "check the "+strings.Join(tools.Reply, "/")+" call and the MCP server")
	}

	// A finished turn that posted nothing asserts nothing. `awaiting_input` means
	// "analysis is posted and a human must answer", and a plan state of `draft` means
	// "a plan exists" — neither is true when the issue has no comment at all. Writing
	// them anyway is how a task whose agent said nothing came to be read, on the next
	// delivery, as "a plan is waiting for approval" and went straight to an execution
	// turn with edit rights (issue #27). Fail instead, so the row matches the issue.
	state := registry.StateAwaitingInput
	switch {
	case err != nil || (!result.Finished && !result.TimedOut):
		state = registry.StateFailed
	case result.TimedOut:
		// A timeout is not a failure: the session may still be running.
		state = registry.StateExecuting
	case !replied:
		state = registry.StateFailed
	case decision.Action == rules.ActionExecute:
		state = registry.StateDone
	}
	if planErr != nil {
		// A plan turn that broke its read contract has produced nothing a human can
		// execute, regardless of what the runner's own state says (a timeout included).
		state = registry.StateFailed
	}
	plan := task.Plan
	if replied && decision.Action == rules.ActionPlan {
		// Only a planning turn creates a plan, and only one that read the discussion
		// first. An analysis turn must leave the plan empty: marking it draft made the
		// next `/opencode start` jump straight to edit rights without a plan (found by
		// the live journey E2E). A plan posted without the read is not a plan either:
		// clearing it is what stops the next start from executing it.
		if planRead {
			plan = registry.PlanDraft
		} else {
			plan = registry.PlanNone
		}
	}
	if decision.Action == rules.ActionPlan && state == registry.StateFailed {
		// A failed plan turn has nothing a human can execute. Clearing a draft from an
		// earlier turn is the difference between "plan again" and "execute the plan
		// this failed turn was meant to replace".
		plan = registry.PlanNone
	}

	outcome := TurnResult{
		SessionID: result.SessionID,
		Text:      result.Text,
		Finished:  result.Finished,
		TimedOut:  result.TimedOut,
		Replied:   replied,
		Cost:      result.Cost,
		Tokens:    result.Tokens,
		Refusals:  refusedCommands(result.Permissions),
		Action:    decision.Action,
	}
	if _, err := e.opts.Registry.Update(e.opts.Source.Name(), rec.IssueID, func(t *registry.Task) {
		if result.SessionID != "" {
			t.SessionID = result.SessionID
		}
		if task.Worktree != "" {
			t.Worktree = task.Worktree
		}
		t.Repo = match.Entry.Repo.Path
		t.State = state
		t.Plan = plan
		// The row carries the reason as well as the state: "failed" with no answer to
		// "failed at what?" sends the operator to a rotating log, and /healthz only keeps
		// each host's most recent error.
		if state == registry.StateFailed {
			recordErr := err
			if planErr != nil {
				recordErr = planErr
			}
			t.LastError = failureText(recordErr, result)
		} else {
			t.LastError = ""
		}
		// The phase a permit continues in, and the commands a bare permit would
		// authorise next: both are facts about the turn that just ran.
		t.LastAction = string(decision.Action)
		t.Refusals = refusedCommands(result.Permissions)
		if state == registry.StateDone {
			// The authorised commands were for the work this task has now finished, so
			// keeping them would silently widen whatever this task does next.
			t.Grant = nil
		}
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
		e.log.Error("cannot record the turn in the registry", "issue", rec.IssueID, "error", err)
	}
	return outcome, planErr
}

// sameTool compares a source's tool name with the name the server reported. On
// v2 the MCP surface spells a name `youtrack.add_issue_comment`, while the source
// contract and the prompt use `youtrack_add_issue_comment`; treating those as two
// tools made a posted comment look like silence (found by the real E2E).
func sameTool(got, want string) bool {
	norm := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.ReplaceAll(value, ".", "_")
		value = strings.ReplaceAll(value, "-", "_")
		return value
	}
	return norm(got) == norm(want)
}

func anyToolCompleted(calls []agent.ToolCall, tools []string) bool {
	for _, call := range calls {
		if call.Status == "completed" && matchesAnyTool(call.Name, tools) {
			return true
		}
	}
	return false
}

// matchesAnyTool reports whether got is one of wants, using the same spelling
// normalisation as the reply check.
func matchesAnyTool(got string, wants []string) bool {
	for _, want := range wants {
		if sameTool(got, want) {
			return true
		}
	}
	return false
}

// planReadSatisfied reports whether this turn may be recorded as a plan. A turn
// with no declared read requirement is satisfied; otherwise a completed read must
// have arrived *before* a completed reply. Reading after posting the plan is too
// late: the plan a human reads was written without it.
func planReadSatisfied(calls []agent.ToolCall, readTools, replyTools []string) bool {
	if len(readTools) == 0 {
		return true
	}
	return readBeforeReply(calls, readTools, replyTools)
}

// readBeforeReply reports whether any completed reply follows a completed read.
// Tool order is the order the runtime reported the calls in; mergeTurns keeps the
// first turn's calls before the follow-up's, which is exactly the order that makes
// a corrected plan count and the original unread one not.
func readBeforeReply(calls []agent.ToolCall, readTools, replyTools []string) bool {
	read := false
	for _, call := range calls {
		if call.Status != "completed" {
			continue
		}
		if matchesAnyTool(call.Name, readTools) {
			read = true
			continue
		}
		if read && matchesAnyTool(call.Name, replyTools) {
			return true
		}
	}
	return false
}

// silentFinish reports whether a turn ended cleanly and posted no reply — the state
// that must never be recorded as an answer (issue #27).
//
// A turn that errored, never finished, or timed out is excluded: those have their own
// states, and re-prompting a session whose prompt the server rejected would only fail
// again. A result with no session id is excluded too, because there is nothing to
// continue.
func silentFinish(result agent.Result, err error, replied bool) bool {
	return err == nil && result.Finished && !result.TimedOut && !replied && result.SessionID != ""
}

// MinNudgeDeadline keeps a short task deadline from making the follow-up turn so
// tight that it cannot post a comment.
const MinNudgeDeadline = 2 * time.Minute

// nudgeDeadline bounds the follow-up turn: a fraction of the ordinary budget, because
// that turn may only post a comment. Without it one delivery could hold a worker for
// two full turn budgets.
//
// The floor is a floor on the *fraction*, not a raise: a deployment whose ordinary
// turn budget is shorter than MinNudgeDeadline keeps its own (shorter) bound, because
// the follow-up must never get more time than a real turn.
func nudgeDeadline(ordinary time.Duration) time.Duration {
	if ordinary <= 0 {
		return MinNudgeDeadline
	}
	bound := ordinary / 3
	if bound < MinNudgeDeadline {
		bound = MinNudgeDeadline
	}
	if bound > ordinary {
		bound = ordinary
	}
	return bound
}

// mergeTurns folds the follow-up turn into the first one, so the audit and the
// registry record one delivery: the costs add up and every permission decision from
// both turns is kept. The second turn decides the outcome — it is the one that either
// posted the comment or did not.
func mergeTurns(first, second agent.Result, err error) agent.Result {
	merged := first
	merged.SessionID = second.SessionID
	merged.Finished = second.Finished
	merged.TimedOut = second.TimedOut
	merged.ErrorText = second.ErrorText
	merged.Elapsed = first.Elapsed + second.Elapsed
	merged.Cost = first.Cost + second.Cost
	merged.Tokens = addTokens(first.Tokens, second.Tokens)
	// A follow-up that produced no text must not erase what the first turn said: that
	// text is what recognises our own comment coming back when the marker is lost.
	if strings.TrimSpace(second.Text) != "" {
		merged.Text = second.Text
	}
	merged.Tools = append(append([]agent.ToolCall(nil), first.Tools...), second.Tools...)
	merged.Permissions = append(append([]agent.PermissionDecision(nil), first.Permissions...), second.Permissions...)
	if err != nil && merged.ErrorText == "" {
		merged.ErrorText = err.Error()
	}
	return merged
}

func addTokens(first, second agent.Tokens) agent.Tokens {
	sum := first
	sum.Input += second.Input
	sum.Output += second.Output
	sum.Reasoning += second.Reasoning
	sum.Total += second.Total
	sum.CacheRead += second.CacheRead
	sum.CacheWrite += second.CacheWrite
	return sum
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

// failPreparation records a task whose worktree could not be prepared.
//
// A row left at `analyzing` asserts that the read-only analysis turn is running, when in
// fact no session exists and no comment will ever be posted: the operator's `jq` over the
// registry, and the state the next delivery reads, both describe work that is not
// happening (issue #34). The state is `failed` rather than terminal because the next
// delivery for the same task retries the preparation — a missing worktree is still
// missing, so `ensureTask` runs again.
func (e *Executor) failPreparation(rec *store.Record, host Host, cause error) {
	e.log.Error("cannot prepare the task",
		"issue", rec.IssueID, "runtime", host.Name, "error", cause,
		"hint", "the task is recorded failed; the next delivery for it retries the preparation")
	// The per-host view names the task, so a repository problem cannot be mistaken for a
	// host that is down.
	e.setRuntimeError(host.Name, fmt.Sprintf("preparing %s: %v", rec.IssueID, cause))
	if _, err := e.opts.Registry.Update(e.opts.Source.Name(), rec.IssueID, func(t *registry.Task) {
		t.State = registry.StateFailed
		t.LastError = cause.Error()
	}); err != nil {
		if !errors.Is(err, registry.ErrTaskNotFound) {
			e.log.Error("cannot record the preparation failure", "issue", rec.IssueID, "error", err)
			return
		}
		// The delivery failed before any row existed — a repository that moved under a
		// bound task, a routing entry with no default branch. There is nothing to attach
		// the failure to; the log line above is the whole record, and saying so is better
		// than an update error nobody can act on.
		e.log.Warn("no task row exists for the delivery that failed preparation, so only the log records it",
			"issue", rec.IssueID)
	}
}

// failureText is what a failed task row records as the reason. The turn's error is the
// most precise source, then the runtime's own last error, then a description of the state
// — a turn that finished having posted nothing is a failure with no error value at all
// (rule 16).

func failureText(err error, result agent.Result) string {
	switch {
	case err != nil:
		return err.Error()
	case strings.TrimSpace(result.ErrorText) != "":
		return result.ErrorText
	case result.Finished:
		return "the turn finished without posting a comment"
	default:
		return "the turn did not finish"
	}
}

// grantedCommands is the human's authorisation for this task, or nil.
func grantedCommands(task registry.Task) []string {
	if task.Grant == nil || len(task.Grant.Commands) == 0 {
		return nil
	}
	return append([]string(nil), task.Grant.Commands...)
}

// refusedCommands extracts the commands a turn was refused for, in order and without
// duplicates. This set is what a bare `/opencode permit` authorises, so it has to be
// exactly what the human read in the agent's comment: no inference, no patterns.
func refusedCommands(decisions []agent.PermissionDecision) []registry.Refusal {
	var refusals []registry.Refusal
	seen := map[string]bool{}
	for _, decision := range decisions {
		command := strings.TrimSpace(decision.Command)
		if decision.Reply != agent.DecisionReject || command == "" || seen[command] {
			continue
		}
		seen[command] = true
		refusals = append(refusals, registry.Refusal{Command: command, Reason: decision.Reason})
	}
	return refusals
}

// grantPermit records the human's authorisation and turns the permit comment into the
// turn that carries on from where the refusals stopped the agent.
//
// The authorised set accumulates: a second permit adds what the newest turn was
// refused for instead of replacing the first, because the authorisation lasts until
// the task ends and dropping an earlier command would refuse the agent something a
// human already approved.
func (e *Executor) grantPermit(rec *store.Record, task registry.Task, decision rules.Decision) (rules.Decision, registry.Task) {
	commands := grantedCommands(task)
	known := make(map[string]bool, len(commands))
	for _, command := range commands {
		known[command] = true
	}
	var added []string
	for _, refusal := range task.Refusals {
		if command := strings.TrimSpace(refusal.Command); command != "" && !known[command] {
			known[command] = true
			commands = append(commands, command)
			added = append(added, command)
		}
	}
	if len(added) == 0 {
		// Every refusal was already authorised. Nothing to record, and the turn still
		// runs: the human asked for the work to continue, which is a legitimate thing
		// to ask for twice.
		e.log.Info("a permit added no new command; every refusal was already authorised",
			"issue", rec.IssueID, "by", rec.PrimaryActor)
	} else {
		e.log.Warn("a human authorised refused commands; the task continues",
			"issue", rec.IssueID, "by", rec.PrimaryActor,
			"added", strings.Join(added, " && "),
			"authorised_total", len(commands))
	}
	if updated, err := e.opts.Registry.Update(e.opts.Source.Name(), rec.IssueID, func(t *registry.Task) {
		t.Grant = &registry.Grant{Commands: commands, By: rec.PrimaryActor, At: time.Now().UTC()}
	}); err != nil {
		e.log.Error("cannot record the authorisation", "issue", rec.IssueID, "error", err)
		task.Grant = &registry.Grant{Commands: commands, By: rec.PrimaryActor, At: time.Now().UTC()}
	} else {
		task = updated
	}

	// Continue in the phase the refusals stopped, which is the only phase that can do
	// the work the human just unblocked. A row written before the action was recorded
	// has nothing to continue, and a read-only turn is the safe guess.
	action := rules.Action(strings.TrimSpace(task.LastAction))
	switch action {
	case rules.ActionAnalyze, rules.ActionPlan, rules.ActionExecute:
	default:
		e.log.Warn("the task has no recorded action to continue; permitting a read-only turn instead",
			"issue", rec.IssueID, "recorded", task.LastAction)
		action = rules.ActionAnalyze
	}
	return rules.Decision{
		Action: action, Trigger: decision.Trigger,
		Reason: decision.Reason + "; continuing the task with the authorised commands",
	}, task
}

// revokeGrant clears the authorisation a human gave, so the next turn is judged by the
// policy alone again.
func (e *Executor) revokeGrant(rec *store.Record, task registry.Task) {
	if task.Grant == nil || len(task.Grant.Commands) == 0 {
		e.log.Info("a revoke arrived but no command was authorised for this task",
			"issue", rec.IssueID, "by", rec.PrimaryActor)
		return
	}
	if _, err := e.opts.Registry.Update(e.opts.Source.Name(), rec.IssueID, func(t *registry.Task) {
		t.Grant = nil
	}); err != nil {
		e.log.Error("cannot revoke the authorisation", "issue", rec.IssueID, "error", err)
		return
	}
	e.log.Warn("a human revoked the authorised commands; the policy applies again",
		"issue", rec.IssueID, "by", rec.PrimaryActor,
		"revoked", strings.Join(task.Grant.Commands, " && "))
}

// shortCommit renders a commit id for a log line: long enough to be unambiguous in
// a small repository, short enough that the interesting fields stay visible.
