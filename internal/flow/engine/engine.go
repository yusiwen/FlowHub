// Package engine executes the builtin flow in shadow mode.
//
// A shadow run resolves the task projection, evaluates the upstream policy and
// walks the graph, but every node that would touch a workspace, a session or a
// runtime is recorded as skipped. It is the migration instrument: the decision it
// produces for a real event can be compared with internal/dispatch without either
// side affecting the other.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/flow"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
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
}

// Engine consumes events and writes shadow runs.
type Engine struct {
	opts  Options
	log   *slog.Logger
	store *Store
	now   func() time.Time

	queue chan upstream.Event
	done  chan struct{}
	wg    sync.WaitGroup

	mu     sync.Mutex
	closed bool
	seen   map[string]bool

	submitted atomic.Int64
	dropped   atomic.Int64
	runs      atomic.Int64
	failed    atomic.Int64
}

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
	return &Engine{
		opts:  opts,
		log:   opts.Logger,
		store: store,
		now:   opts.Now,
		queue: make(chan upstream.Event, opts.QueueSize),
		done:  make(chan struct{}),
		seen:  map[string]bool{},
	}, nil
}

// Start runs the single shadow worker until ctx is done or Close is called.
func (e *Engine) Start(ctx context.Context) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-e.queue:
				if !ok {
					return
				}
				if _, err := e.RunEvent(event); err != nil {
					e.failed.Add(1)
					e.log.Error("shadow run failed", "event", event.ID, "error", err)
				}
			}
		}
	}()
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
	select {
	case e.queue <- event:
	default:
		e.dropped.Add(1)
	}
}

// Close stops the worker and drains the queue. It is idempotent.
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
	close(e.queue)
	e.mu.Unlock()
	e.wg.Wait()
	close(e.done)
	return nil
}

// Snapshot is the /healthz projection for the shadow engine.
func (e *Engine) Snapshot() map[string]any {
	if e == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":   true,
		"mode":      "shadow",
		"flow":      e.opts.Flow.Definition.ID,
		"submitted": e.submitted.Load(),
		"dropped":   e.dropped.Load(),
		"runs":      e.runs.Load(),
		"failed":    e.failed.Load(),
		"dir":       e.store.Dir(),
	}
}

// RunEvent executes one event synchronously. It is exported for the parity
// harness; the worker uses it too.
func (e *Engine) RunEvent(event upstream.Event) (*Run, error) {
	trigger := e.triggerFor(event)
	if trigger == nil {
		return nil, nil
	}
	key := e.opts.Flow.Definition.ID + "\x00" + trigger.Trigger.ID + "\x00" + event.ID
	e.mu.Lock()
	if e.seen[key] {
		e.mu.Unlock()
		return nil, nil
	}
	e.seen[key] = true
	e.mu.Unlock()

	run, err := e.runShadow(event, trigger)
	if err != nil {
		e.failed.Add(1)
		return run, err
	}
	e.runs.Add(1)
	return run, nil
}

type shadowStep struct {
	state    StepState
	output   map[string]any
	next     string
	finished bool
	runState State
}

func (e *Engine) runShadow(event upstream.Event, trigger *flow.CompiledTrigger) (*Run, error) {
	now := e.now().UTC()
	revision := sha256.Sum256(e.opts.Definition)
	run := &Run{
		ID:        newRunID(now),
		Flow:      e.opts.Flow.Definition.ID,
		Revision:  hex.EncodeToString(revision[:]),
		Mode:      "shadow",
		State:     StateRunning,
		Trigger:   trigger.Trigger.ID,
		Event:     event,
		StartedAt: now,
		Steps:     map[string]*Step{},
	}
	if err := e.store.Create(run); err != nil {
		return run, err
	}
	if err := e.store.Append(run.ID, "run.started", map[string]any{
		"flow": run.Flow, "trigger": run.Trigger, "event_id": event.ID,
	}); err != nil {
		return run, err
	}
	if err := e.store.Save(run); err != nil {
		return run, err
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
		outcome, err := e.execute(node, event, context, views)
		if err != nil {
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
			return run, nil
		}
		if outcome.next == "" {
			return e.failRun(run, current, fmt.Errorf("node %q has no selected successor", current))
		}
		current = outcome.next
	}
	return e.failRun(run, "", fmt.Errorf("the graph reached no end node"))
}

func (e *Engine) execute(node *flow.CompiledNode, event upstream.Event, context map[string]any, views map[string]rules.TaskView) (shadowStep, error) {
	next := ""
	if len(node.Next) > 0 {
		next = node.Next[0].Edge.To
	}
	switch flow.NodeType(node.Node.Type) {
	case flow.NodeStart:
		return shadowStep{state: StepSucceeded, next: next}, nil
	case flow.NodeEnsure:
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
		return shadowStep{state: StepSucceeded, output: output, next: next}, nil
	case flow.NodeEvaluate:
		config := node.Config.(*flow.EvaluateConfig)
		if !routableFromSteps(context) {
			return shadowStep{state: StepSucceeded, output: map[string]any{
				"action": "ignore",
				"basis":  "routing",
				"reason": fmt.Sprintf("no routing entry for %s/%s", event.Upstream, event.Subject.Project),
			}, next: next}, nil
		}
		decision := e.opts.Policy.Decide(neutralEvent(event), views[config.Task])
		return shadowStep{state: StepSucceeded, output: map[string]any{
			"action": string(decision.Action),
			"basis":  string(decision.Trigger),
			"reason": decision.Reason,
		}, next: next}, nil
	case flow.NodeBranch:
		selected, err := selectEdge(node, context)
		if err != nil {
			return shadowStep{}, err
		}
		return shadowStep{state: StepSucceeded, output: map[string]any{"selected": selected}, next: selected}, nil
	case flow.NodeTurn, flow.NodeGrant, flow.NodeRevoke:
		// Shadow never touches a workspace, a session or a registry row.
		return shadowStep{state: StepSkipped, next: next}, nil
	case flow.NodeEnd:
		config := node.Config.(*flow.EndConfig)
		state := StateSucceeded
		if config.State == "failed" {
			state = StateFailed
		}
		return shadowStep{state: StepSucceeded, finished: true, runState: state}, nil
	default:
		return shadowStep{}, fmt.Errorf("node %q: type %q has no shadow executor", node.Node.ID, node.Node.Type)
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
