package opencode

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a minimal stand-in for `opencode serve`, covering the endpoints
// the runner uses and recording what the client sent.
type fakeServer struct {
	t *testing.T

	mu          sync.Mutex
	sessionID   string
	directory   string
	pending     []PermissionRequest
	messages    []Message
	busy        bool
	replies     map[string]Reply
	replyErrMsg map[string]string
	// afterReply, when set, mutates the state once a permission is answered.
	afterReply func(f *fakeServer)
	// refusePrompt makes prompt_async fail, to exercise the error path.
	refusePrompt bool
	// silentPrompt accepts prompt_async but never starts a turn: the user message is
	// recorded, no assistant message is written and the session stays idle. That is
	// what a rejected prompt looks like from the outside.
	silentPrompt bool

	sawDirectory []string
	sawAuth      []string
	sawAgent     string
	sawRuleset   []PermissionRule
}

func newFakeServer(t *testing.T) *fakeServer {
	return &fakeServer{
		t:           t,
		sessionID:   "ses_fake1",
		replies:     map[string]Reply{},
		replyErrMsg: map[string]string{},
	}
}

func (f *fakeServer) start() *httptest.Server {
	handler := http.NewServeMux()
	handler.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"healthy": true, "version": "test"})
	})
	handler.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.recordQuery(r)
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body CreateSessionRequest
		decodeBody(f.t, r, &body)
		f.sawAgent = body.Agent
		f.sawRuleset = body.Permission
		writeJSON(w, Session{ID: f.sessionID, Directory: f.directory, Agent: body.Agent, Title: body.Title})
	})
	handler.HandleFunc("/session/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.recordQuery(r)
		status := Status{Type: "idle"}
		if f.busy {
			status.Type = "busy"
		}
		writeJSON(w, map[string]Status{f.sessionID: status})
	})
	handler.HandleFunc("/session/"+f.sessionID+"/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.recordQuery(r)
		if f.refusePrompt {
			http.Error(w, `{"error":"session is busy"}`, http.StatusConflict)
			return
		}
		var body PromptRequest
		decodeBody(f.t, r, &body)
		f.messages = append(f.messages, Message{
			Info:  MessageInfo{ID: "msg_user", Role: "user", Time: MessageTime{Created: 1}},
			Parts: []Part{{Type: "text", Text: body.Parts[0].Text}},
		})
		f.busy = !f.silentPrompt
		w.WriteHeader(http.StatusNoContent)
	})
	handler.HandleFunc("/session/"+f.sessionID+"/message", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.recordQuery(r)
		writeJSON(w, f.messages)
	})
	handler.HandleFunc("/session/"+f.sessionID, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.recordQuery(r)
		writeJSON(w, Session{ID: f.sessionID, Directory: f.directory, Cost: 0.5, Tokens: Tokens{Total: 42}})
	})
	handler.HandleFunc("/permission", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.recordQuery(r)
		writeJSON(w, f.pending)
	})
	handler.HandleFunc("/permission/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/permission/"), "/reply")
		var body ReplyRequest
		decodeBody(f.t, r, &body)

		// Mutate under the lock, then release it before running the callback: the
		// callback locks again (finishWith), and sync.Mutex is not reentrant.
		f.mu.Lock()
		f.recordQuery(r)
		f.replies[id] = body.Reply
		f.replyErrMsg[id] = body.Message
		kept := f.pending[:0]
		for _, request := range f.pending {
			if request.ID != id {
				kept = append(kept, request)
			}
		}
		f.pending = kept
		after := f.afterReply
		f.mu.Unlock()

		if after != nil {
			after(f)
		}
		writeJSON(w, true)
	})
	return httptest.NewServer(handler)
}

func (f *fakeServer) recordQuery(r *http.Request) {
	f.sawDirectory = append(f.sawDirectory, r.URL.Query().Get("directory"))
	if user, _, ok := r.BasicAuth(); ok {
		f.sawAuth = append(f.sawAuth, user)
	}
}

func (f *fakeServer) addPending(request PermissionRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, request)
}

func (f *fakeServer) finishWith(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, Message{
		Info: MessageInfo{
			ID:     "msg_assistant",
			Role:   "assistant",
			Finish: "stop",
			Cost:   0.001,
			Tokens: Tokens{Input: 10, Output: 5, Total: 15},
			Time:   MessageTime{Created: 2, Completed: 3},
		},
		Parts: []Part{{Type: "text", Text: text}},
	})
	f.busy = false
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func decodeBody(t *testing.T, r *http.Request, target any) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode body %s: %v", raw, err)
	}
}

