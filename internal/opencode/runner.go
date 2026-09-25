package opencode

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Runner drives one unattended task: create the session, deliver the prompt,
// answer permission requests while the turn runs, and report the outcome.
//
// It deliberately owns no state between calls. Session ownership (task key to
// session ID) belongs to FlowHub's registry, and the one-task-one-worktree
// isolation belongs to the dispatcher that prepares Task.Directory.
type Runner struct {
	client  *Client
	arbiter *Arbiter
	log     *slog.Logger
	// Poll is how often status and pending permissions are checked. The design
	// documents measured that a permission request is visible within ~2s, and
	// that the app-level timeout is generous, so one second is plenty.
	Poll time.Duration
	// FirstResponse bounds how long a turn may take to produce its first assistant
	// message. Zero uses DefaultFirstResponse, capped at a third of the task
	// deadline. A prompt the agent server never admits otherwise looks exactly like
	// a slow turn until the deadline.
	FirstResponse time.Duration
}

// Task is one unattended turn.
type Task struct {
	// Directory is the absolute, canonical path opencode binds the session to.
	// It should be the task's own git worktree, never the shared checkout.
	Directory string
	// Prompt is the user text for this turn.
	Prompt string
	// Agent selects the opencode agent; empty means the server default.
	Agent string
	// Model selects the model as "provider/model-id" (the spelling opencode uses
	// in its own configuration). Empty means the agent's default, which is the
	// honest default: the agent definition should own the model it was tuned for.
	Model string
	// Title is the session title, used as a human hint only (opencode rewrites
	// it after the first turn).
	Title string
	// SessionID continues an existing session. The design documents require one
	// task to keep one session across turns, and opencode lets a caller prompt a
	// session but never choose its ID, so the registry supplies it here. Empty
	// means "create a new session".
	SessionID string
	// Ruleset is the session-level permission ruleset. It is the first line of
	// defence and overrides project and global configuration, so it should at
	// minimum deny edit, external_directory, webfetch and websearch.
	Ruleset []PermissionRule
	// Metadata is stored with the session; FlowHub puts the task key here.
	Metadata map[string]any
	// Deadline bounds the whole turn. A deadline is not a failure: the session
	// keeps running and can be inspected later.
	Deadline time.Duration
}

// Result is the outcome of one turn.
type Result struct {
	SessionID string
	// Text is the last assistant text produced by the turn.
	Text string
	// Finished reports whether a new completed assistant message was observed.
	Finished bool
	// TimedOut reports that the deadline elapsed before Finished.
	TimedOut bool
	// Error is the last assistant error, if the turn failed.
	Error string
	Cost  float64
	// Tokens is the aggregate over the turn's assistant messages.
	Tokens      Tokens
	Permissions []Answered
	// Tools lists the tool calls the turn made, which is how a caller verifies a
	// claim such as "I posted the analysis comment" instead of trusting the prose.
	Tools   []ToolCall
	Elapsed time.Duration
}

// ToolCall is one tool invocation observed in a turn.
type ToolCall struct {
	Name   string
	Status string
	Input  string
	Output string
}

// Answered records one permission decision for the audit trail.
type Answered struct {
	RequestID  string
	Permission string
	Command    string
	Reply      Reply
	Reason     string
}

// Allowed reports whether the call was granted.
func (a Answered) Allowed() bool { return a.Reply == ReplyOnce || a.Reply == ReplyAlways }

// NewRunner builds a runner.
func NewRunner(client *Client, arbiter *Arbiter, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{client: client, arbiter: arbiter, log: log, Poll: time.Second}
}

