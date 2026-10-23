package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/store"
	"github.com/yusiwen/flowhub/internal/tasks"
)

// TaskService exposes the extracted task lifecycle to another orchestrator. The
// flow engine consumes tasks.Service; dispatch still owns the policy and scheduler.
func (d *Dispatcher) TaskService() tasks.Service {
	if d == nil {
		return nil
	}
	return d.taskExec
}

// TaskSelector exposes runtime selection (inventory, eligibility, spread, capacity)
// to the flow engine. It stays in dispatch because it is the routing policy, not
// the task lifecycle.
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

var _ tasks.Selector = flowSelector{}