func testRunner(t *testing.T, server string) *Runner {
	t.Helper()
	client := New(Options{BaseURL: server, Timeout: 5 * time.Second})
	runner := NewRunner(client, DefaultArbiter(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.Poll = 5 * time.Millisecond
	return runner
}

func TestRunnerCompletesAfterAnsweringAPermission(t *testing.T) {
	fake := newFakeServer(t)
	fake.directory = "/private/tmp/work"
	server := fake.start()
	defer server.Close()

	fake.addPending(PermissionRequest{
		ID: "per_1", SessionID: fake.sessionID, Permission: "bash",
		Patterns: []string{"pwd", "ls -la"},
		Metadata: PermissionMetadata{Command: "pwd && ls -la"},
	})
	fake.afterReply = func(f *fakeServer) { f.finishWith("the directory holds 4 entries") }

	runner := testRunner(t, server.URL)
	result, err := runner.Run(context.Background(), Task{
		Directory: "/private/tmp/work",
		Prompt:    "count the entries",
		Agent:     "build",
		Title:     "trial",
		Ruleset:   []PermissionRule{{Permission: "bash", Pattern: "*", Action: "ask"}},
		Deadline:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Finished || result.TimedOut {
		t.Fatalf("result = %+v, want a finished turn", result)
	}
	if result.Text != "the directory holds 4 entries" {
		t.Fatalf("text = %q", result.Text)
	}
	if result.SessionID != fake.sessionID {
		t.Fatalf("session = %q", result.SessionID)
	}
	if len(result.Permissions) != 1 {
		t.Fatalf("permissions = %+v, want one answered", result.Permissions)
	}
	if got := result.Permissions[0].Reply; got != ReplyOnce {
		t.Fatalf("reply = %s, want once", got)
	}
	if got := fake.replies["per_1"]; got != ReplyOnce {
		t.Fatalf("server saw %s", got)
	}
	if result.Tokens.Total == 0 || result.Cost == 0 {
		t.Fatalf("usage not aggregated: %+v", result)
	}
	if fake.sawAgent != "build" {
		t.Fatalf("agent = %q", fake.sawAgent)
	}
	if len(fake.sawRuleset) != 1 || fake.sawRuleset[0].Action != "ask" {
		t.Fatalf("session ruleset was not sent: %+v", fake.sawRuleset)
	}
}

func TestRunnerRejectsDangerousCommandsAndStillFinishes(t *testing.T) {
	fake := newFakeServer(t)
	fake.directory = "/private/tmp/work"
	server := fake.start()
	defer server.Close()

	fake.addPending(PermissionRequest{
		ID: "per_danger", SessionID: fake.sessionID, Permission: "bash",
		Patterns: []string{"rm -rf /"},
		Metadata: PermissionMetadata{Command: "rm -rf /"},
	})
	fake.afterReply = func(f *fakeServer) { f.finishWith("I could not delete anything") }

	result, err := testRunner(t, server.URL).Run(context.Background(), Task{
		Directory: "/private/tmp/work", Prompt: "clean up", Deadline: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Finished {
		t.Fatalf("result = %+v, want finished: a rejection must not hang the turn", result)
	}
	if len(result.Permissions) != 1 || result.Permissions[0].Reply != ReplyReject {
		t.Fatalf("permissions = %+v, want one rejection", result.Permissions)
	}
	if msg := fake.replyErrMsg["per_danger"]; msg == "" {
		t.Fatal("a rejection must carry feedback for the model")
	}
}

func TestRunnerTimesOutWithoutFailing(t *testing.T) {
	fake := newFakeServer(t)
	fake.directory = "/private/tmp/work"
	server := fake.start()
	defer server.Close()

	// A permission that is never answerable keeps the session busy forever.
	fake.addPending(PermissionRequest{
		ID: "per_stuck", SessionID: "ses_other_session", Permission: "bash",
		Metadata: PermissionMetadata{Command: "pwd"},
	})

	result, err := testRunner(t, server.URL).Run(context.Background(), Task{
		Directory: "/private/tmp/work", Prompt: "wait", Deadline: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v (a timeout must not be an error)", err)
	}
	if result.Finished || !result.TimedOut {
		t.Fatalf("result = %+v, want TimedOut", result)
	}
	if len(result.Permissions) != 0 {
		t.Fatalf("permissions = %+v, want none: the request belongs to another session", result.Permissions)
	}
}

func TestRunnerPropagatesPromptFailure(t *testing.T) {
	fake := newFakeServer(t)
	fake.directory = "/private/tmp/work"
	fake.refusePrompt = true
	server := fake.start()
	defer server.Close()

	_, err := testRunner(t, server.URL).Run(context.Background(), Task{
		Directory: "/private/tmp/work", Prompt: "x", Deadline: time.Second,
	})
	if err == nil {
		t.Fatal("want an error when prompt_async fails")
	}
}

// TestRunnerFailsWhenThePromptNeverStarts is a regression test for the expensive
// failure measured on 2026-09-25: a prompt the agent server rejects (a model its
// provider dropped) produces no assistant message and leaves the session idle, so
// the runner saw a turn that looked merely slow and held the only worker for more
// than eleven minutes. The first-response bound fails it in seconds instead.
func TestRunnerFailsWhenThePromptNeverStarts(t *testing.T) {
	fake := newFakeServer(t)
	fake.directory = "/private/tmp/work"
	fake.silentPrompt = true
	server := fake.start()
	defer server.Close()

	runner := testRunner(t, server.URL)
	runner.FirstResponse = 50 * time.Millisecond
	started := time.Now()
	result, err := runner.Run(context.Background(), Task{
		Directory: "/private/tmp/work", Prompt: "analyse", Deadline: 30 * time.Second,
	})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a prompt that never started was reported as a turn")
	}
	if !strings.Contains(err.Error(), "started no turn") || !strings.Contains(err.Error(), "model the provider does not offer") {
		t.Fatalf("the failure does not name what happened or where to look: %v", err)
	}
	if result.TimedOut {
		t.Fatal("this is a failure, not a timeout: nothing is running")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s; the bound, not the deadline, has to end this turn", elapsed)
	}
}

// TestRunnerDoesNotFailASessionThatIsStillBusy is the other half: a busy session
// with no assistant message yet is what a slow model looks like, and the
// first-response bound must not turn that into a failure. A pending permission for
// another session keeps this one busy for the whole turn.
func TestRunnerDoesNotFailASessionThatIsStillBusy(t *testing.T) {
	fake := newFakeServer(t)
	fake.directory = "/private/tmp/work"
	fake.addPending(PermissionRequest{
		ID: "per_stuck", SessionID: "ses_other_session", Permission: "bash",
		Metadata: PermissionMetadata{Command: "pwd"},
	})
	server := fake.start()
	defer server.Close()

	runner := testRunner(t, server.URL)
	runner.FirstResponse = 30 * time.Millisecond
	result, err := runner.Run(context.Background(), Task{
		Directory: "/private/tmp/work", Prompt: "analyse", Deadline: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("a busy session was failed: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("result = %+v, want a timeout", result)
	}
}

// TestFirstResponseBoundNeverOutlivesTheDeadline pins the arithmetic: a turn with a
// short budget fails fast, a normal one gets the default, and a tiny deadline does
// not produce an immediate false failure.
func TestFirstResponseBoundNeverOutlivesTheDeadline(t *testing.T) {
	cases := []struct {
		name     string
		set      time.Duration
		deadline time.Duration
		want     time.Duration
	}{
		{name: "an explicit bound wins", set: 3 * time.Second, deadline: time.Hour, want: 3 * time.Second},
		{name: "the default for a normal deadline", deadline: 15 * time.Minute, want: DefaultFirstResponse},
		{name: "a third of a short deadline", deadline: 90 * time.Second, want: 30 * time.Second},
		{name: "never below the floor", deadline: 2 * time.Second, want: MinimumFirstResponse},
		{name: "no deadline at all", deadline: 0, want: DefaultFirstResponse},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &Runner{FirstResponse: test.set}
			if got := runner.firstResponseBound(test.deadline); got != test.want {
				t.Fatalf("firstResponseBound(%s) = %s, want %s", test.deadline, got, test.want)
			}
		})
	}
}

func TestRunnerRequiresDirectoryAndPrompt(t *testing.T) {
	runner := testRunner(t, "http://127.0.0.1:1")
	if _, err := runner.Run(context.Background(), Task{Prompt: "x"}); err == nil {
		t.Fatal("want an error without a directory")
	}
	if _, err := runner.Run(context.Background(), Task{Directory: "/tmp"}); err == nil {
		t.Fatal("want an error without a prompt")
	}
}

// Every session-scoped call must carry ?directory=: without it the server falls
// back to its own default location and reports nothing pending.
func TestClientAlwaysSendsTheDirectory(t *testing.T) {
	fake := newFakeServer(t)
	fake.directory = "/private/tmp/work"
	server := fake.start()
	defer server.Close()

	if _, err := testRunner(t, server.URL).Run(context.Background(), Task{
		Directory: "/private/tmp/work", Prompt: "x", Deadline: 60 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sawDirectory) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, directory := range fake.sawDirectory {
		if directory != "/private/tmp/work" {
			t.Fatalf("a request went out without ?directory= (saw %q)", directory)
		}
	}
}

func TestClientSendsBasicAuthWhenConfigured(t *testing.T) {
	var sawUser, sawPass string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUser, sawPass, _ = r.BasicAuth()
		writeJSON(w, Health{Healthy: true})
	}))
	defer server.Close()

	client := New(Options{BaseURL: server.URL, Username: "opencode", Password: "hunter2"})
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if sawUser != "opencode" || sawPass != "hunter2" {
		t.Fatalf("basic auth = %q/%q", sawUser, sawPass)
	}
}

func TestHTTPErrorAndUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := New(Options{BaseURL: server.URL}).Health(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !IsUnauthorized(err) {
		t.Fatalf("IsUnauthorized(%v) = false", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error does not name the status: %v", err)
	}
}
