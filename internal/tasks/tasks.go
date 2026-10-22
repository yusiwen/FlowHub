// Package tasks is the execution seam between an orchestrator and one task.
//
// It owns the four operations that must not be reimplemented by the flow engine:
// ensure (routing/registry/workspace/session), one turn (prompt, runtime, reply
// contract, registry), grant and revoke. Dispatch implements it today; the flow
// engine consumes the interface, so enabling `on` does not duplicate the
// invariants that keep a task on one worktree, one session and one runtime.
package tasks

import (
	"context"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/store"
)

// Host is the runtime binding a task uses for its life. It is data, not a
// dispatch decision: the orchestrator chooses it once and the task service records
// that choice.
type Host struct {
	Name          string
	Runtime       agent.Runtime
	Agent         string
	Model         string
	Deadline      time.Duration
	MaxConcurrent int
}

// EnsureRequest creates or finds the task for one delivery.
type EnsureRequest struct {
	// Record is the audit record for the delivery. The service uses its issue id,
	// project and actor; the raw body stays in the record.
	Record *store.Record
	// Delivery is the decoded neutral event.
	Delivery event.Event
	Entry    *projectmap.Entry
	Host     Host
}

// TurnRequest runs one turn on a task that Ensure has prepared.
type TurnRequest struct {
	Record       *store.Record
	Delivery     event.Event
	Task         registry.Task
	Entry        *projectmap.Entry
	Host         Host
	Action       rules.Action
	RequireReply bool
	// Basis is the human-readable trigger reason the reply sign-off states. It
	// comes from the flow's evaluate step, never from the model.
	Basis string
}

// TurnResult is what a turn recorded. The task field is the row after the update.
type TurnResult struct {
	Task      registry.Task
	SessionID string
	Text      string
	Finished  bool
	TimedOut  bool
	Replied   bool
	Cost      float64
	Tokens    agent.Tokens
	Refusals  []registry.Refusal
	Action    rules.Action
}

// GrantRequest records the commands the most recent turn was refused for. An empty
// Commands means "all recorded refusals".
type GrantRequest struct {
	Record   *store.Record
	Task     registry.Task
	Entry    *projectmap.Entry
	Commands []string
	Trigger  string
}

// GrantResult is the updated task and the action the continuation should use.
type GrantResult struct {
	Task   registry.Task
	Action rules.Action
}

// RevokeRequest clears a grant.
type RevokeRequest struct {
	Record *store.Record
	Task   registry.Task
}

// Service is implemented by dispatch today and by the extracted task service in
// M3-1. The flow engine imports this package, never dispatch.
type Service interface {
	Ensure(ctx context.Context, request EnsureRequest) (registry.Task, error)
	Turn(ctx context.Context, request TurnRequest) (TurnResult, error)
	Grant(ctx context.Context, request GrantRequest) (GrantResult, error)
	Revoke(ctx context.Context, request RevokeRequest) error
}

// Selector chooses the runtime binding for a new task. It is separate from the
// service because dispatch's routing policy (inventory, eligibility, spread,
// capacity) stays where it is until M3-1.
type Selector interface {
	Select(ctx context.Context, record *store.Record, entry *projectmap.Entry) (Host, error)
}
