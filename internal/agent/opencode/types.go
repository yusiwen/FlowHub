package opencode

import "encoding/json"

// Health is the liveness answer: /global/health on a v1 server, /api/info on a
// v2 one. A v2 server has no `healthy` flag — a 200 with a version is the whole
// answer — so Healthy is synthesized by the v2 transport.
type Health struct {
	Healthy bool   `json:"healthy"`
	Version string `json:"version"`
}

// Status is one entry of /session/status (v1) or /api/session/active (v2).
type Status struct {
	Type string `json:"type"`
}

// Busy reports whether a status means the turn is still running. A pending
// permission also reports busy, so this is not a completion signal on its own.
func (s Status) Busy() bool { return s.Type == "busy" || s.Type == "retry" }

// CreateSessionRequest is the POST /session (v1) or /api/session (v2) body.
//
// Field names are not uniform across the API: the session takes
// {providerID, id} while a prompt takes {providerID, modelID}. Leaving Model nil
// lets the agent's own default apply, which is what the devops agent should own.
//
// Exactly one ruleset field applies: Permission for a v1 server, Permissions for
// a v2 one. The transport refuses the other, so a runtime that built the wrong
// one fails loudly instead of creating a session with no ruleset at all.
type CreateSessionRequest struct {
	Title       string             `json:"title,omitempty"`
	Agent       string             `json:"agent,omitempty"`
	Model       *SessionModel      `json:"model,omitempty"`
	Permission  []PermissionRule   `json:"permission,omitempty"`
	Permissions []PermissionRuleV2 `json:"permissions,omitempty"`
	// Metadata is free-form and stored with the session; FlowHub puts the task
	// key here so a session can be traced back to the issue.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// SessionModel is the session-level model reference (note the lowercase id).
type SessionModel struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
	Variant    string `json:"variant,omitempty"`
}

// PermissionRule is one entry of a v1 permission ruleset. The evaluation order is
// last-match-wins across the merged rulesets (built-in defaults, config, agent,
// then the session ruleset passed here), so the session ruleset has the final say.
type PermissionRule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     string `json:"action"` // allow | ask | deny
}

// PermissionRuleV2 is one entry of a v2 permission ruleset.
//
// The shape and the vocabulary both changed: an ordered array of
// {action, resource, effect}, where the v1 `permission` is now `action`, the v1
// `pattern` is now `resource`, and the v1 `action` is now `effect`. Evaluation is
// still last-match-wins, so the catch-all still has to come first.
type PermissionRuleV2 struct {
	Action   string `json:"action"`   // read | edit | shell | external_directory | … or *
	Resource string `json:"resource"` // path, command, URL, agent id, … or *
	Effect   string `json:"effect"`   // allow | ask | deny
}

// ModelRef is the prompt-level model reference (note modelID).
//
// GET /agent uses the same spelling, so one type covers both: the registry entry
// carries the model the server would use if a caller does not pin one.
type ModelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// AgentInfo is one entry of GET /agent (v1) or GET /api/agent (v2).
//
// Name is the agent's identifier — v1's `name`, v2's `id` — because that is the
// value a session is created with. v2 also carries a display `name` ("Build" for
// the id "build"); it is deliberately not used, so a profile comparison cannot
// pass on a label the server would not accept.
type AgentInfo struct {
	Name   string    `json:"name"`
	Mode   string    `json:"mode"`
	Hidden bool      `json:"hidden"`
	Model  *ModelRef `json:"model,omitempty"`
}

// ModelString renders the agent's model as "provider/model-id", or "" when the
// server let the agent inherit one.
func (a AgentInfo) ModelString() string {
	if a.Model == nil || a.Model.ProviderID == "" || a.Model.ModelID == "" {
		return ""
	}
	return a.Model.ProviderID + "/" + a.Model.ModelID
}

