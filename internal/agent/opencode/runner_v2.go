package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// runV2 drives one turn on an opencode 2.x server.
//
// The completion signal is different from v1 and simpler: the server appends an
// `idle` message when the agent loop ends, carrying an `outcome` of succeeded,
// failed or interrupted. There is no status to race against and no need to infer
// "finished" from a completed assistant message plus an idle session — the idle
// message *is* that statement. Permissions still keep a turn alive, so they are
// answered while waiting, exactly as in v1 (measured 2026-10-21: a pending shell
// request held the session out of idle until it was rejected).
//
// The session is also self-contained now: it owns its location, agent and model.
// A prompt carries only text, and a session that the registry recorded under a
// v1 server does not exist here — that is detected before the prompt and answered
// by creating a fresh session, because a task bound to a dead session would
// otherwise fail every future delivery.
func (r *Runner) runV2(ctx context.Context, task Task) (Result, error) {
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
	baseline := 0
	if sessionID != "" {
		existing, err := r.client.Messages(ctx, task.Directory, sessionID)
		switch {
		case err == nil:
			baseline = len(existing)
		case IsNotFound(err):
			// A v1 session id recorded before the host was upgraded to opencode 2,
			// or a session someone deleted. Starting a new one keeps the task
			// runnable; the registry row is rewritten from result.SessionID below.
			r.log.Warn("the recorded session does not exist on this server; starting a new one",
				"session", sessionID, "directory", task.Directory)
			sessionID = ""
		default:
			return result, fmt.Errorf("opencode: list messages: %w", err)
		}
	}
	if sessionID == "" {
		session, err := r.client.CreateSession(ctx, task.Directory, CreateSessionRequest{
			Title:       task.Title,
			Agent:       task.Agent,
			Model:       sessionModel(task.Model),
			Permissions: task.RulesetV2,
			Metadata:    task.Metadata,
		})
		if err != nil {
			return result, fmt.Errorf("opencode: create session in %s: %w", task.Directory, err)
		}
		sessionID = session.ID
		result.Tokens = session.Tokens
	}
	result.SessionID = sessionID

	// The agent and the model belong to the session in v2; sending them per prompt
	// is not part of the API, and a continued session keeps the ones it was created
	// with.
	if err := r.client.PromptAsync(ctx, task.Directory, sessionID, PromptRequest{
		Parts: []TextPart{{Type: "text", Text: task.Prompt}},
	}); err != nil {
		return result, fmt.Errorf("opencode: deliver prompt to session %s: %w", sessionID, err)
	}
	r.log.Info("opencode turn started",
		"session", sessionID, "directory", task.Directory, "agent", task.Agent,
		"model", task.Model, "continued", strings.TrimSpace(task.SessionID) != "",
		"deadline", task.Deadline, "api", "v2")

	deadline := time.Now().Add(task.Deadline)
	if task.Deadline <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	firstResponse := r.firstResponseBound(task.Deadline)
	firstResponseAt := time.Now().Add(firstResponse)

	handled := map[string]bool{}
	for {
		if err := r.answerPending(ctx, task.Directory, sessionID, &result, handled); err != nil {
			return result, err
		}

		messages, err := r.client.Messages(ctx, task.Directory, sessionID)
		if err != nil {
			return result, err
		}
		turn := messagesFrom(messages, baseline)
		idle := firstIdle(turn)
		status, err := r.client.SessionStatuses(ctx, task.Directory, sessionID)
		if err != nil {
			return result, err
		}
		pending, err := r.pendingFor(ctx, task.Directory, sessionID)
		if err != nil {
			return result, err
		}
		pending = withoutHandled(pending, handled)

		// The idle marker is the turn's own statement that it ended, and a pending
		// permission keeps it from being written. Both are required.
		if idle != nil && len(pending) == 0 {
			result.Finished = true
			result.Text, result.Error = v2TurnOutput(turn, idle)
			result.Tools = v2TurnTools(turn)
			result.Cost, result.Tokens = r.aggregateV2(ctx, task.Directory, sessionID, turn)
			result.Elapsed = time.Since(started)
			r.log.Info("opencode turn finished",
				"session", sessionID, "elapsed", result.Elapsed.Round(time.Millisecond),
				"outcome", idle.Info.Outcome, "cost", result.Cost, "tokens", result.Tokens.Total,
				"permissions", len(result.Permissions))
			return result, nil
		}

		if time.Now().After(deadline) {
			result.TimedOut = true
			result.Text, result.Error = v2TurnOutput(turn, nil)
			result.Tools = v2TurnTools(turn)
			result.Cost, result.Tokens = r.aggregateV2(ctx, task.Directory, sessionID, turn)
			result.Elapsed = time.Since(started)
			r.log.Warn("opencode turn timed out",
				"session", sessionID, "elapsed", result.Elapsed.Round(time.Millisecond),
				"busy", status.Busy(), "pending_permissions", len(pending),
				"permissions_answered", len(result.Permissions))
			return result, nil
		}

		// Same failure as in v1: a prompt the server rejected before it started a
		// turn produces no assistant message and the session stays idle. In v2 the
		// server usually writes an idle message with outcome failed for this, but a
		// server that dies before admitting the input produces neither — bound it
		// rather than hold a worker until the deadline.
		if turnAssistantCount(turn) == 0 && idle == nil && !status.Busy() && len(pending) == 0 && time.Now().After(firstResponseAt) {
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

// messagesFrom returns the messages that belong to this turn. The prompt appends
// its user message after the baseline that was taken before it, so everything
// from that index on is the turn's.
func messagesFrom(messages []Message, baseline int) []Message {
	if baseline >= len(messages) {
		return nil
	}
	if baseline < 0 {
		baseline = 0
	}
	return messages[baseline:]
}

// firstIdle returns the turn's idle marker, if the turn has ended.
func firstIdle(turn []Message) *Message {
	for index := range turn {
		if turn[index].Info.Role == "idle" {
			return &turn[index]
		}
	}
	return nil
}

// turnAssistantCount counts the assistant messages of this turn.
func turnAssistantCount(turn []Message) int {
	count := 0
	for _, message := range turn {
		if message.Info.Role == "assistant" {
			count++
		}
	}
	return count
}

// v2TurnOutput reads the answer out of the turn and, when the idle marker says
// the turn failed, its reason.
func v2TurnOutput(turn []Message, idle *Message) (string, string) {
	var text string
	var assistantErr string
	for _, message := range turn {
		if message.Info.Role != "assistant" {
			continue
		}
		if joined := strings.TrimSpace(textOf(message)); joined != "" {
			text = joined
		}
		if errText := structuredMessage(message.Info.Error); errText != "" {
			assistantErr = errText
		}
	}
	if idle == nil {
		return text, assistantErr
	}
	switch idle.Info.Outcome {
	case "failed", "interrupted":
		if errText := structuredMessage(idle.Info.Error); errText != "" {
			return text, errText
		}
		if assistantErr != "" {
			return text, assistantErr
		}
		return text, "the turn ended with outcome " + idle.Info.Outcome
	default:
		return text, ""
	}
}

// v2TurnTools lists the tool calls of this turn.
func v2TurnTools(turn []Message) []ToolCall {
	var calls []ToolCall
	for _, message := range turn {
		if message.Info.Role != "assistant" {
			continue
		}
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

// aggregateV2 sums the turn's usage from its assistant messages, falling back to
// the session record when the messages carry nothing.
func (r *Runner) aggregateV2(ctx context.Context, directory, sessionID string, turn []Message) (float64, Tokens) {
	var cost float64
	var tokens Tokens
	for _, message := range turn {
		if message.Info.Role != "assistant" {
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

// structuredMessage renders a v2 StructuredError as the human-readable string the
// result carries. An error that is not JSON (a v1 error, or a bare string) is
// returned as-is, so the same helper is safe on both shapes.
func structuredMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var structured struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &structured); err == nil && structured.Message != "" {
		if structured.Type != "" {
			return structured.Type + ": " + structured.Message
		}
		return structured.Message
	}
	return trimmed
}