// Run executes one task and returns its result.
//
// The loop is driven by three signals, all of which are needed:
//
//   - GET /permission?directory= for pending requests (a pending permission keeps
//     the session busy, so it must be answered or the turn never ends);
//   - GET /session/status?directory= to know whether the turn is still running;
//   - a new assistant message with time.completed set, which is the only
//     reliable "the turn really finished" marker, because status can lag.
func (r *Runner) Run(ctx context.Context, task Task) (Result, error) {
	started := time.Now()
	result := Result{}

	if strings.TrimSpace(task.Directory) == "" {
		return result, fmt.Errorf("opencode: task has no directory")
	}
	if strings.TrimSpace(task.Prompt) == "" {
		return result, fmt.Errorf("opencode: task has no prompt")
	}
	if err := validateModel(task.Model); err != nil {
		return result, err
	}

	sessionID := strings.TrimSpace(task.SessionID)
	if sessionID == "" {
		session, err := r.client.CreateSession(ctx, task.Directory, CreateSessionRequest{
			Title:      task.Title,
			Agent:      task.Agent,
			Model:      sessionModel(task.Model),
			Permission: task.Ruleset,
			Metadata:   task.Metadata,
		})
		if err != nil {
			return result, fmt.Errorf("opencode: create session in %s: %w", task.Directory, err)
		}
		sessionID = session.ID
		result.Tokens = session.Tokens
	}
	result.SessionID = sessionID

	// The baseline is taken before the prompt, so completion is detected against
	// the turns that already exist — which is what makes a second turn on the
	// same session work. Two counts come out of one read: the completed assistant
	// messages decide completion, and *any* assistant message decides whether the
	// turn started at all.
	existing, err := r.client.Messages(ctx, task.Directory, sessionID)
	if err != nil {
		return result, fmt.Errorf("opencode: list messages: %w", err)
	}
	baseline := len(completedAssistant(existing))
	startedBefore := assistantCount(existing)

	if err := r.client.PromptAsync(ctx, task.Directory, sessionID, PromptRequest{
		Agent: task.Agent,
		Model: promptModel(task.Model),
		Parts: []TextPart{{Type: "text", Text: task.Prompt}},
	}); err != nil {
		return result, fmt.Errorf("opencode: deliver prompt to session %s: %w", sessionID, err)
	}
	r.log.Info("opencode turn started",
		"session", sessionID, "directory", task.Directory, "agent", task.Agent,
		"model", task.Model, "continued", task.SessionID != "", "deadline", task.Deadline)

	deadline := time.Now().Add(task.Deadline)
	if task.Deadline <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	firstResponse := r.firstResponseBound(task.Deadline)
	firstResponseAt := time.Now().Add(firstResponse)

	for {
		if err := r.answerPending(ctx, task.Directory, sessionID, &result); err != nil {
			return result, err
		}

		messages, err := r.client.Messages(ctx, task.Directory, sessionID)
		if err != nil {
			return result, err
		}
		completed := completedAssistant(messages)
		statuses, err := r.client.SessionStatuses(ctx, task.Directory)
		if err != nil {
			return result, err
		}
		busy := statuses[sessionID].Busy()
		pending, err := r.pendingFor(ctx, task.Directory, sessionID)
		if err != nil {
			return result, err
		}

		// A turn is finished when a *new* assistant message has completed, the
		// session is not busy and nothing is waiting for a decision. Checking all
		// three avoids the two measured traps: status lagging after completion,
		// and busy being reported while a permission is pending.
		if len(completed) > baseline && !busy && len(pending) == 0 {
			result.Finished = true
			result.Text, result.Error = lastTurnOutput(messages, baseline)
			result.Tools = turnTools(messages, baseline)
			result.Cost, result.Tokens = r.aggregate(ctx, task.Directory, sessionID, messages, baseline)
			result.Elapsed = time.Since(started)
			r.log.Info("opencode turn finished",
				"session", sessionID, "elapsed", result.Elapsed.Round(time.Millisecond),
				"cost", result.Cost, "tokens", result.Tokens.Total, "permissions", len(result.Permissions))
			return result, nil
		}

		if time.Now().After(deadline) {
			result.TimedOut = true
			result.Text, result.Error = lastTurnOutput(messages, baseline)
			result.Cost, result.Tokens = r.aggregate(ctx, task.Directory, sessionID, messages, baseline)
			result.Elapsed = time.Since(started)
			// A timeout is not an error: the session may still be running, and
			// FlowHub marks the task "still running" rather than resetting it.
			r.log.Warn("opencode turn timed out",
				"session", sessionID, "elapsed", result.Elapsed.Round(time.Millisecond),
				"busy", busy, "pending_permissions", len(pending), "permissions_answered", len(result.Permissions))
			return result, nil
		}

		// A prompt the agent server rejects before it starts a turn — a model its
		// provider dropped, an agent profile the server does not have — produces no
		// assistant message and leaves the session idle, which is indistinguishable
		// from a slow turn until the deadline. Measured 2026-09-25: a turn that had
		// already died in 30ms still held the single worker eleven minutes later. The
		// assistant message is written before the provider is called, so this long
		// with none means the prompt was never admitted — and nothing is running, so
		// this is a failure rather than a timeout.
		if assistantCount(messages) <= startedBefore && !busy && len(pending) == 0 && time.Now().After(firstResponseAt) {
			result.Elapsed = time.Since(started)
			return result, fmt.Errorf("opencode: the agent server accepted the prompt but started no turn within %s (session %s is idle and has no new assistant message); its own log holds the reason, usually a model the provider does not offer or an agent profile the server does not have",
				firstResponse.Round(time.Millisecond), sessionID)
		}

		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(r.Poll):
		}
	}
}

