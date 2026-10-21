package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// This file is the opencode 2.x transport. The v1 routes it replaces are gone
// from the 2.x binary (not deprecated — removed), and /global/health answers the
// web app's HTML shell with status 200. Measured against 2.0.32 on 2026-10-21;
// the wire shapes below are the ones the server actually returned, not only what
// the generated spec says (ADR 0012).
//
// Three differences shape the code here:
//
//   - responses are wrapped: {"data": …}. The message list adds a cursor, and a
//     long session therefore needs pagination where v1 returned one array.
//   - a session carries its location, so session-scoped routes take no
//     ?directory=. Endpoints that resolve configuration (agent, model, the global
//     permission list) take ?location[directory]=, a deepObject — not the flat
//     ?directory= that /api/session's own list endpoint uses.
//   - the permission vocabulary changed name: v1 `bash` is v2 `shell`, and a
//     request carries `resources` (one entry per command segment) with no
//     metadata.command. The arbiter keeps speaking v1 names, so the action is
//     translated on the way in; that way one policy, one test suite, both servers.

const (
	// messagesPerPage is what the v2 message list is asked for. It is also the
	// bound the pagination loop trusts: a page that comes back full gets a follow-up
	// request, a short page ends the loop.
	messagesPerPage = 200
	// maxMessagePages bounds the follow-up requests a pathological history can
	// cause. 40k messages is far beyond any task session; hitting it is a bug, and
	// silently truncating the list would mis-count the turn's baseline.
	maxMessagePages = 200
)

func (c *Client) healthV2(ctx context.Context) (Health, error) {
	var info struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/info", nil, nil, &info); err != nil {
		return Health{}, err
	}
	// v2 has no `healthy` flag: a 200 with a version is the whole readiness
	// answer, and a server that is still warming up fails /api/info instead.
	return Health{Healthy: true, Version: info.Version}, nil
}

