// Package engine executes the builtin flow.
//
// A shadow run resolves the task projection, evaluates the upstream policy and
// walks the graph, but every node that would touch a workspace, a session or a
// runtime is recorded as skipped. An `on` run uses the same graph and task
// service, so the decision and the execution path are the same code.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yusiwen/flowhub/internal/compare"
	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/flow"
	"github.com/yusiwen/flowhub/internal/flow/mode"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/store"
	tasksvc "github.com/yusiwen/flowhub/internal/tasks"
	"github.com/yusiwen/flowhub/internal/upstream"
)

// Router answers whether an event's project has a routing entry.
type Router interface {
	Match(source, project string) (projectmap.Match, bool)
}

// Tasks is the read side of the task registry.
type Tasks interface {
	Get(source, key string) (registry.Task, bool)
}

// Options configures a shadow engine.
type Options struct {
	Flow       *flow.Compiled
	Definition []byte
	Projects   Router
	Tasks      Tasks
	Policy     rules.Policy
	DataDir    string
	QueueSize  int
	Logger     *slog.Logger
	Now        func() time.Time
	// ReplaySource reads the durable log after a cursor. It is what makes the
	// queue's drops recoverable while the process is still running; nil disables
	// periodic catch-up.
	ReplaySource func(after string) ([]upstream.Event, error)
	// ReplayInterval bounds how long a dropped event can wait; zero means 5s.
	ReplayInterval time.Duration
	// MaxConcurrent is the number of distinct tasks the scheduler may run at
	// once. Events for one task stay serialized in arrival order. Zero means 1.
	// The task service still enforces its per-runtime capacity.
	MaxConcurrent int
	// PauseFile is the dispatch kill switch; an `on` flow must honour it too.
	PauseFile string
	// MaxCostPerTask stops an `on` turn whose task has already spent this much.
	// Zero disables the check.
	MaxCostPerTask float64
	// ModeEnabled gates shadow execution. Nil means enabled (tests and callers
	// that do not have a deployment state yet). ModeFor supersedes it when set:
	// it answers off/shadow/on for one project.
	ModeEnabled func(flowID string) bool
	ModeFor     func(flowID, project string) string
	// TaskService and TaskSelector are required for `on`; nil keeps the engine
	// shadow-only.
	TaskService  tasksvc.Service
	TaskSelector tasksvc.Selector
	// Compare joins a shadow decision with dispatch's decision for the same event.
	Compare *compare.Store
}

// Engine consumes events and writes shadow runs.
type Engine struct {
	opts  Options
	log   *slog.Logger
	store *Store
	now   func() time.Time

	queue    chan upstream.Event
	done     chan struct{}
	taskDone chan string
	wg       sync.WaitGroup
	workers  sync.WaitGroup

	// Scheduler state. pending holds the events for each task in arrival order;
	// active is the set of tasks with a worker. MaxConcurrent bounds active, so a
	// long turn cannot starve every other task.
	schedMu      sync.Mutex
	pending      map[string][]scheduledEvent
	runningTasks map[string]bool

	startMu    sync.Mutex
	startSeq   uint64
	startOrder []startEntry
	started    map[uint64]bool

	mu     sync.Mutex
	closed bool
	cancel context.CancelFunc
	seen   map[string]bool
	queued map[string]bool

	cursorMu sync.Mutex
	cursor   string

	submitted atomic.Int64
	dropped   atomic.Int64
	runs      atomic.Int64
	failed    atomic.Int64

	// active holds the cancel function for each run this process is executing.
	// canceled survives the run's state save so a cancellation racing the final
	// failure path still wins and records canceled, not failed.
	activeMu sync.Mutex
	active   map[string]context.CancelFunc
	canceled map[string]bool
	cancelMu sync.Mutex
}

// Cancel refuses when the run is not active in this process. It is separate from
// not-cancelable (a terminal run) so the API can answer 409 without guessing.
var (
	ErrRunNotActive     = errors.New("run is not active in this process")
	ErrRunNotCancelable = errors.New("run is not cancelable")
)

// New validates the options, opens the run store and recovers in-flight runs.
func New(opts Options) (*Engine, error) {
	if opts.Flow == nil {
		return nil, fmt.Errorf("engine: a compiled flow is required")
	}
	if opts.DataDir == "" {
		return nil, fmt.Errorf("engine: a data directory is required")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 64
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ReplayInterval <= 0 {
		opts.ReplayInterval = 5 * time.Second
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 1
	}
	if opts.MaxConcurrent > 64 {
		opts.MaxConcurrent = 64
	}
	store, err := NewStore(opts.DataDir)
	if err != nil {
		return nil, err
	}
	recovered, err := store.Recover()
	if err != nil {
		return nil, err
	}
	if recovered > 0 {
		opts.Logger.Warn("flow runs interrupted by the restart were marked needs_attention", "runs", recovered)
	}
	engine := &Engine{
		opts:         opts,
		log:          opts.Logger,
		store:        store,
		now:          opts.Now,
		queue:        make(chan upstream.Event, opts.QueueSize),
		done:         make(chan struct{}),
		taskDone:     make(chan string, opts.MaxConcurrent),
		seen:         map[string]bool{},
		queued:       map[string]bool{},
		active:       map[string]context.CancelFunc{},
		canceled:     map[string]bool{},
		pending:      map[string][]scheduledEvent{},
		runningTasks: map[string]bool{},
		started:      map[uint64]bool{},
	}
	if err := engine.loadCursor(); err != nil {
		return nil, err
	}
	return engine, nil
}

// Start runs the scheduler until ctx is done or Close is called.
func (e *Engine) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.schedule(ctx)
	}()
	if e.opts.ReplaySource != nil {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.catchUp(ctx)
		}()
	}
}