// answerPending replies to every pending permission of this session.
func (r *Runner) answerPending(ctx context.Context, directory, sessionID string, result *Result) error {
	pending, err := r.pendingFor(ctx, directory, sessionID)
	if err != nil {
		return err
	}
	for _, request := range pending {
		decision := r.arbiter.Decide(request)
		message := ""
		if !decision.Allowed() {
			// A rejection with feedback becomes a CorrectedError the model can
			// read, so it can adapt instead of repeating the same call.
			message = decision.Reason
		}
		if err := r.client.ReplyPermission(ctx, directory, request.ID, decision.Reply, message); err != nil {
			return fmt.Errorf("opencode: reply to permission %s: %w", request.ID, err)
		}
		answered := Answered{
			RequestID:  request.ID,
			Permission: request.Permission,
			Command:    request.Metadata.Command,
			Reply:      decision.Reply,
			Reason:     decision.Reason,
		}
		result.Permissions = append(result.Permissions, answered)
		r.log.Info("permission answered",
			"session", sessionID, "permission", request.Permission,
			"command", request.Metadata.Command, "reply", decision.Reply, "reason", decision.Reason)
	}
	return nil
}

func (r *Runner) pendingFor(ctx context.Context, directory, sessionID string) ([]PermissionRequest, error) {
	all, err := r.client.Permissions(ctx, directory)
	if err != nil {
		return nil, fmt.Errorf("opencode: list permissions: %w", err)
	}
	var mine []PermissionRequest
	for _, request := range all {
		if request.SessionID == sessionID {
			mine = append(mine, request)
		}
	}
	return mine, nil
}

// firstResponseBound is how long a turn may take to produce its first assistant
// message before the turn is called dead.
//
// opencode writes that message before it calls the provider, so the bound measures
// the server admitting the prompt rather than the model being fast. It is
// deliberately far below the task deadline (fifteen minutes by default), because
// the failure it catches is immediate, and it never exceeds a third of the deadline:
// a turn with a short budget should fail fast rather than wait for a deadline it
// would hit first.
func (r *Runner) firstResponseBound(deadline time.Duration) time.Duration {
	if r.FirstResponse > 0 {
		return r.FirstResponse
	}
	bound := DefaultFirstResponse
	if deadline > 0 && deadline/3 < bound {
		bound = deadline / 3
	}
	if bound < MinimumFirstResponse {
		bound = MinimumFirstResponse
	}
	return bound
}

const (
	// DefaultFirstResponse is generous: an ordinary turn produces its assistant
	// message within a second or two, and a cold provider connection within a few.
	DefaultFirstResponse = 90 * time.Second
	// MinimumFirstResponse keeps a short task deadline from turning the check into
	// an immediate false failure.
	MinimumFirstResponse = 5 * time.Second
)

// assistantCount counts the assistant messages, completed or still in flight. A
// turn has started as soon as one exists.
func assistantCount(messages []Message) int {
	count := 0
	for _, message := range messages {
		if message.Info.Role == "assistant" {
			count++
		}
	}
	return count
}

