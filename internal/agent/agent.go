// Package agent is the seam between FlowHub and whatever drives one unattended
// turn.
//
// ADR 0001 chose in-process interfaces over plugins, so a runtime is a package
// that implements Runtime and is wired in `main`. Routing, the registry and the
// audit know only the types here: which product runs a turn, how that product
// spells a permission, and what its wire protocol looks like all stay behind the
// adapter.
//
// Two properties are deliberately the runtime's rather than the dispatcher's:
//
//   - the session ruleset ("which permissions a session starts with") belongs to
//     the product's permission model, while "which tools this source needs" belongs
//     to the source. A Turn therefore carries the source's allowlist as *data* and
//     the runtime builds the ruleset from it;
//   - the shell policy stays inside the adapter, because a source must not be able
//     to widen what an unattended turn may execute.
package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Phase is the lifecycle position of a turn.
//
// The same session runs the read-only analysis and the execution turn, so a
// session ruleset cannot express the difference: the ruleset is fixed at session
// creation and would either deny edits forever or allow them from the start. The
// runtime therefore applies its own phase policy while answering permission
// requests, which only works because the gated permission is "ask" — "allow" would
// bypass the decision and "deny" would remove the tool altogether.
type Phase string

const (
	// PhaseAnalysis is read-only: the agent inspects the issue and the repository
	// and must post its findings.
	PhaseAnalysis Phase = "analysis"
	// PhaseExecution may change the task's workspace.
	PhaseExecution Phase = "execution"
)

// Downloads is the attachment-download policy a runtime enforces for one turn.
// The values come from the source's ToolPolicy, because which host serves a
// tracker's attachments is the tracker adapter's fact, not the runtime's.
type Downloads struct {
	// Hosts is the only set of hosts a download may reach. Empty disables
	// downloads entirely.
	Hosts []string
	// Prefix is the only directory inside the workspace a download may be written
	// to, relative to it.
	Prefix string
}

// Turn is one unattended turn.
type Turn struct {
	// Directory is the workspace the runtime binds the session to. It is opaque:
	// the dispatcher never stats, joins or globs it, because for a provider that
	// lives on another host it names a directory on somebody else's machine.
	Directory string
	// Prompt is the user text for this turn.
	Prompt string
	// Agent selects the runtime's agent profile; empty means the runtime default.
	Agent string
	// Model pins the model as "provider/model-id". Empty means the profile's own
	// default, which is the honest default: the profile should own the model it was
	// tuned for.
	Model string
	// Title is a human hint for the session, if the runtime has such a concept.
	Title string
	// SessionID continues an existing session. Empty means "create one".
	SessionID string
	// Phase decides what this turn may change.
	Phase Phase
	// AllowedTools are the source's tool names that may run without asking. The
	// runtime turns them into its own ruleset; everything absent stays gated.
	AllowedTools []string
	// Downloads constrains attachment downloads for this turn.
	Downloads Downloads
	// Metadata is free-form and stored with the session, if the runtime stores any.
	Metadata map[string]any
	// Deadline bounds the whole turn. A deadline is not a failure: the session may
	// still be running and can be inspected later.
	Deadline time.Duration
}

// Tokens is the token usage over one turn.
type Tokens struct {
	Input      int
	Output     int
	Reasoning  int
	Total      int
	CacheRead  int
	CacheWrite int
}

// ToolCall is one tool invocation observed in a turn. It is how a caller verifies
// a claim such as "I posted the analysis comment" instead of trusting the prose.
type ToolCall struct {
	Name   string
	Status string
	Input  string
	Output string
}

// PermissionDecision records one permission answer, for the audit trail.
type PermissionDecision struct {
	RequestID  string
	Permission string
	Command    string
	// Reply is the runtime's own answer word ("once", "always", "reject"), kept as
	// a string because the vocabulary belongs to the runtime.
	Reply  string
	Reason string
}

// Result is the outcome of one turn.
type Result struct {
	SessionID string
	// Text is the last assistant text produced by the turn.
	Text string
	// Finished reports whether the runtime observed the turn complete.
	Finished bool
	// TimedOut reports that the deadline elapsed before Finished.
	TimedOut bool
	// ErrorText is the last error the runtime saw, if the turn failed.
	ErrorText string
	Cost      float64
	Tokens    Tokens
	// Tools lists the tool calls the turn made.
	Tools []ToolCall
	// Permissions lists the permission requests the runtime answered.
	Permissions []PermissionDecision
	Elapsed     time.Duration
}

// Runtime drives turns on one host.
type Runtime interface {
	// Name is stable and unique among the runtimes a process talks to.
	Name() string
	// Health reports the version of the runtime behind this name, or an error if it
	// cannot take work.
	Health(ctx context.Context) (version string, err error)
	// Run executes one turn. A partial Result is returned alongside the error, so a
	// failed turn is still auditable.
	Run(ctx context.Context, turn Turn) (Result, error)
}

// SplitModel splits the "provider/model-id" spelling every runtime in this design
// uses for a model reference.
//
// It lives here rather than in an adapter because the spelling is the neutral
// vocabulary: configuration validates it, and provisioning compares it against
// what a host reported, neither of which should have to import a runtime product.
func SplitModel(model string) (provider, id string, ok bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", false
	}
	// Cut on the first slash: a model id may itself contain one (for example
	// "accounts/fireworks/models/llama"), a provider id may not.
	provider, id, found := strings.Cut(model, "/")
	provider, id = strings.TrimSpace(provider), strings.TrimSpace(id)
	if !found || provider == "" || id == "" {
		return "", "", false
	}
	return provider, id, true
}

// JoinModel is the inverse of SplitModel, for a runtime that reports the two halves
// separately.
func JoinModel(provider, id string) string {
	provider, id = strings.TrimSpace(provider), strings.TrimSpace(id)
	if provider == "" || id == "" {
		return ""
	}
	return provider + "/" + id
}

// ValidateModel reports whether a configured model reference can be honoured. A
// runtime takes a provider id and a model id separately, so a bare model name
// cannot be expressed and must be refused rather than silently ignored.
func ValidateModel(model string) error {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	if _, _, ok := SplitModel(model); !ok {
		return fmt.Errorf("model %q must be spelled provider/model-id", model)
	}
	return nil
}