// Submit enqueues an event without blocking. A full queue drops and counts, like
// the audit queue; the event log already has the durable copy.
func (e *Engine) Submit(event upstream.Event) {
	if e == nil {
		return
	}
	e.submitted.Add(1)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		e.dropped.Add(1)
		return
	}
	if event.ID != "" && e.queued[event.ID] {
		return
	}
	select {
	case e.queue <- event:
		if event.ID != "" {
			e.queued[event.ID] = true
		}
	default:
		e.dropped.Add(1)
	}
}

// Close stops the worker and drains the queue. It is idempotent.
// Replay feeds catch-up events to the worker. It blocks while the queue is full,
// so a replay larger than the queue is not silently dropped; it is called at
// startup before the HTTP server starts serving.
func (e *Engine) Replay(events []upstream.Event) {
	for _, event := range events {
		for {
			e.mu.Lock()
			if e.closed {
				e.mu.Unlock()
				return
			}
			if event.ID != "" && e.queued[event.ID] {
				e.mu.Unlock()
				break
			}
			select {
			case e.queue <- event:
				e.submitted.Add(1)
				if event.ID != "" {
					e.queued[event.ID] = true
				}
				e.mu.Unlock()
			default:
				// Do not hold mu while waiting: the worker takes it for its seen
				// map, so holding it here would deadlock a full queue.
				e.mu.Unlock()
				select {
				case <-e.done:
					return
				case <-time.After(10 * time.Millisecond):
				}
				continue
			}
			break
		}
	}
}

func (e *Engine) catchUp(ctx context.Context) {
	ticker := time.NewTicker(e.opts.ReplayInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cursor := e.Cursor()
			// An empty cursor is a real state: the service started with an empty log
			// and the first event arrived while paused. Since("") is the whole log, so
			// catch-up can still recover it; main initializes a non-empty tail before
			// serving when history exists.
			events, err := e.opts.ReplaySource(cursor)
			if err != nil {
				e.log.Warn("periodic flow catch-up failed", "error", err)
				continue
			}
			if len(events) > 0 {
				e.Replay(events)
			}
		}
	}
}

// Cancel stops an active run and records it canceled. The cancel function is
// registered as soon as the run meta exists; a run that has already reached a
// terminal state (or is no longer active here) is refused with 409.
func (e *Engine) Cancel(runID string) (*Run, error) {
	if e == nil || runID == "" {
		return nil, fmt.Errorf("engine: a run id is required")
	}
	run, err := e.store.Load(runID)
	if err != nil {
		return nil, err
	}
	if run.State != StateRunning {
		return run, ErrRunNotCancelable
	}
	e.activeMu.Lock()
	stop, ok := e.active[runID]
	if ok {
		e.canceled[runID] = true
	}
	e.activeMu.Unlock()
	if !ok {
		return run, ErrRunNotActive
	}
	stop()
	return e.finishCanceled(run)
}

func (e *Engine) isCanceled(runID string) bool {
	e.activeMu.Lock()
	defer e.activeMu.Unlock()
	return e.canceled[runID]
}

// finishCanceled is idempotent. It marks any running step interrupted, records
// the terminal state and journals both the cancellation and the finish. It may be
// called by the API while the worker is still unwinding and again by the worker;
// the second call observes canceled and does nothing.
func (e *Engine) finishCanceled(run *Run) (*Run, error) {
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	if run == nil {
		return nil, fmt.Errorf("engine: no run to cancel")
	}
	if run.State == StateCanceled {
		return run, nil
	}
	e.activeMu.Lock()
	canceled := e.canceled[run.ID]
	e.activeMu.Unlock()
	if !canceled && run.State != StateRunning && run.State != StateWaiting {
		return run, ErrRunNotCancelable
	}
	now := e.now().UTC()
	const reason = "canceled by operator"
	for _, step := range run.Steps {
		if step.State == StepRunning {
			step.State = StepInterrupted
			step.Error = reason
			step.FinishedAt = &now
		}
	}
	run.State = StateCanceled
	run.Error = reason
	run.FinishedAt = &now
	if err := e.store.Append(run.ID, "run.canceled", map[string]any{"reason": reason}); err != nil {
		return run, err
	}
	if err := e.store.Append(run.ID, "run.finished", map[string]any{"state": StateCanceled, "error": reason}); err != nil {
		return run, err
	}
	if err := e.store.Save(run); err != nil {
		return run, err
	}
	return run, nil
}

func (e *Engine) finish(event upstream.Event) {
	if event.ID == "" {
		return
	}
	e.mu.Lock()
	delete(e.queued, event.ID)
	e.mu.Unlock()
}

type scheduledEvent struct {
	seq   uint64
	event upstream.Event
}

type startEntry struct {
	seq uint64
	id  string
}