// TextPart is a prompt part carrying text.
type TextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// PromptRequest is the POST /session/{id}/prompt_async body.
type PromptRequest struct {
	Agent string     `json:"agent,omitempty"`
	Model *ModelRef  `json:"model,omitempty"`
	Parts []TextPart `json:"parts"`
	// MessageID is a client-generated id (^msg) intended for idempotency. Left
	// empty: the server-side behaviour is still unverified, and FlowHub's own
	// task registry rejects duplicates before a prompt is ever sent.
	MessageID string `json:"messageID,omitempty"`
}

// Session is one session record.
type Session struct {
	ID        string  `json:"id"`
	Slug      string  `json:"slug"`
	Directory string  `json:"directory"`
	Title     string  `json:"title"`
	Agent     string  `json:"agent"`
	Version   string  `json:"version"`
	Cost      float64 `json:"cost"`
	Tokens    Tokens  `json:"tokens"`
}

// Tokens is the usage counter. The message level carries Total; the session level
// does not, so Total is filled in by the caller when needed.
type Tokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Total     int `json:"total"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

// PermissionRequest is one pending permission in the policy's v1 vocabulary,
// whichever API generation supplied it: the v2 transport maps `action` to
// Permission, `resources` to Patterns and `save` to Always (client_v2.go).
//
// Measurements that matter for a safe arbiter:
//   - Patterns holds one entry per command in the request, but a compound shell
//     line arrives as a single request: `pwd && ls -la` produced
//     patterns=["pwd","ls -la"] with metadata.command="pwd && ls -la". v2 returns
//     only the resources, so the transport reconstructs the command with " && ".
//   - Always lists the patterns the server would remember if the reply is
//     "always"; that memory lives on the directory instance, so it survives new
//     sessions but not a server restart.
type PermissionRequest struct {
	ID         string             `json:"id"`
	SessionID  string             `json:"sessionID"`
	Permission string             `json:"permission"`
	Patterns   []string           `json:"patterns"`
	Metadata   PermissionMetadata `json:"metadata"`
	Always     []string           `json:"always"`
}

// PermissionMetadata carries tool-specific detail; for bash it is the full
// command line, which is what a policy must judge.
type PermissionMetadata struct {
	Command string `json:"command"`
}

// Reply is the answer to a permission request.
type Reply string

const (
	// ReplyOnce allows this call only.
	ReplyOnce Reply = "once"
	// ReplyAlways allows it and remembers the request's Always patterns for the
	// directory instance.
	ReplyAlways Reply = "always"
	// ReplyReject fails the tool call. The model receives a tool error and
	// continues, so rejection is safe to use as the unattended default.
	ReplyReject Reply = "reject"
)

// ReplyRequest is the POST /permission/{id}/reply body.
type ReplyRequest struct {
	Reply Reply `json:"reply"`
	// Message becomes feedback the model can read, which makes "reject" useful
	// for steering rather than only blocking.
	Message string `json:"message,omitempty"`
}

// Message is one entry of GET /session/{id}/message.
type Message struct {
	Info  MessageInfo `json:"info"`
	Parts []Part      `json:"parts"`
}

// MessageInfo is the message envelope.
type MessageInfo struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Agent      string          `json:"agent"`
	ProviderID string          `json:"providerID"`
	ModelID    string          `json:"modelID"`
	Finish     string          `json:"finish"`
	Cost       float64         `json:"cost"`
	Tokens     Tokens          `json:"tokens"`
	Time       MessageTime     `json:"time"`
	Error      json.RawMessage `json:"error"`
	// Outcome is set on a v2 `idle` message: succeeded, failed or interrupted.
	// It is the authoritative end of a v2 turn, which is why the v2 transport
	// projects it here instead of keeping a second message type.
	Outcome string `json:"outcome,omitempty"`
}

// MessageTime holds millisecond timestamps. Completed is only set once the turn
// really ended, which makes it the one reliable completion marker.
type MessageTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed"`
}

// Part is one slice of a message.
type Part struct {
	Type  string     `json:"type"`
	Text  string     `json:"text,omitempty"`
	Tool  string     `json:"tool,omitempty"`
	State *ToolState `json:"state,omitempty"`
}

// ToolState is the execution state of a tool part. A part stuck in "running" is
// the fingerprint of a call waiting for a permission decision.
type ToolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}
