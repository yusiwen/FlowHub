// Package runner holds the parts of one unattended turn that are independent of
// the event source: which host runs it, one turn's request, the workspace-provider
// cache, and the per-runtime capacity.
//
// The dispatcher keeps its rules, prompt, reply contract and registry policy on top
// of this package; the flow engine will call the same runner. The split exists so
// the two orchestrators cannot drift on the things that must stay identical: the
// agent/model/deadline a turn carries, the one-workspace-per-task rule, and the
// per-host breadth.
package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
)

// Host is one runtime with the policy a turn on it uses.
type Host struct {
	Name    string
	Runtime agent.Runtime
	// Agent is the profile a turn runs under; empty means the runtime's default.
	Agent string
	// Model is the pinned "provider/model-id"; empty leaves the profile's default.
	Model string
	// Deadline bounds a turn when the request does not name its own.
	Deadline time.Duration
	// MaxConcurrent is the host's breadth, for a shared Capacity.
	MaxConcurrent int
}

// Turn is one request to a host.
type Turn struct {
	Directory string
	Prompt    string
	Title     string
	SessionID string
	Phase     agent.Phase
	// Agent and Model override the host's values when non-empty. The dispatcher
	// resolves them from the routing entry before the call; a flow step may pin a
	// different profile for one step.
	Agent string
	Model string
	// AllowedTools is the source's allowlist, data only. The runtime builds its own
	// session ruleset from it.
	AllowedTools []string
	Downloads    agent.Downloads
	Granted      []string
	Metadata     map[string]any
	// Deadline overrides the host's when non-zero.
	Deadline time.Duration
}

// Run executes one turn and returns the runtime's result. It does not retry, does
// not inspect the reply and does not touch the task registry: those are the
// caller's policy.
func (h Host) Run(ctx context.Context, turn Turn) (agent.Result, error) {
	if h.Runtime == nil {
		return agent.Result{}, fmt.Errorf("runner: %s: no runtime", h.Name)
	}
	agentName := h.Agent
	if turn.Agent != "" {
		agentName = turn.Agent
	}
	model := h.Model
	if turn.Model != "" {
		model = turn.Model
	}
	deadline := turn.Deadline
	if deadline <= 0 {
		deadline = h.Deadline
	}
	return h.Runtime.Run(ctx, agent.Turn{
		Directory:    turn.Directory,
		Prompt:       turn.Prompt,
		Agent:        agentName,
		Model:        model,
		Title:        turn.Title,
		SessionID:    turn.SessionID,
		Phase:        turn.Phase,
		AllowedTools: turn.AllowedTools,
		Downloads:    turn.Downloads,
		Granted:      turn.Granted,
		Metadata:     turn.Metadata,
		Deadline:     deadline,
	})
}