// assignOrder records the scheduler's receive order. Cursor advancement uses
// this order, not worker completion order: parallel runs finish out of order.
func (e *Engine) assignOrder(event upstream.Event) uint64 {
	e.startMu.Lock()
	// A paused event is consumed without advancing the cursor. Its old order
	// entry stays at the head so later events cannot jump over it; when catch-up
	// replays the same event id, replace that entry rather than blocking forever.
	if event.ID != "" {
		kept := e.startOrder[:0]
		for _, entry := range e.startOrder {
			if entry.id == event.ID {
				delete(e.started, entry.seq)
				continue
			}
			kept = append(kept, entry)
		}
		e.startOrder = kept
	}
	e.startSeq++
	seq := e.startSeq
	e.startOrder = append(e.startOrder, startEntry{seq: seq, id: event.ID})
	e.started[seq] = false
	e.startMu.Unlock()
	return seq
}

// markStarted advances the cursor over the contiguous prefix of events that
// have reached the point where their run exists (or their event was ignored).
// An event that has not started blocks later events from advancing it.
func (e *Engine) markStarted(seq uint64) {
	if seq == 0 {
		return
	}
	e.startMu.Lock()
	if e.started[seq] {
		e.startMu.Unlock()
		return
	}
	e.started[seq] = true
	last := ""
	for len(e.startOrder) > 0 && e.started[e.startOrder[0].seq] {
		last = e.startOrder[0].id
		delete(e.started, e.startOrder[0].seq)
		e.startOrder = e.startOrder[1:]
	}
	e.startMu.Unlock()
	if last != "" {
		if err := e.SetCursor(last); err != nil {
			e.log.Warn("could not advance the flow replay cursor", "event", last, "error", err)
		}
	}
}

// schedule is the only reader of e.queue. It groups events by task and starts
// one worker per task, up to MaxConcurrent. A task's events stay in arrival
// order; different tasks may run in parallel, bounded by the task service's
// per-runtime capacity as well.
func (e *Engine) schedule(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-e.queue:
			if !ok {
				return
			}
			e.scheduleEvent(ctx, event)
		case key := <-e.taskDone:
			e.schedMu.Lock()
			delete(e.runningTasks, key)
			start := e.startReadyLocked(ctx)
			e.schedMu.Unlock()
			e.launch(ctx, start)
		}
	}
}

func (e *Engine) scheduleEvent(ctx context.Context, event upstream.Event) {
	if ctx.Err() != nil {
		return
	}
	key := flowTaskKey(event)
	order := e.assignOrder(event)
	e.schedMu.Lock()
	e.pending[key] = append(e.pending[key], scheduledEvent{seq: order, event: event})
	start := e.startReadyLocked(ctx)
	e.schedMu.Unlock()
	e.launch(ctx, start)
}

// startReadyLocked marks pending tasks active while a slot is free. The caller
// holds schedMu. It does not start goroutines while holding the lock.
func (e *Engine) startReadyLocked(ctx context.Context) []string {
	var start []string
	if ctx.Err() != nil {
		return start
	}
	for key, queue := range e.pending {
		if len(queue) == 0 {
			delete(e.pending, key)
			continue
		}
		if e.runningTasks[key] {
			continue
		}
		if len(e.runningTasks) >= e.opts.MaxConcurrent {
			break
		}
		e.runningTasks[key] = true
		start = append(start, key)
	}
	return start
}

func (e *Engine) launch(ctx context.Context, keys []string) {
	for _, key := range keys {
		e.workers.Add(1)
		go e.runTask(ctx, key)
	}
}

func (e *Engine) runTask(ctx context.Context, key string) {
	defer e.workers.Done()
	for {
		e.schedMu.Lock()
		queue := e.pending[key]
		if len(queue) == 0 {
			delete(e.pending, key)
			e.schedMu.Unlock()
			select {
			case e.taskDone <- key:
			case <-ctx.Done():
			}
			return
		}
		item := queue[0]
		e.pending[key] = queue[1:]
		e.schedMu.Unlock()

		if _, err := e.runEventContext(ctx, item.event, item.seq); err != nil {
			e.failed.Add(1)
			e.log.Error("flow run failed", "event", item.event.ID, "error", err)
		}
		if ctx.Err() != nil {
			e.schedMu.Lock()
			delete(e.pending, key)
			delete(e.runningTasks, key)
			e.schedMu.Unlock()
			return
		}
	}
}

func flowTaskKey(event upstream.Event) string {
	if event.Subject.Key != "" {
		return event.Upstream + "\x00" + event.Subject.Key
	}
	return event.ID
}

func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	cancel := e.cancel
	close(e.queue)
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	e.wg.Wait()
	e.workers.Wait()
	close(e.done)
	return nil
}

// Snapshot is the /healthz projection for the shadow engine.
func (e *Engine) Snapshot() map[string]any {
	if e == nil {
		return map[string]any{"enabled": false}
	}
	e.schedMu.Lock()
	activeTasks := len(e.runningTasks)
	pendingTasks := len(e.pending)
	e.schedMu.Unlock()
	return map[string]any{
		"enabled":           true,
		"mode":              "shadow",
		"flow":              e.opts.Flow.Definition.ID,
		"submitted":         e.submitted.Load(),
		"dropped":           e.dropped.Load(),
		"runs":              e.runs.Load(),
		"failed":            e.failed.Load(),
		"on_available":      e.CanExecuteOn(),
		"concurrency":       e.opts.MaxConcurrent,
		"active_tasks":      activeTasks,
		"pending_tasks":     pendingTasks,
		"pending":           e.pendingCompare(),
		"shadow_compared":   e.compareCounts(),
		"shadow_mismatches": e.compareMismatches(),
		"dir":               e.store.Dir(),
	}
}

