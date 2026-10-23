// Package dispatch turns an accepted event into one unattended agent turn.
//
// It owns intake, routing and scheduling. The task lifecycle (ensure, one turn,
// grant, revoke) lives in internal/tasks and is shared with the flow engine.
// Everything here runs on a worker goroutine, never on the webhook request path,
// because the publisher of the webhook waits for our 202.
//
// It names no tracker and no agent product (ADR 0001): the event source, the runtime
// and the workspace provider are all injected, and each one owns the policy that is
// really its own — the source the trigger phrase and the tool allowlist, the runtime
// the session ruleset and the shell policy, the provider the baseline and the
// checkout.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
	"github.com/yusiwen/flowhub/internal/compare"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/runner"
	"github.com/yusiwen/flowhub/internal/runtimes"
	"github.com/yusiwen/flowhub/internal/source"
	"github.com/yusiwen/flowhub/internal/store"
	tasksvc "github.com/yusiwen/flowhub/internal/tasks"
	"github.com/yusiwen/flowhub/internal/workspace"
)

// Options configures a Dispatcher.
type Options struct {
	Registry *registry.Registry
	Projects *projectmap.Map
	Log      *slog.Logger
	// Runtimes is the enrolled runtime inventory. When it is nil or empty the
	// dispatcher falls back to DefaultRuntime, which is the environment-configured
	// one named "default".
	Runtimes *runtimes.Inventory
	// NewRuntime builds the runtime adapter for one host from its name and address.
	// It is injected because the dispatcher learns a runtime's address at routing
	// time (the inventory is mutable while the process runs) and must not name a
	// product to build one.
	NewRuntime func(name, url string) agent.Runtime
	// DefaultRuntime is the runtime configured purely through the environment,
	// used when nothing else names one. Nil means "only enrolled or declared
	// runtimes".
	DefaultRuntime agent.Runtime
	// Declared are the runtimes the configuration file declares. A declared name that
	// is not enrolled is still usable: it is the single-host case, and it is what
	// FLOWHUB_OPENCODE_URL used to be the only way to express.
	Declared []DeclaredRuntime
	// NewWorkspace builds the workspace provider for one checkout base directory.
	// Injected for the same reason as NewRuntime: which provider produces a task's
	// directory is a deployment decision, not a routing one.
	NewWorkspace func(base string) (workspace.Workspace, error)
	// WorktreeBase is the fallback directory for task checkouts, used when a routing
	// entry does not declare its own. Entries normally declare one, because the base
	// has to be outside the repository it belongs to.
	WorktreeBase string
	// Agent is the runtime's agent profile name; empty uses the runtime default.
	Agent string
	// Deadline bounds one turn.
	Deadline time.Duration
	// FirstResponse bounds how long a turn may take to produce its first assistant
	// message before it is failed. Zero leaves the runtime's own default.
	FirstResponse time.Duration
	// Source decodes deliveries and supplies this tracker's policy, prompt and tool
	// allowlist. Required: the dispatcher names no vendor (ADR 0001).
	Source source.Source
	// QueueSize bounds the deliveries waiting for the worker.
	QueueSize int
	// PauseFile disables dispatch while it exists.
	PauseFile string
	// AttachmentsDir is where downloads must be written, relative to the worktree.
	AttachmentsDir string
	// MaxCostPerTask stops a task whose accumulated cost passes this value.
	// Zero disables the check.
	MaxCostPerTask float64
	// Capacity is the shared per-runtime turn gate. Nil means the dispatcher
	// creates a private one; the flow engine passes the same instance.
	Capacity *runner.Capacity
	// Skip, when set, asks whether another orchestrator owns this delivery. It runs
	// before the queue: a project moved to an `on` flow must not also be dispatched.
	// The reason is logged; a skipped delivery is never handled or audited as work.
	Skip func(*store.Record) (bool, string)
	// Decision, when set, receives dispatch's decision for an event. The flow engine
	// joins it with its shadow decision by the event id.
	Decision func(compare.Decision)
}

// DeclaredRuntime is a runtime the configuration file declares: where it is, and the
// policy to ask of it. Enrolment is a separate record (the control-plane inventory):
// it answers "has this host been prepared and verified, and is it up", while this
// answers "what should the dispatcher ask of it".
type DeclaredRuntime struct {
	Name string
	URL  string
	// Agent is the runtime's agent profile; empty falls back to the project and then
	// to the process default.
	Agent string
	// Model pins the model for this host; empty falls back to what the host reported
	// at enrolment.
	Model string
	// Deadline bounds one turn on this host; zero falls back to the process default.
	Deadline time.Duration
	// MaxConcurrent is how many distinct tasks this host may serve at once. Zero or
	// absent means 1; the host's own claim can only lower it (ADR 0003 §6).
	MaxConcurrent int
}