// aggregate sums the turn's usage from its assistant messages, falling back to
// the session record when the messages carry nothing.
func (r *Runner) aggregate(ctx context.Context, directory, sessionID string, messages []Message, baseline int) (float64, Tokens) {
	var cost float64
	var tokens Tokens
	seen := 0
	for _, message := range messages {
		if message.Info.Role != "assistant" || message.Info.Time.Completed == 0 {
			continue
		}
		seen++
		if seen <= baseline {
			continue
		}
		cost += message.Info.Cost
		tokens.Input += message.Info.Tokens.Input
		tokens.Output += message.Info.Tokens.Output
		tokens.Reasoning += message.Info.Tokens.Reasoning
		tokens.Cache.Read += message.Info.Tokens.Cache.Read
		tokens.Cache.Write += message.Info.Tokens.Cache.Write
	}
	tokens.Total = tokens.Input + tokens.Output
	if cost == 0 && tokens.Total == 0 {
		if session, err := r.client.Session(ctx, directory, sessionID); err == nil {
			return session.Cost, session.Tokens
		}
	}
	return cost, tokens
}

// completedAssistant returns the assistant messages that actually finished.
func completedAssistant(messages []Message) []Message {
	var out []Message
	for _, message := range messages {
		if message.Info.Role == "assistant" && message.Info.Time.Completed != 0 {
			out = append(out, message)
		}
	}
	return out
}

// lastTurnOutput returns the text of the newest completed assistant message and
// its error, if any. The text is preferred from the final message, but a turn
// that ended in a tool call still has earlier text worth reporting.
func lastTurnOutput(messages []Message, baseline int) (string, string) {
	completed := completedAssistant(messages)
	if len(completed) <= baseline {
		return "", ""
	}
	turn := completed[baseline:]
	var text string
	var errText string
	for _, message := range turn {
		joined := strings.TrimSpace(textOf(message))
		if joined != "" {
			text = joined
		}
		if len(message.Info.Error) > 0 && string(message.Info.Error) != "null" {
			errText = strings.TrimSpace(string(message.Info.Error))
		}
	}
	return text, errText
}

// turnTools lists the tool calls that belong to this turn.
func turnTools(messages []Message, baseline int) []ToolCall {
	completed := completedAssistant(messages)
	if len(completed) <= baseline {
		return nil
	}
	var calls []ToolCall
	for _, message := range completed[baseline:] {
		for _, part := range message.Parts {
			if part.Type != "tool" || part.State == nil {
				continue
			}
			calls = append(calls, ToolCall{
				Name:   part.Tool,
				Status: part.State.Status,
				Input:  string(part.State.Input),
				Output: part.State.Output,
			})
		}
	}
	return calls
}

func textOf(message Message) string {
	var builder strings.Builder
	for _, part := range message.Parts {
		if part.Type != "text" || part.Text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(part.Text)
	}
	return builder.String()
}

// SplitModel splits the "provider/model-id" spelling opencode uses for a model
// reference. It is exported so the routing-table validation can reject a typo at
// startup instead of at the first turn.
func SplitModel(model string) (provider, id string, ok bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", false
	}
	// Cut on the first slash: a model id may itself contain one
	// (for example "accounts/fireworks/models/llama"), a provider id may not.
	provider, id, found := strings.Cut(model, "/")
	provider, id = strings.TrimSpace(provider), strings.TrimSpace(id)
	if !found || provider == "" || id == "" {
		return "", "", false
	}
	return provider, id, true
}

func validateModel(model string) error {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	if _, _, ok := SplitModel(model); !ok {
		return fmt.Errorf("opencode: model %q must be spelled provider/model-id", model)
	}
	return nil
}

// sessionModel renders the model for session creation, which uses the lowercase
// {providerID, id} spelling. A nil result leaves the agent's own default alone.
func sessionModel(model string) *SessionModel {
	provider, id, ok := SplitModel(model)
	if !ok {
		return nil
	}
	return &SessionModel{ProviderID: provider, ID: id}
}

// promptModel renders the model for a prompt, which uses {providerID, modelID}.
func promptModel(model string) *ModelRef {
	provider, id, ok := SplitModel(model)
	if !ok {
		return nil
	}
	return &ModelRef{ProviderID: provider, ModelID: id}
}
