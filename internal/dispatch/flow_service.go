package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/store"
	"github.com/yusiwen/flowhub/internal/tasks"
)

// TaskService exposes the execution half of dispatch to another orchestrator. The
// flow engine consumes tasks.Service; dispatch still owns the policy and the
// scheduler, and this adapter is the seam that lets `on` run without duplicating
// the worktree/session/registry invariants. The service itself moves out of this
// package in M3-1.
func (d *Dispatcher) TaskService() tasks.Service {
	if d == nil {
		return nil
	}
	return flowTasks{d: d}
}

// TaskSelector exposes runtime selection (inventory, eligibility, spread, capacity)
// to the flow engine. It stays in dispatch until M3-1 because it is the routing
// policy, not the task lifecycle.
func (d *Dispatcher) TaskSelector() tasks.Selector {
	if d == nil {
		return nil
	}
	return flowSelector{d: d}
}

type flowSelector struct {
	d *Dispatcher
}

func (s flowSelector) Select(ctx context.Context, rec *store.Record, entry *projectmap.Entry) (tasks.Host, error) {
	if s.d == nil {
		return tasks.Host{}, fmt.Errorf("dispatch: no task service is available")
	}
	task, _ := s.d.opts.Registry.Get(s.d.opts.Source.Name(), rec.IssueID)
	binding, err := s.d.pickRuntime(ctx, task, entry)
	if err != nil {
		return tasks.Host{}, err
	}
	// The flow engine uses the same capacity gate as dispatch. A scheduler may not
	// exist yet (intake is skipped for this project), so set the limit explicitly.
	s.d.capacity.SetLimit(binding.Name, binding.maxConcurrent())
	return s.d.flowHost(entry, binding), nil
}

type flowTasks struct {
	d *Dispatcher
}

func (s flowTasks) binding(host tasks.Host) (runtimeBinding, error) {
	binding, ok := s.d.bindingFor(host.Name)
	if !ok {
		return runtimeBinding{}, fmt.Errorf("runtime %q is no longer configured", host.Name)
	}
	return binding, nil
}

func (s flowTasks) Ensure(ctx context.Context, request tasks.EnsureRequest) (registry.Task, error) {
	binding, err := s.binding(request.Host)
	if err != nil {
		return registry.Task{}, err
	}
	agentName := request.Host.Agent
	if strings.TrimSpace(agentName) == "" {
		agentName = runtimeAgent(request.Entry, binding, s.d.opts.Agent)
	}
	task, known := s.d.opts.Registry.Get(s.d.opts.Source.Name(), request.Record.IssueID)
	return s.d.ensureTask(ctx, request.Record, request.Entry, task, known, binding, agentName)
}

func (s flowTasks) Turn(ctx context.Context, request tasks.TurnRequest) (tasks.TurnResult, error) {
	binding, err := s.binding(request.Host)
	if err != nil {
		return tasks.TurnResult{}, err
	}
	action := request.Action
	if string(action) == "last" {
		action = lastAction(request.Task)
	}
	switch action {
	case rules.ActionAnalyze, rules.ActionPlan, rules.ActionExecute:
	default:
		return tasks.TurnResult{}, fmt.Errorf("task %s has no action to continue (recorded %q)", request.Record.IssueID, request.Task.LastAction)
	}
	decision := rules.Decision{
		Action:  action,
		Trigger: "flow",
		Reason:  request.Basis,
	}
	outcome := s.d.runTurn(ctx, request.Record, request.Delivery, request.Task,
		projectmap.Match{Entry: request.Entry}, decision, binding)
	updated, _ := s.d.opts.Registry.Get(s.d.opts.Source.Name(), request.Record.IssueID)
	return tasks.TurnResult{
		Task: updated, SessionID: outcome.SessionID, Text: outcome.Text,
		Finished: outcome.Finished, TimedOut: outcome.TimedOut, Replied: outcome.Replied,
		Cost: outcome.Cost, Tokens: outcome.Tokens, Refusals: outcome.Refusals, Action: outcome.Action,
	}, nil
}

func (s flowTasks) Grant(_ context.Context, request tasks.GrantRequest) (tasks.GrantResult, error) {
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
				return tasks.GrantResult{}, fmt.Errorf("command %q was not refused by the most recent turn", command)
			}
			if !known[command] {
				known[command] = true
				granted = append(granted, command)
			}
		}
		updated, err := s.d.opts.Registry.Update(s.d.opts.Source.Name(), request.Record.IssueID, func(t *registry.Task) {
			t.Grant = &registry.Grant{Commands: granted, By: request.Record.PrimaryActor, At: time.Now().UTC()}
		})
		if err != nil {
			return tasks.GrantResult{}, err
		}
		return tasks.GrantResult{Task: updated, Action: lastAction(updated)}, nil
	}
	action, updated := s.d.grantPermit(request.Record, task, rules.Decision{
		Trigger: rules.Trigger(request.Trigger), Reason: "flow permit",
	})
	return tasks.GrantResult{Task: updated, Action: action.Action}, nil
}

func (s flowTasks) Revoke(_ context.Context, request tasks.RevokeRequest) error {
	s.d.revokeGrant(request.Record, request.Task)
	return nil
}

func (d *Dispatcher) flowHost(entry *projectmap.Entry, binding runtimeBinding) tasks.Host {
	agentName := runtimeAgent(entry, binding, d.opts.Agent)
	model := strings.TrimSpace(entry.Model)
	if model == "" {
		model = binding.modelFor(agentName)
	}
	if model == "" {
		model = binding.declaredModel()
	}
	return tasks.Host{
		Name:          binding.Name,
		Runtime:       binding.Runtime,
		Agent:         agentName,
		Model:         model,
		Deadline:      binding.deadline(d.opts.Deadline),
		MaxConcurrent: binding.maxConcurrent(),
	}
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

var _ tasks.Service = flowTasks{}
var _ tasks.Selector = flowSelector{}