// Dispatcher consumes accepted deliveries.
type Dispatcher struct {
	opts Options
	log  *slog.Logger
	// policy is the trigger policy the source supplied, resolved once so every
	// delivery and every prompt is judged by the same rules.
	policy rules.Policy

	// cacheMu guards runtimes, which is rebuilt when a host's address changes.
	cacheMu  sync.Mutex
	runtimes map[string]cachedRuntime

	queue   chan *store.Record
	queued  atomic.Int64
	handled atomic.Int64
	dropped atomic.Int64
	ignored atomic.Int64

	// queueMu guards the per-runtime schedulers, the routing memo and the per-runtime
	// error text. The intake loop creates a scheduler (and starts its workers) the
	// first time a runtime is used, and /healthz reads the counters from an HTTP
	// handler. The lock order when both are held is queueMu, then a scheduler's own
	// mutex, and never the reverse.
	queueMu    sync.Mutex
	schedulers map[string]*runtimeScheduler
	// routes remembers which runtime an in-flight burst of a task was routed to. It
	// exists because a task whose registry row does not exist yet would otherwise be
	// ranked once per delivery, and two deliveries of one burst could be handed to
	// two hosts — two worktrees, two sessions, one issue (ADR 0003 §3). The entry is
	// dropped when that task's work drains, so this stays a cache of a decision
	// rather than a second registry.
	routes    map[taskID]runtimeBinding
	lastError map[string]string

	// capacity counts and bounds turns per runtime; the selection policy and
	// /healthz read the same counters, and the flow engine shares the gate.
	capacity *runner.Capacity

	// taskExec is the extracted task lifecycle (M3-1). Dispatch still routes and
	// schedules, but ensure/turn/grant/revoke no longer live here.
	taskExec *tasksvc.Executor
}