// CanExecuteOn reports whether an `on` deployment can run turns in this process:
// both the task service and the runtime selector are wired.
func (e *Engine) CanExecuteOn() bool {
	return e != nil && e.opts.TaskService != nil && e.opts.TaskSelector != nil
}

// RunStore exposes the run store to the hub API.
func (e *Engine) RunStore() *Store {
	if e == nil {
		return nil
	}
	return e.store
}

// RunEvent executes one event synchronously with a background context. It is
// exported for the parity harness.
func (e *Engine) RunEvent(event upstream.Event) (*Run, error) {
	return e.RunEventContext(context.Background(), event)
}

// RunEventContext executes one event synchronously. The worker passes its own
// context so an `on` turn is cancelled on shutdown.
func (e *Engine) RunEventContext(ctx context.Context, event upstream.Event) (*Run, error) {
	return e.runEventContext(ctx, event, 0)
}

func (e *Engine) runEventContext(ctx context.Context, event upstream.Event, order uint64) (run *Run, err error) {
	defer func() {
		if run != nil {
			e.activeMu.Lock()
			delete(e.canceled, run.ID)
			e.activeMu.Unlock()
		}
		e.finish(event)
		if order == 0 && err == nil && !e.paused() {
			e.advance(event)
		}
	}()
	deployment := e.modeFor(event)
	if deployment == mode.Off || deployment == "" {
		if order != 0 {
			e.markStarted(order)
		}
		return nil, nil
	}
	if e.paused() {
		e.log.Warn("flow engine is paused; delivery ignored", "pause_file", e.opts.PauseFile, "event", event.ID)
		return nil, nil
	}
	if deployment == mode.On && !e.CanExecuteOn() {
		return nil, fmt.Errorf("on needs a task service; this process has none")
	}
	if deployment == mode.On {
		if !e.authorAllowed(event) {
			e.log.Warn("actor is not on the project's author allowlist; flow delivery ignored",
				"issue", event.Subject.Key, "project", event.Subject.Project, "actor", event.Actor.Login)
			if order != 0 {
				e.markStarted(order)
			}
			return nil, nil
		}
		if task, known := e.task(event); known && e.opts.MaxCostPerTask > 0 && task.Cost >= e.opts.MaxCostPerTask {
			e.log.Warn("task exceeded its cost budget; a human must take over",
				"issue", event.Subject.Key, "cost", task.Cost, "budget", e.opts.MaxCostPerTask)
			if order != 0 {
				e.markStarted(order)
			}
			return nil, nil
		}
	}
	trigger := e.triggerFor(event)
	if trigger == nil {
		if order != 0 {
			e.markStarted(order)
		}
		return nil, nil
	}
	key := e.opts.Flow.Definition.ID + "\x00" + trigger.Trigger.ID + "\x00" + event.ID
	e.mu.Lock()
	if e.seen[key] {
		e.mu.Unlock()
		if order != 0 {
			e.markStarted(order)
		}
		return nil, nil
	}
	e.seen[key] = true
	e.mu.Unlock()

	run, err = e.run(ctx, event, trigger, deployment, order)
	if run != nil && e.isCanceled(run.ID) {
		if _, finishErr := e.finishCanceled(run); finishErr != nil {
			return run, finishErr
		}
		e.runs.Add(1)
		return run, nil
	}
	if err != nil {
		e.failed.Add(1)
		return run, err
	}
	e.runs.Add(1)
	return run, nil
}

type stepOutcome struct {
	state    StepState
	output   map[string]any
	next     string
	finished bool
	runState State
}

// liveExecution is the state an `on` run threads through the graph: the record the
// task service expects, the routing entry and the runtime host chosen for the task.
type liveExecution struct {
	record  *store.Record
	entry   *projectmap.Entry
	host    tasksvc.Host
	task    registry.Task
	basis   string
	trigger rules.Trigger
}