func (c *Client) agentsV2(ctx context.Context) ([]AgentInfo, error) {
	var payload struct {
		Data []struct {
			ID     string      `json:"id"`
			Name   string      `json:"name"`
			Mode   string      `json:"mode"`
			Hidden bool        `json:"hidden"`
			Model  *modelRefV2 `json:"model"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/agent", nil, nil, &payload); err != nil {
		return nil, err
	}
	agents := make([]AgentInfo, 0, len(payload.Data))
	for _, agent := range payload.Data {
		info := AgentInfo{Name: agent.ID, Mode: agent.Mode, Hidden: agent.Hidden}
		if agent.Model != nil && agent.Model.ProviderID != "" && agent.Model.ID != "" {
			info.Model = &ModelRef{ProviderID: agent.Model.ProviderID, ModelID: agent.Model.ID}
		}
		agents = append(agents, info)
	}
	return agents, nil
}

// catalogueV2 reads /api/model and keeps the model a session could pin: the
// server's own `modelID`, not the catalogue entry's display id. A model the
// server marks disabled or deprecated is not offered for a new session, so the
// enrolment check should not accept it either.
func (c *Client) catalogueV2(ctx context.Context) (map[string][]string, error) {
	var payload struct {
		Data []struct {
			ID         string `json:"id"`
			ModelID    string `json:"modelID"`
			ProviderID string `json:"providerID"`
			Status     string `json:"status"`
			Enabled    bool   `json:"enabled"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/model", nil, nil, &payload); err != nil {
		return nil, err
	}
	catalogue := make(map[string][]string)
	for _, model := range payload.Data {
		if !model.Enabled || model.Status == "deprecated" {
			continue
		}
		id := strings.TrimSpace(model.ModelID)
		if id == "" {
			id = model.ID
		}
		catalogue[model.ProviderID] = append(catalogue[model.ProviderID], id)
	}
	for provider := range catalogue {
		sort.Strings(catalogue[provider])
	}
	return catalogue, nil
}

// createSessionV2 posts the v2 body. The location is part of the body (there is
// no ?directory= on this route), and the model uses {providerID, id} exactly as
// in v1 — only the endpoint moved.
func (c *Client) createSessionV2(ctx context.Context, directory string, req CreateSessionRequest) (Session, error) {
	if len(req.Permission) > 0 {
		return Session{}, fmt.Errorf("opencode: a v2 server cannot take the v1 permission ruleset; the runtime built the wrong ruleset")
	}
	if strings.TrimSpace(directory) == "" {
		return Session{}, fmt.Errorf("opencode: a v2 session needs an absolute directory")
	}
	body := struct {
		Title       string             `json:"title,omitempty"`
		Agent       string             `json:"agent,omitempty"`
		Model       *SessionModel      `json:"model,omitempty"`
		Permissions []PermissionRuleV2 `json:"permissions,omitempty"`
		Location    struct {
			Directory string `json:"directory"`
		} `json:"location"`
		Metadata map[string]any `json:"metadata,omitempty"`
	}{
		Title: req.Title, Agent: req.Agent, Model: req.Model,
		Permissions: req.Permissions, Metadata: req.Metadata,
	}
	body.Location.Directory = directory

	var payload struct {
		Data sessionV2 `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/session", nil, body, &payload); err != nil {
		return Session{}, err
	}
	return payload.Data.toSession(), nil
}

// promptAsyncV2 admits one prompt. The v2 body carries only the text: the agent
// and the model are properties of the session, pinned when it was created, so a
// per-message override is not part of this API. The server durably queues the
// input and schedules the agent loop itself — no web UI runner is involved
// (measured 2026-10-21, correcting the older field note).
func (c *Client) promptAsyncV2(ctx context.Context, sessionID string, req PromptRequest) error {
	text := strings.TrimSpace(textOfPrompt(req))
	if text == "" {
		return fmt.Errorf("opencode: a v2 prompt needs text")
	}
	body := struct {
		Text string `json:"text"`
	}{Text: text}
	var receipt json.RawMessage
	return c.do(ctx, http.MethodPost, "/api/session/"+sessionID+"/prompt", nil, body, &receipt)
}

// textOfPrompt joins the text parts of a v1-shaped prompt request. The runner
// builds one text part, so this is a conversion rather than a parser.
func textOfPrompt(req PromptRequest) string {
	var parts []string
	for _, part := range req.Parts {
		if part.Type != "" && part.Type != "text" {
			continue
		}
		if strings.TrimSpace(part.Text) != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (c *Client) sessionV2(ctx context.Context, sessionID string) (Session, error) {
	var payload struct {
		Data sessionV2 `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/session/"+sessionID, nil, nil, &payload); err != nil {
		return Session{}, err
	}
	return payload.Data.toSession(), nil
}

// sessionStatusV2 reads /api/session/active: the sessions this server process is
// currently draining. An absent session is idle, which is the same convention as
// v1's status map.
func (c *Client) sessionStatusV2(ctx context.Context, sessionID string) (Status, error) {
	var payload struct {
		Data map[string]struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/session/active", nil, nil, &payload); err != nil {
		return Status{}, err
	}
	if entry, ok := payload.Data[sessionID]; ok && entry.Type == "running" {
		return Status{Type: "busy"}, nil
	}
	return Status{}, nil
}

// messagesV2 walks the paginated message list. The list is ordered oldest first,
// which is what the runner's baseline arithmetic assumes; `cursor` is followed
// without `order`, because the server rejects the combination.
func (c *Client) messagesV2(ctx context.Context, sessionID string) ([]Message, error) {
	var messages []Message
	cursor := ""
	for page := 0; page < maxMessagePages; page++ {
		query := url.Values{"limit": {fmt.Sprint(messagesPerPage)}}
		if cursor == "" {
			query.Set("order", "asc")
		} else {
			query.Set("cursor", cursor)
		}
		var payload struct {
			Data   []messageV2 `json:"data"`
			Cursor struct {
				Next *string `json:"next"`
			} `json:"cursor"`
		}
		if err := c.do(ctx, http.MethodGet, "/api/session/"+sessionID+"/message", query, nil, &payload); err != nil {
			return nil, err
		}
		for _, message := range payload.Data {
			messages = append(messages, message.toMessage())
		}
		next := ""
		if payload.Cursor.Next != nil {
			next = strings.TrimSpace(*payload.Cursor.Next)
		}
		if next == "" || len(payload.Data) == 0 {
			return messages, nil
		}
		cursor = next
	}
	return nil, fmt.Errorf("opencode: session %s has more than %d message pages", sessionID, maxMessagePages)
}

func (c *Client) permissionsV2(ctx context.Context, sessionID string) ([]PermissionRequest, error) {
	var payload struct {
		Data []permissionV2 `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/session/"+sessionID+"/permission", nil, nil, &payload); err != nil {
		return nil, err
	}
	requests := make([]PermissionRequest, 0, len(payload.Data))
	for _, request := range payload.Data {
		requests = append(requests, request.toPermissionRequest())
	}
	return requests, nil
}

func (c *Client) replyPermissionV2(ctx context.Context, sessionID, requestID string, reply Reply, message string) error {
	body := struct {
		Decision Reply  `json:"decision"`
		Message  string `json:"message,omitempty"`
	}{Decision: reply, Message: message}
	path := "/api/session/" + sessionID + "/permission/" + requestID + "/reply"
	return c.do(ctx, http.MethodPost, path, nil, body, nil)
}

// ---- wire shapes ----

type modelRefV2 struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant"`
}

type tokenUsageV2 struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

type sessionV2 struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Agent    string        `json:"agent"`
	Model    *modelRefV2   `json:"model"`
	Cost     float64       `json:"cost"`
	Tokens   *tokenUsageV2 `json:"tokens"`
	Outcome  string        `json:"outcome"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Time struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
		Idle    int64 `json:"idle"`
	} `json:"time"`
}

func (s sessionV2) toSession() Session {
	session := Session{
		ID:        s.ID,
		Directory: s.Location.Directory,
		Title:     s.Title,
		Agent:     s.Agent,
		Cost:      s.Cost,
	}
	if s.Tokens != nil {
		session.Tokens = Tokens{
			Input:     s.Tokens.Input,
			Output:    s.Tokens.Output,
			Reasoning: s.Tokens.Reasoning,
			Total:     s.Tokens.Input + s.Tokens.Output,
			Cache:     s.Tokens.Cache,
		}
	}
	return session
}

type structuredErrorV2 struct {
	Type     string `json:"type"`
	Message  string `json:"message"`
	Status   int    `json:"status"`
	Response *struct {
		Body string `json:"body"`
	} `json:"response"`
}

type toolStateV2 struct {
	Status  string          `json:"status"`
	Input   json.RawMessage `json:"input"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		URI  string `json:"uri"`
	} `json:"content"`
	Error *structuredErrorV2 `json:"error"`
}

