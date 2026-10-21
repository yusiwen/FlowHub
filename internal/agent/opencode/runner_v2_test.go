package opencode

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
)

// fakeV2 is a stand-in for an opencode 2.x server. It speaks the measured shapes
// of 2.0.32: wrapped responses, session-scoped permission routes, and the idle
// message that ends a turn.
type fakeV2 struct {
	t *testing.T

	mu sync.Mutex
	// sessionID is the id a create returns; dead lists ids that 404, which is what
	// a v1 session recorded by the registry looks like on a v2 server.
	sessionID string
	dead      map[string]bool

	messages []map[string]any
	pending  []permissionV2
	active   bool
	// pageSize makes the message endpoint paginate; zero returns everything.
	pageSize int

	created  createSessionV2Body
	sessions int
	prompts  []string
	replies  []v2Reply

	afterPrompt func(*fakeV2)
	afterReply  func(*fakeV2)
}

type v2Reply struct {
	Decision string `json:"decision"`
	Message  string `json:"message"`
}

type createSessionV2Body struct {
	Title       string             `json:"title"`
	Agent       string             `json:"agent"`
	Model       *SessionModel      `json:"model"`
	Permissions []PermissionRuleV2 `json:"permissions"`
	Location    struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Metadata map[string]any `json:"metadata"`
}

func newFakeV2(t *testing.T) *fakeV2 {
	return &fakeV2{
		t:         t,
		sessionID: "ses_v2",
		dead:      map[string]bool{},
	}
}