func (e *Engine) run(ctx context.Context, event upstream.Event, trigger *flow.CompiledTrigger, deployment string, order uint64) (*Run, error) {
	now := e.now().UTC()
	revision := sha256.Sum256(e.opts.Definition)
	run := &Run{
		ID:        newRunID(now),
		Flow:      e.opts.Flow.Definition.ID,
		Revision:  hex.EncodeToString(revision[:]),
		Mode:      deployment,
		State:     StateRunning,
		Trigger:   trigger.Trigger.ID,
		Event:     event,
		StartedAt: now,
		Steps:     map[string]*Step{},
	}
	var live *liveExecution
	if deployment == mode.On {
		if e.opts.TaskService == nil || e.opts.TaskSelector == nil {
			return e.failRun(run, "", fmt.Errorf("on needs a task service; this process has none"))
		}
		live = &liveExecution{record: recordFor(event)}
	}
	if err := e.store.Create(run); err != nil {
		return run, err
	}
	runCtx, stop := context.WithCancel(ctx)
	e.activeMu.Lock()
	e.active[run.ID] = stop
	e.activeMu.Unlock()
	defer func() {
		stop()
		e.activeMu.Lock()
		delete(e.active, run.ID)
		e.activeMu.Unlock()
	}()
	if err := e.store.Append(run.ID, "run.started", map[string]any{
		"flow": run.Flow, "trigger": run.Trigger, "event_id": event.ID,
	}); err != nil {
		return run, err
	}
	if err := e.store.Save(run); err != nil {
		return run, err
	}
	// The run exists before the cursor advances, so a crash mid-turn leaves a run
	// for recovery instead of losing the event. markStarted waits for earlier
	// scheduler-order events, which is what keeps parallel runs from skipping.
	if order != 0 {
		e.markStarted(order)
	} else {
		e.advance(event)
	}
	tasks, views := e.projections(event)
	steps := map[string]map[string]any{}
	current := e.opts.Flow.Start
	for current != "" {
		node := e.opts.Flow.Nodes[current]
		if node == nil {
			return e.failRun(run, current, fmt.Errorf("node %q is not in the compiled graph", current))
		}
		step := &Step{ID: node.Node.ID, Type: node.Node.Type, State: StepRunning, Attempt: 1, StartedAt: now}
		run.Steps[step.ID] = step
		if err := e.store.Append(run.ID, "step.started", map[string]any{"step": step.ID, "type": step.Type}); err != nil {
			return run, err
		}
		if err := e.store.Save(run); err != nil {
			return run, err
		}

		context, err := e.context(run, event, steps, tasks)
		if err != nil {
			return e.failStep(run, step, err)
		}
		if e.isCanceled(run.ID) {
			return e.finishCanceled(run)
		}
		outcome, err := e.execute(runCtx, node, event, context, views, live)
		if err != nil {
			if e.isCanceled(run.ID) {
				return e.finishCanceled(run)
			}
			return e.failStep(run, step, err)
		}
		step.Output = outcome.output
		step.finish(outcome.state, now)
		steps[step.ID] = map[string]any{"state": string(outcome.state), "output": outcome.output}
		eventType := "step.succeeded"
		if outcome.state == StepSkipped {
			eventType = "step.skipped"
		}
		if err := e.store.Append(run.ID, eventType, map[string]any{
			"step": step.ID, "state": outcome.state, "output": outcome.output,
		}); err != nil {
			return run, err
		}
		if node.Node.Type == string(flow.NodeBranch) && outcome.next != "" {
			if err := e.store.Append(run.ID, "edge.selected", map[string]any{"from": node.Node.ID, "to": outcome.next}); err != nil {
				return run, err
			}
		}
		if err := e.store.Save(run); err != nil {
			return run, err
		}
		if outcome.finished {
			state := StateSucceeded
			if outcome.runState != "" {
				state = outcome.runState
			}
			run.finish(state, now)
			if err := e.store.Append(run.ID, "run.finished", map[string]any{"state": state}); err != nil {
				return run, err
			}
			if err := e.store.Save(run); err != nil {
				return run, err
			}
			e.observeRun(run, event)
			return run, nil
		}
		if outcome.next == "" {
			return e.failRun(run, current, fmt.Errorf("node %q has no selected successor", current))
		}
		current = outcome.next
	}
	return e.failRun(run, "", fmt.Errorf("the graph reached no end node"))
}

func (e *Engine) execute(ctx context.Context, node *flow.CompiledNode, event upstream.Event, context map[string]any, views map[string]rules.TaskView, live *liveExecution) (stepOutcome, error) {
	next := ""
	if len(node.Next) > 0 {
		next = node.Next[0].Edge.To
	}
	switch flow.NodeType(node.Node.Type) {
	case flow.NodeStart:
		return stepOutcome{state: StepSucceeded, next: next}, nil
	case flow.NodeEnsure:
		if live != nil {
			return e.ensureLive(ctx, live, event, next)
		}
		task, found := e.task(event)
		routable := false
		project := event.Subject.Project
		runtime := ""
		if e.opts.Projects != nil {
			if match, ok := e.opts.Projects.Match(event.Upstream, project); ok {
				routable = true
				if match.Entry != nil {
					runtime = match.Entry.Runtime
				}
			}
		}
		taskID := event.Subject.Key
		if found {
			taskID = task.Key
		}
		output := map[string]any{
			"task_id":  taskID,
			"routable": routable,
			"project":  project,
			"runtime":  runtime,
			"reused":   found,
		}
		return stepOutcome{state: StepSucceeded, output: output, next: next}, nil
	case flow.NodeEvaluate:
		config := node.Config.(*flow.EvaluateConfig)
		view := views[config.Task]
		if live != nil && live.task.Key != "" {
			view = viewOf(live.task)
		}
		decision := e.opts.Policy.Decide(neutralEvent(event), view)
		if decision.Action == rules.ActionIgnore {
			// A policy ignore (our own comment, a pause phrase, the turn limit) is
			// the same decision dispatch reaches before it checks routing. Report it
			// as such, so an unmapped project does not turn one ignore into two
			// different reasons during parity.
			return stepOutcome{state: StepSucceeded, output: map[string]any{
				"action": "ignore", "basis": string(decision.Trigger), "reason": decision.Reason,
			}, next: next}, nil
		}
		if !routableFromSteps(context) {
			return stepOutcome{state: StepSucceeded, output: map[string]any{
				"action": "ignore",
				"basis":  "routing",
				"reason": fmt.Sprintf("no routing entry for %s/%s", event.Upstream, event.Subject.Project),
			}, next: next}, nil
		}
		if live != nil {
			live.basis = decision.Reason
			live.trigger = decision.Trigger
		}
		return stepOutcome{state: StepSucceeded, output: map[string]any{
			"action": string(decision.Action),
			"basis":  string(decision.Trigger),
			"reason": decision.Reason,
		}, next: next}, nil
	case flow.NodeBranch:
		selected, err := selectEdge(node, context)
		if err != nil {
			return stepOutcome{}, err
		}
		return stepOutcome{state: StepSucceeded, output: map[string]any{"selected": selected}, next: selected}, nil
	case flow.NodeTurn:
		if live == nil {
			// Shadow never touches a workspace, a session or a runtime.
			return stepOutcome{state: StepSkipped, next: next}, nil
		}
		return e.turnLive(ctx, live, node, event, next)
	case flow.NodeGrant:
		if live == nil {
			return stepOutcome{state: StepSkipped, next: next}, nil
		}
		return e.grantLive(ctx, live, node, next)
	case flow.NodeRevoke:
		if live == nil {
			return stepOutcome{state: StepSkipped, next: next}, nil
		}
		return e.revokeLive(ctx, live, next)
	case flow.NodeEnd:
		config := node.Config.(*flow.EndConfig)
		state := StateSucceeded
		if config.State == "failed" {
			state = StateFailed
		}
		return stepOutcome{state: StepSucceeded, finished: true, runState: state}, nil
	default:
		return stepOutcome{}, fmt.Errorf("node %q: type %q has no shadow executor", node.Node.ID, node.Node.Type)
	}
}