// New builds a dispatcher. It does not start working until Run is called.
func New(opts Options) (*Dispatcher, error) {
	if opts.Registry == nil {
		return nil, errors.New("dispatch: a registry is required")
	}

	if opts.Projects == nil {
		return nil, errors.New("dispatch: a routing table is required")
	}
	if opts.Source == nil {
		return nil, errors.New("dispatch: an event source is required")
	}
	if opts.NewWorkspace == nil {
		return nil, errors.New("dispatch: a workspace provider factory is required")
	}
	if opts.NewRuntime == nil {
		return nil, errors.New("dispatch: a runtime factory is required")
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
	capacity := opts.Capacity
	if capacity == nil {
		capacity = runner.NewCapacity()
	}
	ruleset := opts.Source.Policy().Defaults()
	if strings.TrimSpace(ruleset.Trigger) == "" {
		// A dispatcher whose policy has no trigger is a silent no-op that looks
		// healthy, so the missing trigger is a programming error rather than a
		// configuration one: the adapter's defaults always supply it.
		return nil, errors.New("dispatch: the event source supplied no trigger policy")
	}

	d := &Dispatcher{
		opts:       opts,
		policy:     ruleset,
		log:        opts.Log,
		runtimes:   map[string]cachedRuntime{},
		capacity:   capacity,
		queue:      make(chan *store.Record, opts.QueueSize),
		schedulers: map[string]*runtimeScheduler{},
		routes:     map[taskID]runtimeBinding{},
		lastError:  map[string]string{},
	}
	exec, err := tasksvc.New(tasksvc.Options{
		Registry:     opts.Registry,
		Source:       opts.Source,
		Policy:       ruleset,
		Log:          opts.Log,
		WorktreeBase: opts.WorktreeBase,
		NewWorkspace: opts.NewWorkspace,
		Capacity:     capacity,
		RuntimeError: d.setRuntimeError,
		RuntimeOK:    d.clearRuntimeError,
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch: task service: %w", err)
	}
	d.taskExec = exec
	return d, nil
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
	if d.opts.Skip != nil {
		if skip, reason := d.opts.Skip(rec); skip {
			d.ignored.Add(1)
			d.log.Info("dispatch skipped; a flow owns the project",
				"issue", rec.IssueID, "project", rec.ProjectKey, "reason", reason)
			return
		}
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
// runtime, and that runtime's scheduler runs the turn.
type routed struct {
	record  *store.Record
	binding runtimeBinding
}

// taskIDOf is the identity a delivery's turn is serialized by.
func (d *Dispatcher) taskIDOf(rec *store.Record) taskID {
	return taskID{source: d.opts.Source.Name(), key: rec.IssueID}
}

// Run routes deliveries to their runtime's workers until the context is cancelled.
//
// The unit of *routing* is the runtime; the unit of *serialization* is the task
// (ADR 0003). One scheduler per runtime groups the deliveries routed to it by task
// and serves up to that host's breadth of distinct tasks at once, so different tasks
// run in parallel while one task's second delivery waits for its first turn. The
// reason the exclusion exists at all — a prompt sent to a busy session is silently
// swallowed (opencode issue #46842) — is a property of a *session*, and a task owns
// exactly one session for life.
//
// The intake loop itself only decides the runtime (and probes it, so it is off the
// webhook path but still serialized); it never runs a turn.
func (d *Dispatcher) Run(ctx context.Context) {
	d.log.Info("dispatcher started",
		"queue_size", d.opts.QueueSize, "deadline", d.opts.Deadline,
		"agent", d.opts.Agent, "analyze_on_create", !d.policy.SkipAnalyzeOnCreate,
		"trigger", d.policy.Trigger)
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
// Production goes through Run — intake, then the runtime's scheduler and workers —
// while this is the same two steps without a scheduler in between, which is what the
// pipeline tests want. Both call exactly the same code, and this path is
// deliberately serial, so it releases the routing memo itself.
func (d *Dispatcher) handle(ctx context.Context, rec *store.Record) {
	item, ok := d.routeOne(ctx, rec)
	if !ok {
		return
	}
	defer d.forgetRoute(d.taskIDOf(rec))
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
// many tasks it is serving, how many turns are actually running, the breadth it was
// started with, and the last thing that went wrong on it. One HTTP call answers
// "which host is stuck" instead of a log dig.
func (d *Dispatcher) runtimeSnapshot() map[string]map[string]any {
	d.queueMu.Lock()
	schedulers := make(map[string]*runtimeScheduler, len(d.schedulers))
	for name, scheduler := range d.schedulers {
		schedulers[name] = scheduler
	}
	errors := make(map[string]string, len(d.lastError))
	for name, message := range d.lastError {
		errors[name] = message
	}
	d.queueMu.Unlock()

	out := make(map[string]map[string]any, len(schedulers))
	for name, scheduler := range schedulers {
		// The scheduler's own mutex is taken here without queueMu held, so the only
		// lock order in this file stays queueMu -> scheduler, never the reverse.
		entry := scheduler.snapshot()
		entry["in_flight"] = d.inFlightFor(name)
		if message, ok := errors[name]; ok {
			entry["last_error"] = message
		}
		out[name] = entry
	}
	return out
}

// enterTurn records that a turn is running on a runtime and returns the function
// that clears it. The spread policy ranks by these counters, so with one worker
// per runtime (ADR 0001 step 5) they say which host is busy right now.
func (d *Dispatcher) observeDecision(rec *store.Record, decision rules.Decision, routable bool) {
	if d.opts.Decision == nil || rec == nil || rec.EventID == "" {
		return
	}
	d.opts.Decision(compare.Decision{
		EventID: rec.EventID, Source: d.opts.Source.Name(), IssueID: rec.IssueID,
		Project: rec.ProjectKey, Action: string(decision.Action), Reason: decision.Reason,
		Routable: routable, At: time.Now().UTC(),
	})
}

func (d *Dispatcher) inFlightFor(runtime string) int {
	return d.capacity.InFlight(runtime)
}

// inFlightSnapshot copies the counters for /healthz.
func (d *Dispatcher) inFlightSnapshot() map[string]int {
	return d.capacity.Snapshot()
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

	task, _ := d.opts.Registry.Get(d.opts.Source.Name(), rec.IssueID)
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

	// A task with no row yet is ranked once per burst, not once per delivery: two
	// deliveries that arrive together would otherwise be free to choose two hosts,
	// and the issue would get two worktrees and two sessions. The memo is dropped
	// when this task's work drains, so the registry stays authoritative.
	id := d.taskIDOf(rec)
	if binding, ok := d.rememberedRoute(id); ok {
		return routed{record: rec, binding: binding}, true
	}

	match, ok := d.opts.Projects.Match(d.opts.Source.Name(), rec.ProjectKey)
	if !ok {
		// Fail closed, and fail here rather than in the worker: an unroutable project
		// must not take up a runtime's queue slot.
		d.ignored.Add(1)
		d.log.Warn("no repository is mapped for this project in this source; delivery ignored",
			"issue", rec.IssueID, "source", d.opts.Source.Name(), "project", rec.ProjectKey)
		return routed{}, false
	}
	binding, err := d.pickRuntime(ctx, task, match.Entry)
	if err != nil {
		d.ignored.Add(1)
		d.log.Warn("no runtime can serve this delivery; it is ignored",
			"issue", rec.IssueID, "project", rec.ProjectKey, "error", err)
		return routed{}, false
	}
	d.rememberRoute(id, binding)
	return routed{record: rec, binding: binding}, true
}

// rememberedRoute returns the runtime an in-flight burst of this task was routed
// to, if there is one.
func (d *Dispatcher) rememberedRoute(id taskID) (runtimeBinding, bool) {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	binding, ok := d.routes[id]
	return binding, ok
}

// rememberRoute records the routing decision for a task whose work is now in flight.
func (d *Dispatcher) rememberRoute(id taskID, binding runtimeBinding) {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	d.routes[id] = binding
}

// forgetRoute drops the memo once a task's work has drained. The next delivery then
// reads the registry again, which by now carries the binding the turn recorded.
func (d *Dispatcher) forgetRoute(id taskID) {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	delete(d.routes, id)
}

// pendingRoutes counts the routing decisions still remembered. It is used by the
// scheduler's tests, because a memo that never empties would be a second registry.
func (d *Dispatcher) pendingRoutes() int {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	return len(d.routes)
}

// schedulerFor returns a runtime's scheduler, creating it — with its workers'
// breadth — the first time the runtime is used. The second result says whether it
// was created, which is the caller's signal to start its workers.
//
// The breadth is fixed here and does not follow a later re-enrolment: work already
// queued must not be lost, and shrinking a live pool means deciding what to do with
// the turns it already started (ADR 0003 §6).
func (d *Dispatcher) schedulerFor(name string, breadth int) (*runtimeScheduler, bool) {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	if scheduler, ok := d.schedulers[name]; ok {
		return scheduler, false
	}
	scheduler := newRuntimeScheduler(name, breadth, d.opts.QueueSize)
	d.schedulers[name] = scheduler
	d.capacity.SetLimit(name, breadth)
	return scheduler, true
}

// enqueue hands a delivery to its runtime's scheduler, starting that runtime's
// workers on first use. A full runtime drops the delivery, exactly like the intake
// queue: blocking here would push back into the webhook request path.
//
// Workers are never stopped except by the context. An idle goroutine per known
// runtime costs nothing next to the bookkeeping of stopping one safely, and a
// runtime that is revoked and re-enrolled keeps its scheduler rather than losing work
// that was already routed to it.
func (d *Dispatcher) enqueue(ctx context.Context, item routed, workers *sync.WaitGroup) {
	name := item.binding.Name
	breadth := item.binding.maxConcurrent()
	scheduler, created := d.schedulerFor(name, breadth)
	if created {
		d.log.Info("runtime workers started",
			"runtime", name, "max_concurrent", breadth, "queue_size", d.opts.QueueSize)
		for range breadth {
			workers.Add(1)
			go func() {
				defer workers.Done()
				d.workScheduled(ctx, scheduler)
			}()
		}
	}
	id := d.taskIDOf(item.record)
	if !scheduler.add(id, item) {
		d.dropped.Add(1)
		d.log.Error("the runtime's queue is full, dropping delivery",
			"runtime", name, "issue", item.record.IssueID, "queue_size", d.opts.QueueSize)
		// Nothing of this task is left here, so the memo must not keep pointing at
		// this runtime: the next delivery is free to rank again.
		if scheduler.idle(id) {
			d.forgetRoute(id)
		}
	}
}

// workScheduled is one of a runtime's workers: it serves one delivery at a time,
// and the scheduler guarantees that no two of them serve the same task at once.
func (d *Dispatcher) workScheduled(ctx context.Context, scheduler *runtimeScheduler) {
	for {
		item, ok := scheduler.next(ctx)
		if !ok {
			return
		}
		id := d.taskIDOf(item.record)
		d.runRouted(ctx, item)
		d.handled.Add(1)
		if scheduler.finish(id) {
			d.forgetRoute(id)
		}
	}
}

// busyFor reports how much work is committed to a runtime right now: deliveries
// accepted and not yet started, plus the tasks being served. The spread policy reads
// it, so two deliveries that arrive together do not both land on the host that
// happens to sort first.
func (d *Dispatcher) busyFor(name string) int {
	d.queueMu.Lock()
	scheduler, ok := d.schedulers[name]
	d.queueMu.Unlock()
	if !ok {
		return 0
	}
	return scheduler.busy()
}

// runningFor reports how many tasks a runtime is serving right now.
func (d *Dispatcher) runningFor(name string) int {
	d.queueMu.Lock()
	scheduler, ok := d.schedulers[name]
	d.queueMu.Unlock()
	if !ok {
		return 0
	}
	return scheduler.runningCount()
}

// breadthFor is how many tasks a runtime may serve at once: the breadth its
// scheduler was started with when there is one, because that is the number actually
// in force — a breadth fixed at first use does not follow a later re-enrolment
// (ADR 0003 §6) — and otherwise the configured breadth, for a host no delivery has
// reached yet.
func (d *Dispatcher) breadthFor(binding runtimeBinding) int {
	d.queueMu.Lock()
	scheduler, ok := d.schedulers[binding.Name]
	d.queueMu.Unlock()
	if ok {
		// The field is written once, before the scheduler is published under queueMu,
		// so loading it after taking that lock is ordered.
		return scheduler.max
	}
	return binding.maxConcurrent()
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
	effective := rules.Decision{}
	routable := true
	defer func() {
		if effective.Action != "" {
			d.observeDecision(rec, effective, routable)
		}
	}()
	if d.opts.Skip != nil {
		if skip, reason := d.opts.Skip(rec); skip {
			d.ignored.Add(1)
			d.log.Info("dispatch skipped before the turn; a flow owns the project",
				"issue", rec.IssueID, "project", rec.ProjectKey, "reason", reason)
			return
		}
	}
	if d.Paused() {
		d.ignored.Add(1)
		effective = rules.Decision{Action: rules.ActionIgnore, Reason: "dispatch is paused"}
		routable = false
		d.log.Warn("dispatch is paused; delivery ignored",
			"pause_file", d.opts.PauseFile, "issue", rec.IssueID, "event", rec.Event)
		return
	}

	decoded, err := d.opts.Source.Parse(&source.Request{
		Method: rec.Method, Path: rec.Path, Query: rec.Query,
		RemoteIP: rec.RemoteIP, UserAgent: rec.UserAgent, Body: []byte(rec.RawBody),
	})
	if err != nil || decoded.Event == nil {
		// A delivery that reached this point has already been decoded once; a
		// failure here means the audit record was tampered with.
		d.log.Error("cannot re-decode an accepted delivery", "issue", rec.IssueID, "error", err)
		return
	}
	delivery := *decoded.Event
	if err := delivery.Validate(); err != nil {
		d.log.Error("the source produced an unusable event", "issue", rec.IssueID, "error", err)
		return
	}

	task, known := d.opts.Registry.Get(d.opts.Source.Name(), rec.IssueID)
	taskView := rules.TaskView{Known: known}
	if known {
		taskView.Plan = task.Plan
		taskView.Turns = task.Turns
		taskView.LastReply = task.LastReply
		// What a bare `/opencode permit` authorises: the commands the last turn was
		// refused for, so rules can tell "there is something to authorise" from
		// "the human is asking for something that does not exist".
		taskView.Refusals = task.Refusals
	}

	decision := d.policy.Decide(delivery, taskView)
	if decision.Action == rules.ActionIgnore {
		effective = decision
		d.ignored.Add(1)
		d.log.Info("delivery not dispatched",
			"issue", rec.IssueID, "event", rec.Event, "reason", decision.Reason)
		return
	}
	if d.opts.MaxCostPerTask > 0 && known && task.Cost >= d.opts.MaxCostPerTask {
		effective = rules.Decision{Action: rules.ActionIgnore, Reason: "the task exceeded its cost budget"}
		d.ignored.Add(1)
		d.log.Warn("task exceeded its cost budget; a human must take over",
			"issue", rec.IssueID, "cost", task.Cost, "budget", d.opts.MaxCostPerTask)
		return
	}

	match, ok := d.opts.Projects.Match(d.opts.Source.Name(), rec.ProjectKey)
	if !ok {
		// Fail closed. Guessing a repository is the one mistake this design
		// refuses to make.
		effective = rules.Decision{Action: rules.ActionIgnore, Reason: "no repository is mapped for this project"}
		routable = false
		d.ignored.Add(1)
		d.log.Warn("no repository is mapped for this project in this source; delivery ignored",
			"issue", rec.IssueID, "source", d.opts.Source.Name(), "project", rec.ProjectKey)
		return
	}
	if !authorAllowed(match.Entry.Authors, rec.PrimaryActor) {
		// A per-project allowlist is a restriction, so an actor it does not name
		// gets no work — including the no-actor events (issueDeleted carries no
		// actor), which can therefore never pass this gate.
		effective = rules.Decision{Action: rules.ActionIgnore, Reason: "the actor is not on the project's author allowlist"}
		d.ignored.Add(1)
		d.log.Warn("actor is not on the project's author allowlist; delivery ignored",
			"issue", rec.IssueID, "project", rec.ProjectKey, "actor", rec.PrimaryActor,
			"authors", strings.Join(match.Entry.Authors, ","))
		return
	}

	if decision.Action == rules.ActionRevoke {
		// A retraction runs no turn: there is nothing for the agent to do about it, and
		// spending a turn to say so would be noise. It is answered here — after the
		// actor check, before anything is prepared — so revoking cannot be blocked by
		// the worktree or the binding.
		effective = decision
		if err := d.taskExec.Revoke(ctx, tasksvc.RevokeRequest{Record: rec, Task: task}); err != nil {
			d.log.Error("cannot revoke the authorisation", "issue", rec.IssueID, "error", err)
		}
		return
	}

	// The registry is authoritative about the binding: the router chose this runtime
	// from the same record, but a task may never move between runtimes, so it is
	// checked here too rather than trusted.
	if name := strings.TrimSpace(task.Runtime); name != "" && name != binding.Name {
		effective = rules.Decision{Action: rules.ActionIgnore, Reason: "the task is bound to another runtime"}
		d.ignored.Add(1)
		d.log.Error("refusing to move a bound task to another runtime",
			"issue", rec.IssueID, "bound", name, "chosen", binding.Name)
		return
	}

	host := d.flowHost(match.Entry, binding)
	task, err = d.taskExec.Ensure(ctx, tasksvc.EnsureRequest{
		Record: rec, Delivery: delivery, Entry: match.Entry, Host: host,
	})
	if err != nil {
		effective = rules.Decision{Action: rules.ActionIgnore, Reason: "the task could not be prepared"}
		return
	}

	if decision.Action == rules.ActionPermit {
		// The authorisation is recorded first, and then the *same delivery* becomes the
		// turn that carries on: the human's comment is what unblocks the task, so it
		// must not need a second trigger.
		effective = decision
		granted, err := d.taskExec.Grant(ctx, tasksvc.GrantRequest{
			Record: rec, Task: task, Entry: match.Entry, Trigger: string(decision.Trigger),
		})
		if err != nil {
			d.log.Error("cannot record the authorisation", "issue", rec.IssueID, "error", err)
			return
		}
		task = granted.Task
		decision.Action = granted.Action
		if decision.Action == rules.ActionIgnore {
			effective = decision
			return
		}
	} else {
		effective = decision
	}
	if _, err := d.taskExec.Turn(ctx, tasksvc.TurnRequest{
		Record: rec, Delivery: delivery, Task: task, Entry: match.Entry, Host: host,
		Action: decision.Action, Trigger: decision.Trigger, RequireReply: true,
		Basis: decision.Reason,
	}); err != nil {
		d.log.Error("task turn failed", "issue", rec.IssueID, "error", err)
	}
}

// ensureTask creates the registry row and the worktree if they do not exist yet,
// and refuses to continue when the mapping changed under a running task.

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
	// An enrolled host's attested profile beats the configuration file, because
	// enrolment is the verification: the host reported which profiles exist on it,
	// and `init --check` confirmed the one it claimed. A name the operator writes in
	// the file for such a host would be unverified, and an unverified name makes the
	// product fall back to its own default agent — a looser one. The file's value is
	// therefore used only for a runtime that was never enrolled, or one that reported
	// no profile at all.
	if profile := strings.TrimSpace(binding.AgentProfile); profile != "" {
		return profile
	}
	if declared := strings.TrimSpace(binding.Declared.Agent); declared != "" {
		return declared
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
