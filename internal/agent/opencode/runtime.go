package opencode

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
)

// Runtime is one opencode server, addressed by name, implementing agent.Runtime.
//
// It is a thin adapter over Client and Runner: the client speaks the HTTP API, the
// runner drives one turn, and this type supplies what the seam requires of a
// runtime — its name, a liveness/version answer, and the translation between the
// neutral Turn/Result and this product's own types.
//
// The two policies that belong to this product live here rather than in the
// dispatcher (ADR 0001): the session ruleset ("which permissions a session starts
// with") and the phase-aware arbiter. The dispatcher passes the *source's* tool
// allowlist and download policy as data and never learns how either is expressed.
type Runtime struct {
	name   string
	client *Client
	log    *slog.Logger
	// FirstResponse bounds how long a turn may take to produce its first assistant
	// message. Zero uses the runner's default, capped at a third of the deadline.
	FirstResponse time.Duration
}

// RuntimeOptions configures a Runtime.
type RuntimeOptions struct {
	// Name is the runtime's configured name, which is what the log and the
	// registry record. Empty falls back to the product name.
	Name string
	// Log receives the turn's own messages. Nil uses slog.Default().
	Log *slog.Logger
	// FirstResponse is the bound described on Runtime.FirstResponse.
	FirstResponse time.Duration
}

// NewRuntime builds a runtime for one server.
func NewRuntime(client *Client, opts RuntimeOptions) *Runtime {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = "opencode"
	}
	return &Runtime{name: name, client: client, log: log, FirstResponse: opts.FirstResponse}
}

// Name implements agent.Runtime.
func (r *Runtime) Name() string { return r.name }

// Client exposes the HTTP client, for the callers that need more than a turn: the
// enrolment and activation checks read the agent registry and the model catalogue.
func (r *Runtime) Client() *Client { return r.client }

// Health implements agent.Runtime: the server's version, or an error if it cannot
// be reached.
//
// The probe stays transport-level on purpose. The server's own `healthy` flag is
// not consulted, because it never was: a runtime that answers the health endpoint
// is routable to in this design, and a turn that fails anyway records its own
// error. Tightening this is a routing decision, not part of the seam.
func (r *Runtime) Health(ctx context.Context) (string, error) {
	health, err := r.client.Health(ctx)
	if err != nil {
		return "", fmt.Errorf("opencode: %s: %w", r.name, err)
	}
	return health.Version, nil
}

// Run implements agent.Runtime: one unattended turn on this server.
func (r *Runtime) Run(ctx context.Context, turn agent.Turn) (agent.Result, error) {
	// The ruleset is the first permission layer and its ORDER is load bearing: the
	// session ruleset is an array evaluated last-match-wins, so the catch-all has to
	// come first and the specific entries after it.
	ruleset := sessionRuleset(turn.AllowedTools)

	arbiter := NewAnalysisArbiter()
	if turn.Phase == agent.PhaseExecution {
		arbiter = NewExecutionArbiter()
	}
	arbiter.AllowTools = toolSet(turn.AllowedTools)
	// The attachment exception is the source's policy, applied by this runtime's
	// arbiter: which host serves a tracker's attachments is the adapter's fact.
	arbiter.CurlHosts = append([]string(nil), turn.Downloads.Hosts...)
	arbiter.CurlOutputPrefix = turn.Downloads.Prefix

	runner := NewRunner(r.client, arbiter, r.log)
	runner.FirstResponse = r.FirstResponse

	result, err := runner.Run(ctx, Task{
		Directory: turn.Directory,
		Prompt:    turn.Prompt,
		Agent:     turn.Agent,
		Model:     turn.Model,
		Title:     turn.Title,
		SessionID: turn.SessionID,
		Ruleset:   ruleset,
		Metadata:  turn.Metadata,
		Deadline:  turn.Deadline,
	})
	return projectResult(result), err
}

// projectResult renders this product's result as the neutral one.
func projectResult(result Result) agent.Result {
	tools := make([]agent.ToolCall, 0, len(result.Tools))
	for _, call := range result.Tools {
		tools = append(tools, agent.ToolCall{
			Name: call.Name, Status: call.Status, Input: call.Input, Output: call.Output,
		})
	}
	permissions := make([]agent.PermissionDecision, 0, len(result.Permissions))
	for _, answered := range result.Permissions {
		permissions = append(permissions, agent.PermissionDecision{
			RequestID:  answered.RequestID,
			Permission: answered.Permission,
			Command:    answered.Command,
			Reply:      string(answered.Reply),
			Reason:     answered.Reason,
		})
	}
	return agent.Result{
		SessionID: result.SessionID,
		Text:      result.Text,
		Finished:  result.Finished,
		TimedOut:  result.TimedOut,
		ErrorText: result.Error,
		Cost:      result.Cost,
		Tokens: agent.Tokens{
			Input:      result.Tokens.Input,
			Output:     result.Tokens.Output,
			Reasoning:  result.Tokens.Reasoning,
			Total:      result.Tokens.Total,
			CacheRead:  result.Tokens.Cache.Read,
			CacheWrite: result.Tokens.Cache.Write,
		},
		Tools:       tools,
		Permissions: permissions,
		Elapsed:     result.Elapsed,
	}
}

// sessionRuleset is the first permission layer.
//
// The catch-all comes first on purpose: the session ruleset is an array evaluated
// last-match-wins, so a trailing catch-all would override every specific entry
// (measured: a trailing "*": ask made an allowed tool ask again). Everything the
// specific entries do not permit still reaches the arbiter as an `ask`.
func sessionRuleset(allowedTools []string) []PermissionRule {
	ruleset := []PermissionRule{
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
	for _, tool := range allowedTools {
		ruleset = append(ruleset, PermissionRule{Permission: tool, Pattern: "*", Action: "allow"})
	}
	return ruleset
}

// toolSet turns a source's allowlist into the arbiter's lookup map.
func toolSet(tools []string) map[string]bool {
	if len(tools) == 0 {
		return nil
	}
	set := make(map[string]bool, len(tools))
	for _, tool := range tools {
		set[tool] = true
	}
	return set
}