func (e *Engine) paused() bool {
	if e == nil || e.opts.PauseFile == "" {
		return false
	}
	_, err := os.Stat(e.opts.PauseFile)
	return err == nil
}

func (e *Engine) authorAllowed(event upstream.Event) bool {
	if e.opts.Projects == nil {
		return true
	}
	match, ok := e.opts.Projects.Match(event.Upstream, event.Subject.Project)
	if !ok || match.Entry == nil || len(match.Entry.Authors) == 0 {
		return true
	}
	for _, allowed := range match.Entry.Authors {
		if strings.EqualFold(strings.TrimSpace(allowed), strings.TrimSpace(event.Actor.Login)) {
			return true
		}
	}
	return false
}

func (e *Engine) observeRun(run *Run, event upstream.Event) {
	if e.opts.Compare == nil || run == nil || run.Mode != mode.Shadow || event.ID == "" {
		return
	}
	evaluate := run.Steps["evaluate"]
	ensure := run.Steps["ensure"]
	if evaluate == nil || evaluate.Output == nil {
		return
	}
	action, _ := evaluate.Output["action"].(string)
	reason, _ := evaluate.Output["reason"].(string)
	routable := false
	if ensure != nil && ensure.Output != nil {
		routable, _ = ensure.Output["routable"].(bool)
	}
	e.opts.Compare.ObserveFlow(compare.Decision{
		EventID: event.ID, Source: event.Upstream, IssueID: event.Subject.Key,
		Project: event.Subject.Project, Action: action, Reason: reason,
		Routable: routable, At: e.now().UTC(),
	})
}

func (e *Engine) compareCounts() int64 {
	if e == nil || e.opts.Compare == nil {
		return 0
	}
	compared, _ := e.opts.Compare.Counts()
	return compared
}

func (e *Engine) compareMismatches() int64 {
	if e == nil || e.opts.Compare == nil {
		return 0
	}
	_, mismatches := e.opts.Compare.Counts()
	return mismatches
}

func (e *Engine) pendingCompare() int {
	if e == nil || e.opts.Compare == nil {
		return 0
	}
	return e.opts.Compare.Pending()
}

func (e *Engine) modeFor(event upstream.Event) string {
	if e.opts.ModeFor != nil {
		return e.opts.ModeFor(e.opts.Flow.Definition.ID, event.Subject.Project)
	}
	if e.opts.ModeEnabled != nil && !e.opts.ModeEnabled(e.opts.Flow.Definition.ID) {
		return mode.Off
	}
	return mode.Shadow
}

func recordFor(event upstream.Event) *store.Record {
	return &store.Record{
		Kind: "POST", Event: event.Kind, IssueID: event.Subject.Key,
		ProjectKey: event.Subject.Project, PrimaryActor: event.Actor.Login,
		Accepted: true, Time: event.ReceivedAt, RawBody: string(event.Payload),
	}
}

func viewOf(task registry.Task) rules.TaskView {
	return rules.TaskView{
		Known: task.Key != "", Plan: task.Plan, State: task.State, Turns: task.Turns,
		LastReply: task.LastReply, Refusals: task.Refusals,
	}
}