func (f *fakeV2) start() *httptest.Server {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/info":
			writeJSON(w, map[string]any{"version": "2.0.32", "pid": 1, "urls": []string{}})
		case r.URL.Path == "/api/session" && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			var body createSessionV2Body
			if err := json.Unmarshal(raw, &body); err != nil {
				f.t.Fatalf("decode create body %s: %v", raw, err)
			}
			f.mu.Lock()
			f.sessions++
			f.created = body
			f.mu.Unlock()
			writeJSON(w, map[string]any{"data": map[string]any{
				"id": f.sessionID, "agent": body.Agent,
				"location": map[string]any{"directory": body.Location.Directory},
				"time":     map[string]any{"created": 1, "updated": 1},
			}})
		case r.URL.Path == "/api/session/active":
			f.mu.Lock()
			data := map[string]any{}
			if f.active {
				data[f.sessionID] = map[string]any{"type": "running"}
			}
			f.mu.Unlock()
			writeJSON(w, map[string]any{"data": data})
		case strings.HasPrefix(r.URL.Path, "/api/session/"):
			f.serveSession(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"_tag":"NotFoundError","message":"not found"}`))
		}
	})
	return httptest.NewServer(handler)
}

func (f *fakeV2) serveSession(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/session/")
	parts := strings.Split(rest, "/")
	sessionID := parts[0]

	f.mu.Lock()
	dead := f.dead[sessionID]
	f.mu.Unlock()
	if dead {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"_tag":"SessionNotFoundError","message":"session not found"}`))
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		f.mu.Lock()
		payload := map[string]any{
			"id": sessionID, "location": map[string]any{"directory": "/work"},
			"cost": 0.25, "tokens": map[string]any{"input": 100, "output": 20, "reasoning": 0,
				"cache": map[string]any{"read": 0, "write": 0}},
			"time": map[string]any{"created": 1, "updated": 2},
		}
		f.mu.Unlock()
		writeJSON(w, map[string]any{"data": payload})

	case len(parts) == 2 && parts[1] == "prompt" && r.Method == http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Fatalf("decode prompt %s: %v", raw, err)
		}
		f.mu.Lock()
		f.prompts = append(f.prompts, body.Text)
		f.messages = append(f.messages, map[string]any{
			"id": "msg_user", "type": "user", "text": body.Text,
			"time": map[string]any{"created": 10},
		})
		f.active = true
		after := f.afterPrompt
		f.mu.Unlock()
		if after != nil {
			after(f)
		}
		writeJSON(w, map[string]any{"data": map[string]any{
			"id": "msg_user", "sessionID": sessionID, "type": "user",
		}})

	case len(parts) == 2 && parts[1] == "message" && r.Method == http.MethodGet:
		f.mu.Lock()
		offset := 0
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			offset, _ = strconv.Atoi(cursor)
		}
		limit := f.pageSize
		if limit <= 0 {
			limit = len(f.messages)
		}
		end := offset + limit
		if end > len(f.messages) {
			end = len(f.messages)
		}
		page := append([]map[string]any(nil), f.messages[offset:end]...)
		var next any
		if end < len(f.messages) {
			next = strconv.Itoa(end)
		}
		f.mu.Unlock()
		writeJSON(w, map[string]any{"data": page, "cursor": map[string]any{"next": next}})

	case len(parts) == 2 && parts[1] == "permission" && r.Method == http.MethodGet:
		f.mu.Lock()
		requests := append([]permissionV2(nil), f.pending...)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"data": requests})

	case len(parts) == 4 && parts[1] == "permission" && parts[3] == "reply" && r.Method == http.MethodPost:
		requestID := parts[2]
		var body v2Reply
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Fatalf("decode reply %s: %v", raw, err)
		}
		f.mu.Lock()
		f.replies = append(f.replies, body)
		kept := f.pending[:0]
		for _, request := range f.pending {
			if request.ID != requestID {
				kept = append(kept, request)
			}
		}
		f.pending = kept
		after := f.afterReply
		f.mu.Unlock()
		if after != nil {
			after(f)
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"_tag":"NotFoundError","message":"not found"}`))
	}
}

func (f *fakeV2) appendMessages(messages ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, messages...)
}

func (f *fakeV2) finish(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages,
		map[string]any{
			"id": "msg_assistant", "type": "assistant", "agent": "devops", "finish": "stop",
			"model":  map[string]any{"id": "deepseek-flash", "providerID": "deepseek"},
			"cost":   0.002,
			"tokens": map[string]any{"input": 10, "output": 5, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
			"time":   map[string]any{"created": 11, "completed": 12},
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		},
		map[string]any{
			"id": "msg_idle", "type": "idle", "outcome": "succeeded",
			"time": map[string]any{"created": 13},
		})
	f.active = false
}

func (f *fakeV2) finishFailed(errType, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, map[string]any{
		"id": "msg_idle", "type": "idle", "outcome": "failed",
		"time": map[string]any{"created": 13},
		"error": map[string]any{
			"type": errType, "message": message,
		},
	})
	f.active = false
}

func testRunnerV2(t *testing.T, server string) *Runner {
	t.Helper()
	client := New(Options{BaseURL: server, Timeout: 5 * time.Second})
	runner := NewRunner(client, NewAnalysisArbiter(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.Poll = 5 * time.Millisecond
	return runner
}

func TestV2TurnFinishesOnIdle(t *testing.T) {
	fake := newFakeV2(t)
	fake.afterPrompt = func(f *fakeV2) { f.finish("the tree is clean") }
	server := fake.start()
	defer server.Close()

	runner := testRunnerV2(t, server.URL)
	result, err := runner.Run(context.Background(), Task{
		Directory: "/work",
		Prompt:    "is the tree clean?",
		Agent:     "devops",
		Model:     "deepseek/deepseek-flash",
		Title:     "TEST-1",
		RulesetV2: sessionRulesetV2(nil),
		Deadline:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Finished || result.TimedOut {
		t.Fatalf("result = %+v, want a finished turn", result)
	}
	if result.Text != "the tree is clean" {
		t.Fatalf("text = %q", result.Text)
	}
	if result.SessionID != "ses_v2" {
		t.Fatalf("session = %q", result.SessionID)
	}
	if result.Cost != 0.002 || result.Tokens.Input != 10 || result.Tokens.Output != 5 {
		t.Fatalf("cost/tokens = %v %+v", result.Cost, result.Tokens)
	}
	if len(fake.prompts) != 1 || fake.prompts[0] != "is the tree clean?" {
		t.Fatalf("prompts = %+v", fake.prompts)
	}

	// The session carries what a v2 turn is: location, agent, model, ruleset.
	if fake.created.Agent != "devops" {
		t.Fatalf("agent = %q", fake.created.Agent)
	}
	if fake.created.Model == nil || fake.created.Model.ProviderID != "deepseek" || fake.created.Model.ID != "deepseek-flash" {
		t.Fatalf("model = %+v", fake.created.Model)
	}
	if fake.created.Location.Directory != "/work" {
		t.Fatalf("location = %q", fake.created.Location.Directory)
	}
	if len(fake.created.Permissions) == 0 || fake.created.Permissions[0].Action != "*" || fake.created.Permissions[0].Effect != "ask" {
		t.Fatalf("ruleset = %+v, want the catch-all first", fake.created.Permissions)
	}
}

func TestV2TurnAnswersAPermission(t *testing.T) {
	fake := newFakeV2(t)
	fake.pending = []permissionV2{{
		ID: "per_1", SessionID: "ses_v2", Action: "shell",
		Resources: []string{"pwd"},
		Save:      []string{"pwd *"},
	}}
	fake.afterReply = func(f *fakeV2) { f.finish("working directory printed") }
	server := fake.start()
	defer server.Close()

	runner := testRunnerV2(t, server.URL)
	result, err := runner.Run(context.Background(), Task{
		Directory: "/work",
		Prompt:    "where are you?",
		Agent:     "devops",
		Model:     "deepseek/deepseek-flash",
		RulesetV2: sessionRulesetV2(nil),
		Deadline:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Finished {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Permissions) != 1 {
		t.Fatalf("permissions = %+v", result.Permissions)
	}
	answered := result.Permissions[0]
	if answered.Permission != "bash" || answered.Command != "pwd" || answered.Reply != ReplyOnce {
		t.Fatalf("answered = %+v", answered)
	}
	if len(fake.replies) != 1 || fake.replies[0].Decision != "once" {
		t.Fatalf("server saw %+v", fake.replies)
	}
}

func TestV2TurnReportsAFailedOutcome(t *testing.T) {
	fake := newFakeV2(t)
	fake.afterPrompt = func(f *fakeV2) { f.finishFailed("provider.auth", "User not found.") }
	server := fake.start()
	defer server.Close()

	runner := testRunnerV2(t, server.URL)
	result, err := runner.Run(context.Background(), Task{
		Directory: "/work",
		Prompt:    "hello",
		Agent:     "devops",
		Model:     "deepseek/deepseek-flash",
		RulesetV2: sessionRulesetV2(nil),
		Deadline:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Finished {
		t.Fatalf("result = %+v, want the idle marker to finish the turn", result)
	}
	if !strings.Contains(result.Error, "User not found.") || !strings.Contains(result.Error, "provider.auth") {
		t.Fatalf("error = %q", result.Error)
	}
}

// TestV2DeadSessionStartsANewOne is the upgrade path: the registry still holds a
// v1 session id, and a v2 server has never heard of it. Refusing would make the
// task undeliverable for good, so the turn starts a fresh session and the result
// carries its id — which is what the dispatcher writes back.
func TestV2DeadSessionStartsANewOne(t *testing.T) {
	fake := newFakeV2(t)
	fake.dead["ses_old"] = true
	fake.afterPrompt = func(f *fakeV2) { f.finish("a fresh session") }
	server := fake.start()
	defer server.Close()

	runner := testRunnerV2(t, server.URL)
	result, err := runner.Run(context.Background(), Task{
		Directory: "/work",
		Prompt:    "continue the task",
		Agent:     "devops",
		Model:     "deepseek/deepseek-flash",
		SessionID: "ses_old",
		RulesetV2: sessionRulesetV2(nil),
		Deadline:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SessionID != "ses_v2" {
		t.Fatalf("session = %q, want the new one", result.SessionID)
	}
	if fake.sessions != 1 {
		t.Fatalf("sessions created = %d, want 1", fake.sessions)
	}
	if !result.Finished {
		t.Fatalf("result = %+v", result)
	}
}

// TestV2PaginatedHistory proves the baseline is the whole history, not the first
// page: the idle marker of a previous turn must not be mistaken for this turn's.
func TestV2PaginatedHistory(t *testing.T) {
	fake := newFakeV2(t)
	fake.pageSize = 1
	fake.appendMessages(
		map[string]any{"id": "old_user", "type": "user", "text": "earlier", "time": map[string]any{"created": 1}},
		map[string]any{"id": "old_assistant", "type": "assistant", "time": map[string]any{"created": 2, "completed": 3}, "content": []map[string]any{{"type": "text", "text": "earlier answer"}}},
		map[string]any{"id": "old_idle", "type": "idle", "outcome": "succeeded", "time": map[string]any{"created": 4}},
	)
	fake.afterPrompt = func(f *fakeV2) { f.finish("this turn's answer") }
	server := fake.start()
	defer server.Close()

	runner := testRunnerV2(t, server.URL)
	result, err := runner.Run(context.Background(), Task{
		Directory: "/work",
		Prompt:    "next question",
		Agent:     "devops",
		Model:     "deepseek/deepseek-flash",
		SessionID: "ses_v2",
		RulesetV2: sessionRulesetV2(nil),
		Deadline:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Finished || result.Text != "this turn's answer" {
		t.Fatalf("result = %+v", result)
	}
}

// TestV2TurnFailsWhenNoTurnStarts covers a server that admits the prompt and then
// says nothing: the bound, not the deadline, must end the turn.
func TestV2TurnFailsWhenNoTurnStarts(t *testing.T) {
	fake := newFakeV2(t)
	fake.afterPrompt = func(f *fakeV2) {
		f.mu.Lock()
		f.active = false
		f.mu.Unlock()
	}
	server := fake.start()
	defer server.Close()

	runner := testRunnerV2(t, server.URL)
	runner.FirstResponse = 20 * time.Millisecond
	_, err := runner.Run(context.Background(), Task{
		Directory: "/work",
		Prompt:    "hello",
		Agent:     "devops",
		Model:     "deepseek/deepseek-flash",
		RulesetV2: sessionRulesetV2(nil),
		Deadline:  5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "started no turn") {
		t.Fatalf("error = %v, want the first-response failure", err)
	}
}

// TestRuntimeSendsTheV2Ruleset drives the real Runtime against a v2 server: the
// protocol is detected once and the ruleset is built in the server's vocabulary.
func TestRuntimeSendsTheV2Ruleset(t *testing.T) {
	fake := newFakeV2(t)
	fake.afterPrompt = func(f *fakeV2) { f.finish("done") }
	server := fake.start()
	defer server.Close()

	client := New(Options{BaseURL: server.URL, Timeout: 5 * time.Second})
	runtime := NewRuntime(client, RuntimeOptions{Name: "local", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if _, err := runtime.Run(context.Background(), agent.Turn{
		Directory:    "/work",
		Prompt:       "post the analysis",
		Agent:        "devops",
		Model:        "deepseek/deepseek-flash",
		Phase:        agent.PhaseAnalysis,
		AllowedTools: []string{"youtrack_add_comment"},
		Deadline:     5 * time.Second,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	byAction := map[string]PermissionRuleV2{}
	for _, rule := range fake.created.Permissions {
		byAction[rule.Action] = rule
	}
	if rule, ok := byAction["external_directory"]; !ok || rule.Effect != "deny" {
		t.Fatalf("ruleset = %+v, want external_directory denied (v2 allows it by default)", fake.created.Permissions)
	}
	if rule, ok := byAction["shell"]; !ok || rule.Effect != "ask" {
		t.Fatalf("ruleset = %+v, want shell gated", fake.created.Permissions)
	}
	if rule, ok := byAction["youtrack_add_comment"]; !ok || rule.Effect != "allow" {
		t.Fatalf("ruleset = %+v, want the source's tool allowed", fake.created.Permissions)
	}
	if fake.created.Permissions[0].Action != "*" || fake.created.Permissions[0].Effect != "ask" {
		t.Fatalf("ruleset = %+v, want the catch-all first", fake.created.Permissions)
	}
}