type contentV2 struct {
	Type  string       `json:"type"`
	Text  string       `json:"text"`
	Name  string       `json:"name"`
	State *toolStateV2 `json:"state"`
}

type messageV2 struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Outcome string             `json:"outcome"`
	Error   *structuredErrorV2 `json:"error"`
	Agent   string             `json:"agent"`
	Model   *modelRefV2        `json:"model"`
	Finish  string             `json:"finish"`
	Cost    *float64           `json:"cost"`
	Tokens  *tokenUsageV2      `json:"tokens"`
	Content []contentV2        `json:"content"`
	Text    string             `json:"text"`
}

// toMessage projects a v2 message onto the neutral Message the runner already
// understands. The v2 union (user, assistant, idle, and the lifecycle messages)
// is flattened: `type` becomes Role, and an idle message keeps its Outcome.
func (m messageV2) toMessage() Message {
	info := MessageInfo{
		ID:      m.ID,
		Role:    m.Type,
		Agent:   m.Agent,
		Finish:  m.Finish,
		Outcome: m.Outcome,
		Time:    MessageTime{Created: m.Time.Created, Completed: m.Time.Completed},
	}
	if m.Model != nil {
		info.ProviderID = m.Model.ProviderID
		info.ModelID = m.Model.ID
	}
	if m.Cost != nil {
		info.Cost = *m.Cost
	}
	if m.Tokens != nil {
		info.Tokens = Tokens{
			Input:     m.Tokens.Input,
			Output:    m.Tokens.Output,
			Reasoning: m.Tokens.Reasoning,
			Total:     m.Tokens.Input + m.Tokens.Output,
			Cache:     m.Tokens.Cache,
		}
	}
	if m.Error != nil {
		if encoded, err := json.Marshal(m.Error); err == nil {
			info.Error = encoded
		}
	}

	message := Message{Info: info}
	if m.Text != "" {
		message.Parts = append(message.Parts, Part{Type: "text", Text: m.Text})
	}
	for _, content := range m.Content {
		switch content.Type {
		case "text":
			message.Parts = append(message.Parts, Part{Type: "text", Text: content.Text})
		case "tool":
			part := Part{Type: "tool", Tool: content.Name}
			if content.State != nil {
				state := &ToolState{Status: content.State.Status, Input: content.State.Input}
				var texts []string
				for _, item := range content.State.Content {
					if item.Type == "text" && item.Text != "" {
						texts = append(texts, item.Text)
					}
				}
				state.Output = strings.Join(texts, "\n")
				if content.State.Error != nil {
					state.Error = content.State.Error.Message
				}
				part.State = state
			}
			message.Parts = append(message.Parts, part)
		case "reasoning":
			// Reasoning is not the answer and must not become part of it; the v1
			// projection drops it for the same reason.
		}
	}
	return message
}

type permissionV2 struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID"`
	Action    string         `json:"action"`
	Resources []string       `json:"resources"`
	Save      []string       `json:"save"`
	Metadata  map[string]any `json:"metadata"`
	Message   string         `json:"message"`
}

// toPermissionRequest translates a v2 request into the v1-shaped one the arbiter
// and the runner already speak.
//
// `shell` becomes `bash` because that is the vocabulary the policy's allowlist
// and deny list are written in; keeping two spellings in the policy would mean two
// code paths that can drift. For a shell request the command is reconstructed from
// `resources`, which holds one entry per command segment (`echo one && echo two`
// arrives as ["echo one", "echo two"], measured 2026-10-21). The arbiter splits it
// again, so the reconstruction is faithful; for any other action the resources are
// paths or URLs and are left in Patterns only.
func (p permissionV2) toPermissionRequest() PermissionRequest {
	permission := p.Action
	command := ""
	if permission == "shell" {
		permission = "bash"
		command = strings.Join(p.Resources, " && ")
		if raw, ok := p.Metadata["command"].(string); ok && strings.TrimSpace(raw) != "" {
			command = raw
		}
	}
	return PermissionRequest{
		ID:         p.ID,
		SessionID:  p.SessionID,
		Permission: permission,
		Patterns:   p.Resources,
		Metadata:   PermissionMetadata{Command: command},
		Always:     p.Save,
	}
}