func (e *Engine) ensureLive(ctx context.Context, live *liveExecution, event upstream.Event, next string) (stepOutcome, error) {
	match, ok := e.opts.Projects.Match(event.Upstream, event.Subject.Project)
	if !ok {
		return stepOutcome{state: StepSucceeded, output: map[string]any{
			"task_id": event.Subject.Key, "routable": false,
			"project": event.Subject.Project, "runtime": "", "reused": false,
		}, next: next}, nil
	}
	host, err := e.opts.TaskSelector.Select(ctx, live.record, match.Entry)
	if err != nil {
		return stepOutcome{}, fmt.Errorf("select a runtime: %w", err)
	}
	_, found := e.task(event)
	task, err := e.opts.TaskService.Ensure(ctx, tasksvc.EnsureRequest{
		Record: live.record, Delivery: neutralEvent(event), Entry: match.Entry, Host: host,
	})
	if err != nil {
		return stepOutcome{}, fmt.Errorf("ensure the task: %w", err)
	}
	live.entry = match.Entry
	live.host = host
	live.task = task
	return stepOutcome{state: StepSucceeded, output: map[string]any{
		"task_id": task.Key, "routable": true, "project": event.Subject.Project,
		"runtime": host.Name, "reused": found,
	}, next: next}, nil
}

func (e *Engine) turnLive(ctx context.Context, live *liveExecution, node *flow.CompiledNode, event upstream.Event, next string) (stepOutcome, error) {
	config := node.Config.(*flow.TurnConfig)
	if live.task.Key == "" {
		return stepOutcome{}, fmt.Errorf("task.turn %q has no ensured task", node.Node.ID)
	}
	action := rules.Action(config.Action)
	if config.Action == "last" {
		action = rules.Action(strings.TrimSpace(live.task.LastAction))
	}
	result, err := e.opts.TaskService.Turn(ctx, tasksvc.TurnRequest{
		Record: live.record, Delivery: neutralEvent(event), Task: live.task,
		Entry: live.entry, Host: live.host, Action: action, Trigger: live.trigger,
		RequireReply: config.RequireReply, Basis: live.basis,
	})
	if err != nil {
		return stepOutcome{}, fmt.Errorf("turn %q: %w", node.Node.ID, err)
	}
	if result.Task.Key != "" {
		live.task = result.Task
	}
	if config.RequireReply && !result.Replied {
		return stepOutcome{}, fmt.Errorf("turn %q finished without posting a comment", node.Node.ID)
	}
	return stepOutcome{state: StepSucceeded, output: map[string]any{
		"session_id": result.SessionID, "reply": result.Text,
		"cost": result.Cost, "tokens": result.Tokens,
		"timed_out": result.TimedOut, "replied": result.Replied,
		"refusals": refusals(result.Refusals), "action": string(result.Action),
	}, next: next}, nil
}

func (e *Engine) grantLive(ctx context.Context, live *liveExecution, node *flow.CompiledNode, next string) (stepOutcome, error) {
	config := node.Config.(*flow.GrantConfig)
	var commands []string
	if config.Mode != "refusals" {
		commands = config.List
	}
	result, err := e.opts.TaskService.Grant(ctx, tasksvc.GrantRequest{
		Record: live.record, Task: live.task, Entry: live.entry, Commands: commands, Trigger: live.basis,
	})
	if err != nil {
		return stepOutcome{}, fmt.Errorf("grant %q: %w", node.Node.ID, err)
	}
	if result.Task.Key != "" {
		live.task = result.Task
	}
	granted := []string{}
	actor := ""
	if live.task.Grant != nil {
		granted = append(granted, live.task.Grant.Commands...)
		actor = live.task.Grant.By
	}
	return stepOutcome{state: StepSucceeded, output: map[string]any{
		"granted": granted, "actor": actor, "action": string(result.Action),
	}, next: next}, nil
}

func (e *Engine) revokeLive(ctx context.Context, live *liveExecution, next string) (stepOutcome, error) {
	if err := e.opts.TaskService.Revoke(ctx, tasksvc.RevokeRequest{Record: live.record, Task: live.task}); err != nil {
		return stepOutcome{}, fmt.Errorf("revoke: %w", err)
	}
	if task, ok := e.task(taskEvent(live.record)); ok {
		live.task = task
	}
	return stepOutcome{state: StepSucceeded, output: map[string]any{"revoked": true}, next: next}, nil
}

func taskEvent(rec *store.Record) upstream.Event {
	if rec == nil {
		return upstream.Event{}
	}
	return upstream.Event{
		Upstream: "youtrack", Kind: rec.Event,
		Subject: upstream.Subject{Key: rec.IssueID, Project: rec.ProjectKey},
		Actor:   upstream.Actor{Login: rec.PrimaryActor},
	}
}

func (e *Engine) failStep(run *Run, step *Step, cause error) (*Run, error) {
	now := e.now().UTC()
	step.finish(StepFailed, now)
	step.Error = cause.Error()
	run.Error = cause.Error()
	run.finish(StateFailed, now)
	_ = e.store.Append(run.ID, "step.failed", map[string]any{"step": step.ID, "error": cause.Error()})
	_ = e.store.Append(run.ID, "run.finished", map[string]any{"state": StateFailed, "error": cause.Error()})
	_ = e.store.Save(run)
	return run, cause
}

func (e *Engine) failRun(run *Run, stepID string, cause error) (*Run, error) {
	now := e.now().UTC()
	run.Error = cause.Error()
	run.finish(StateFailed, now)
	_ = e.store.Append(run.ID, "run.finished", map[string]any{"state": StateFailed, "step": stepID, "error": cause.Error()})
	_ = e.store.Save(run)
	return run, cause
}

func (e *Engine) task(event upstream.Event) (registry.Task, bool) {
	if e.opts.Tasks == nil {
		return registry.Task{}, false
	}
	return e.opts.Tasks.Get(event.Upstream, event.Subject.Key)
}

func (e *Engine) projections(event upstream.Event) (map[string]any, map[string]rules.TaskView) {
	tasks := map[string]any{}
	views := map[string]rules.TaskView{}
	for name := range e.opts.Flow.Definition.Tasks {
		task, found := e.task(event)
		plan := string(task.Plan)
		if plan == "" {
			plan = string(registry.PlanNone)
		}
		state := string(task.State)
		tasks[name] = map[string]any{
			"state":       state,
			"plan":        plan,
			"session":     task.SessionID,
			"worktree":    task.Worktree,
			"last_reply":  task.LastReply,
			"last_action": task.LastAction,
			"refusals":    refusals(task.Refusals),
			"grant":       grant(task.Grant),
		}
		views[name] = rules.TaskView{
			State:     task.State,
			Known:     found,
			Plan:      task.Plan,
			Turns:     task.Turns,
			LastReply: task.LastReply,
			Refusals:  task.Refusals,
		}
	}
	return tasks, views
}

func refusals(values []registry.Refusal) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, map[string]any{"command": value.Command, "reason": value.Reason})
	}
	return out
}

func grant(value *registry.Grant) any {
	if value == nil {
		return nil
	}
	return map[string]any{"commands": value.Commands, "by": value.By, "at": value.At}
}

func (e *Engine) context(run *Run, event upstream.Event, steps map[string]map[string]any, tasks map[string]any) (map[string]any, error) {
	trigger, err := eventMap(event)
	if err != nil {
		return nil, err
	}
	stepValues := make(map[string]any, len(steps))
	for id, step := range steps {
		stepValues[id] = step
	}
	return map[string]any{
		"trigger": trigger,
		"run": map[string]any{
			"id":         run.ID,
			"started_at": run.StartedAt.Format(time.RFC3339Nano),
		},
		"params": map[string]any{},
		"steps":  stepValues,
		"tasks":  tasks,
	}, nil
}

func eventMap(event upstream.Event) (map[string]any, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("engine: encode event: %w", err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("engine: decode event context: %w", err)
	}
	return value, nil
}

func (e *Engine) triggerFor(event upstream.Event) *flow.CompiledTrigger {
	var chosen *flow.CompiledTrigger
	for index := range e.opts.Flow.Triggers {
		trigger := &e.opts.Flow.Triggers[index]
		if !eventMatches(trigger.Trigger.Event, event.Kind) {
			continue
		}
		if trigger.When != nil {
			context, err := eventMap(event)
			if err != nil {
				e.log.Error("trigger context failed", "event", event.ID, "error", err)
				continue
			}
			ok, err := trigger.When.Bool(map[string]any{"trigger": context})
			if err != nil {
				e.log.Error("trigger condition failed", "event", event.ID, "error", err)
				continue
			}
			if !ok {
				continue
			}
		}
		if chosen == nil || trigger.Trigger.Priority > chosen.Trigger.Priority {
			chosen = trigger
		}
	}
	return chosen
}

func eventMatches(pattern, kind string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(kind, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == kind
}

func selectEdge(node *flow.CompiledNode, context map[string]any) (string, error) {
	for _, edge := range node.Next {
		if edge.When == nil {
			return edge.Edge.To, nil
		}
		ok, err := edge.When.Bool(context)
		if err != nil {
			return "", fmt.Errorf("edge %s -> %s: %w", edge.Edge.From, edge.Edge.To, err)
		}
		if ok {
			return edge.Edge.To, nil
		}
	}
	return "", fmt.Errorf("branch %q selected no edge", node.Node.ID)
}

func routableFromSteps(context map[string]any) bool {
	steps, ok := context["steps"].(map[string]any)
	if !ok {
		return false
	}
	for _, value := range steps {
		step, ok := value.(map[string]any)
		if !ok {
			continue
		}
		output, ok := step["output"].(map[string]any)
		if !ok {
			continue
		}
		if routable, ok := output["routable"].(bool); ok {
			return routable
		}
	}
	return false
}

func neutralEvent(value upstream.Event) event.Event {
	kind := event.KindOther
	state := ""
	stateField := ""
	switch value.Kind {
	case upstream.KindIssueCreated:
		kind = event.KindCreated
	case upstream.KindCommentAdded:
		kind = event.KindCommented
	case upstream.KindIssueDeleted:
		kind = event.KindDeleted
	case upstream.KindIssueUpdated:
		kind = event.KindUpdated
		for _, change := range value.Changes {
			if strings.EqualFold(change.Field, "state") {
				kind = event.KindStateSet
				state = change.To
				stateField = change.Field
				break
			}
		}
	}
	comment := ""
	if value.Comment != nil {
		comment = value.Comment.Text
	}
	return event.Event{
		Source: value.Upstream,
		Kind:   kind,
		Subject: event.Subject{
			Project: value.Subject.Project,
			Key:     value.Subject.Key,
			Title:   value.Subject.Title,
			Body:    value.Subject.Body,
			URL:     value.Subject.URL,
		},
		Actor:      value.Actor.Login,
		State:      state,
		StateField: stateField,
		Comment:    comment,
		Occurred:   value.OccurredAt,
		Raw:        value.Payload,
	}
}
