package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/opencode"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/store"
)

// fakeOpencode is the smallest server that behaves like `opencode serve` for one
// turn: it accepts a session, a prompt, a permission request and a comment tool
// call, then reports a finished assistant message.
type fakeOpencode struct {
	t *testing.T

	mu        sync.Mutex
	prompts   []string
	sessions  int
	continued int
	toolCalls []string
	directory string
	// messages accumulates one completed assistant message per prompt, which is
	// what makes a second turn on the same session observable.
	messages []opencode.Message

	// askPermission makes the fake raise one bash permission request before
	// finishing, to exercise the arbiter path.
	askPermission bool
	// postComment controls whether the turn "posts" its reply.
	postComment bool
	// ruleset records what the session was created with.
	ruleset []opencode.PermissionRule
	// sessionBody and promptBody keep the whole request so an entry's agent and
	// model override can be asserted.
	sessionBody opencode.CreateSessionRequest
	promptBody  opencode.PromptRequest
}

func newFakeOpencode(t *testing.T) *fakeOpencode {
	return &fakeOpencode{t: t, askPermission: true, postComment: true}
}

func (f *fakeOpencode) start() *httptest.Server {
	handler := http.NewServeMux()
	handler.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"healthy": true, "version": "fake"})
	})
	handler.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sessions++
		var body opencode.CreateSessionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.ruleset = body.Permission
		f.sessionBody = body
		writeJSON(w, opencode.Session{ID: "ses_fake", Directory: f.directory})
	})
	handler.HandleFunc("/session/ses_fake/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body opencode.PromptRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.promptBody = body
		if len(body.Parts) > 0 {
			f.prompts = append(f.prompts, body.Parts[0].Text)
		}
		if len(f.prompts) > 1 {
			f.continued++
		}
		parts := []opencode.Part{}
		if f.askPermission {
			parts = append(parts, opencode.Part{
				Type: "tool", Tool: "bash",
				State: &opencode.ToolState{Status: "completed", Output: "ok"},
			})
		}
		if f.postComment {
			parts = append(parts, opencode.Part{
				Type: "tool", Tool: "youtrack_add_issue_comment",
				State: &opencode.ToolState{Status: "completed"},
			})
			f.toolCalls = append(f.toolCalls, "youtrack_add_issue_comment")
		}
		parts = append(parts, opencode.Part{Type: "text", Text: "analysis done"})
		id := len(f.messages)
		f.messages = append(f.messages,
			opencode.Message{Info: opencode.MessageInfo{ID: "u", Role: "user", Time: opencode.MessageTime{Created: int64(id)}}},
			opencode.Message{Info: opencode.MessageInfo{
				ID: "a", Role: "assistant", Finish: "stop", Cost: 0.004,
				Tokens: opencode.Tokens{Input: 100, Output: 20},
				Time:   opencode.MessageTime{Created: int64(id), Completed: int64(id) + 1},
			}, Parts: parts})
		w.WriteHeader(http.StatusNoContent)
	})
	handler.HandleFunc("/session/ses_fake/message", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, f.messages)
	})
	handler.HandleFunc("/session/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]opencode.Status{"ses_fake": {Type: "idle"}})
	})
	handler.HandleFunc("/permission", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, []opencode.PermissionRequest{})
	})
	return httptest.NewServer(handler)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// testRepo builds a real git repository, hermetic from the developer's own config.
func testRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = repo
		command.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
			"GIT_CONFIG_NOSYSTEM=1")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("-c", "user.email=t@example.com", "-c", "user.name=Test", "commit", "-q", "-m", "init")
	return repo
}

// newTestDispatcher wires a dispatcher against a temp repository and routing
// table. entryExtra is appended to the routing entry verbatim, so a test can
// declare an agent, a model or an author allowlist.
func newTestDispatcher(t *testing.T, fake *fakeOpencode, server *httptest.Server, entryExtra ...string) (*Dispatcher, *registry.Registry, string) {
	t.Helper()
	repo := testRepo(t)
	base := filepath.Join(t.TempDir(), "worktrees")

	// The routing table is a file, because that is the only supported source.
	projectsFile := filepath.Join(t.TempDir(), "config.json")
	extra := ""
	if len(entryExtra) > 0 {
		extra = "," + strings.Join(entryExtra, ",")
	}
	body := `{"projects":[{"youtrack_key":"TEST","repo":{"path":"` + repo + `","default_branch":"main"},"worktrees":"` + base + `"` + extra + `}]}`
	if err := os.WriteFile(projectsFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(projectsFile)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}
	if problems := projects.Validate(); len(problems) != 0 {
		t.Fatalf("routing table is invalid: %v", problems)
	}

	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.jsonl"))
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}

	fake.directory = repo
	dispatcher, err := New(Options{
		Client:   opencode.New(opencode.Options{BaseURL: server.URL, Timeout: 5 * time.Second}),
		Registry: reg,
		Projects: projects,
		Rules:    rules.Policy{}.Defaults(),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agent:    "flowhub-default-agent",
		Deadline: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return dispatcher, reg, repo
}

func delivery(issueID, event, body string) *store.Record {
	return &store.Record{
		Kind: "webhook", Accepted: true, Event: event, IssueID: issueID,
		ProjectKey: "TEST", PrimaryActor: "yusiwen", RawBody: body,
	}
}

func issueCreatedBody(issueID string) string {
	return `{"event":"issueCreated","id":"` + issueID + `","summary":"准入首页","description":"分页返回黑名单机构信息",` +
		`"project":{"key":"TEST","name":"TEST","shortName":"TEST"},"timestamp":"2026-09-21T00:00:00.000Z"}`
}

func TestIssueCreatedRunsAReadOnlyAnalysisTurn(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, reg, repo := newTestDispatcher(t, fake, server)

	dispatcher.handle(context.Background(), delivery("TEST-40", "issueCreated", issueCreatedBody("TEST-40")))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessions != 1 || len(fake.prompts) != 1 {
		t.Fatalf("sessions=%d prompts=%d, want one turn", fake.sessions, len(fake.prompts))
	}
	prompt := fake.prompts[0]
	for _, want := range []string{
		"READ-ONLY", "TEST-40", rules.SelfMarker, "youtrack_add_issue_comment", ".flowhub/attachments",
		// The sign-off has to state what triggered the turn, and the basis must
		// come from the decision rather than from the model's own account.
		"from the creation of this issue.",
		"The final line of the comment must be `> " + rules.SelfMarker + "`",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}

	// The session ruleset is order sensitive: the catch-all first.
	if len(fake.ruleset) == 0 || fake.ruleset[0].Permission != "*" || fake.ruleset[0].Action != "ask" {
		t.Fatalf("the catch-all must come first, got %+v", fake.ruleset)
	}
	var sawEditAsk bool
	for _, rule := range fake.ruleset {
		if rule.Permission == "edit" && rule.Action == "ask" {
			sawEditAsk = true
		}
	}
	if !sawEditAsk {
		t.Fatal("edit must be gated with ask so the phase can decide")
	}

	task, ok := reg.Get("TEST-40")
	if !ok {
		t.Fatal("the task was not recorded")
	}
	if task.SessionID != "ses_fake" || task.Worktree == "" {
		t.Fatalf("task = %+v", task)
	}
	if !strings.HasPrefix(task.Worktree, repo) && !strings.Contains(task.Worktree, "worktrees") {
		t.Fatalf("worktree %q was not created by the manager", task.Worktree)
	}
	if task.State != registry.StateAwaitingInput {
		t.Fatalf("state = %s, want awaiting_input after an analysis turn", task.State)
	}
	if task.Plan != registry.PlanDraft {
		t.Fatalf("plan = %s, want draft after analysis", task.Plan)
	}
	if task.Turns != 1 || task.Cost == 0 || task.LastReply == "" {
		t.Fatalf("task counters were not updated: %+v", task)
	}

	// The worktree must be a real, separate checkout.
	if _, err := os.Stat(filepath.Join(task.Worktree, "README.md")); err != nil {
		t.Fatalf("the worktree has no checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(task.Worktree, ".flowhub")); err != nil {
		t.Fatalf("the scratch directory is missing: %v", err)
	}
}

func TestSecondEventContinuesTheSameSessionAndWorktree(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, reg, _ := newTestDispatcher(t, fake, server)

	dispatcher.handle(context.Background(), delivery("TEST-41", "issueCreated", issueCreatedBody("TEST-41")))
	dispatcher.handle(context.Background(), delivery("TEST-41", "commentAdded",
		`{"event":"commentAdded","id":"TEST-41","summary":"准入首页","description":"d",`+
			`"project":{"key":"TEST","name":"TEST","shortName":"TEST"},`+
			`"comments":[{"text":"/opencode start","created":1,"author":{"login":"yusiwen"}}]}`))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessions != 1 {
		t.Fatalf("sessions = %d, want the session to be reused", fake.sessions)
	}
	if fake.continued != 1 {
		t.Fatalf("the second turn did not continue the session (prompts=%d)", len(fake.prompts))
	}
	if !strings.Contains(fake.prompts[1], "Implement the agreed plan") {
		t.Fatalf("the second prompt is not an execution prompt:\n%s", fake.prompts[1])
	}

	task, ok := reg.Get("TEST-41")
	if !ok {
		t.Fatal("task missing")
	}
	if task.Turns != 2 || task.State != registry.StateDone {
		t.Fatalf("task = %+v, want two turns ending done", task)
	}
}

// The agent posts as the same YouTrack user as the human, so a reply of ours must
// never be treated as an instruction.
func TestOurOwnReplyDoesNotStartAnotherTurn(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, reg, _ := newTestDispatcher(t, fake, server)

	dispatcher.handle(context.Background(), delivery("TEST-42", "issueCreated", issueCreatedBody("TEST-42")))
	task, _ := reg.Get("TEST-42")

	ownComment := `{"event":"commentAdded","id":"TEST-42","summary":"s","description":"d",` +
		`"project":{"key":"TEST","name":"TEST","shortName":"TEST"},` +
		`"comments":[{"text":"looks good\n\n` + rules.SelfMarker + `","created":1,` +
		`"author":{"login":"yusiwen"}}]}`
	dispatcher.handle(context.Background(), delivery("TEST-42", "commentAdded", ownComment))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.prompts) != 1 {
		t.Fatalf("our own reply started another turn (%d prompts)", len(fake.prompts))
	}
	after, _ := reg.Get("TEST-42")
	if after.Turns != task.Turns {
		t.Fatalf("turns changed from %d to %d", task.Turns, after.Turns)
	}
}

func TestAnUnmappedProjectIsNotDispatched(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, reg, _ := newTestDispatcher(t, fake, server)

	rec := delivery("OTHER-1", "issueCreated",
		`{"event":"issueCreated","id":"OTHER-1","summary":"s","description":"d","project":{"key":"OTHER","name":"OTHER","shortName":"OTHER"}}`)
	rec.ProjectKey = "OTHER"
	dispatcher.handle(context.Background(), rec)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessions != 0 || len(fake.prompts) != 0 {
		t.Fatal("an unmapped project must never reach opencode")
	}
	if reg.Len() != 0 {
		t.Fatalf("a task was recorded for an unmapped project: %d", reg.Len())
	}
}

func TestPauseFileStopsDispatchWithoutRestarting(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()

	pause := filepath.Join(t.TempDir(), "PAUSE")
	if err := os.WriteFile(pause, []byte("paused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := testRepo(t)
	projectsFile := filepath.Join(t.TempDir(), "config.json")
	body := `{"projects":[{"youtrack_key":"TEST","repo":{"path":"` + repo + `","default_branch":"main"},"worktrees":"` + filepath.Join(t.TempDir(), "wt") + `"}]}`
	if err := os.WriteFile(projectsFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(projectsFile)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := New(Options{
		Client:   opencode.New(opencode.Options{BaseURL: server.URL}),
		Registry: reg, Projects: projects,
		Rules: rules.Policy{}.Defaults(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		PauseFile: pause,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dispatcher.Paused() {
		t.Fatal("Paused() = false with the pause file present")
	}

	dispatcher.handle(context.Background(), delivery("TEST-43", "issueCreated", issueCreatedBody("TEST-43")))
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessions != 0 || reg.Len() != 0 {
		t.Fatal("a paused dispatcher still started work")
	}
}

func TestQueueOverflowDropsInsteadOfBlocking(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, _, _ := newTestDispatcher(t, fake, server)
	dispatcher.queue = make(chan *store.Record, 1)

	dispatcher.Dispatch(delivery("TEST-44", "issueCreated", issueCreatedBody("TEST-44")))
	dispatcher.Dispatch(delivery("TEST-45", "issueCreated", issueCreatedBody("TEST-45")))

	snapshot := dispatcher.Snapshot()
	if snapshot["dropped"].(int64) != 1 {
		t.Fatalf("dropped = %v, want 1", snapshot["dropped"])
	}
	if snapshot["queued"].(int64) != 1 {
		t.Fatalf("queued = %v, want 1", snapshot["queued"])
	}
}

func TestTurnWithoutAReplyIsRecordedAsSuch(t *testing.T) {
	fake := newFakeOpencode(t)
	fake.postComment = false
	server := fake.start()
	defer server.Close()
	dispatcher, reg, _ := newTestDispatcher(t, fake, server)

	dispatcher.handle(context.Background(), delivery("TEST-46", "issueCreated", issueCreatedBody("TEST-46")))

	task, ok := reg.Get("TEST-46")
	if !ok {
		t.Fatal("the task was not recorded")
	}
	// The registry still records the turn; the missing reply is surfaced in the
	// application log, which is where an operator looks.
	if task.Turns != 1 {
		t.Fatalf("turns = %d", task.Turns)
	}
}

func TestEntryAgentAndModelOverrideTheDefaults(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, _, _ := newTestDispatcher(t, fake, server,
		`"agent":"flowhub-analyst","model":"deepseek/deepseek-v4-flash"`)

	dispatcher.handle(context.Background(), delivery("TEST-50", "issueCreated", issueCreatedBody("TEST-50")))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessionBody.Agent != "flowhub-analyst" {
		t.Fatalf("session agent = %q, want the entry's agent", fake.sessionBody.Agent)
	}
	if fake.promptBody.Agent != "flowhub-analyst" {
		t.Fatalf("prompt agent = %q, want the entry's agent", fake.promptBody.Agent)
	}
	model := fake.sessionBody.Model
	if model == nil || model.ProviderID != "deepseek" || model.ID != "deepseek-v4-flash" {
		t.Fatalf("session model = %+v, want deepseek/deepseek-v4-flash", model)
	}
	if ref := fake.promptBody.Model; ref == nil || ref.ProviderID != "deepseek" || ref.ModelID != "deepseek-v4-flash" {
		t.Fatalf("prompt model = %+v, want the provider/model split", ref)
	}
}

func TestDefaultAgentAppliesWhenTheEntryNamesNone(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, _, _ := newTestDispatcher(t, fake, server)

	dispatcher.handle(context.Background(), delivery("TEST-51", "issueCreated", issueCreatedBody("TEST-51")))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessionBody.Agent != "flowhub-default-agent" {
		t.Fatalf("session agent = %q, want the configured default", fake.sessionBody.Agent)
	}
	if fake.sessionBody.Model != nil || fake.promptBody.Model != nil {
		t.Fatal("an entry without a model must leave the agent's own default alone")
	}
}

func TestAuthorAllowlistBlocksEveryoneElse(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, reg, _ := newTestDispatcher(t, fake, server, `"authors":["yusiwen"]`)

	stranger := delivery("TEST-52", "issueCreated", issueCreatedBody("TEST-52"))
	stranger.PrimaryActor = "someone.else"
	dispatcher.handle(context.Background(), stranger)

	fake.mu.Lock()
	sessions := fake.sessions
	fake.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("sessions = %d, want none for an actor outside the allowlist", sessions)
	}
	if _, ok := reg.Get("TEST-52"); ok {
		t.Fatal("a blocked delivery must not create a task")
	}

	// The named author still gets work through the same dispatcher.
	dispatcher.handle(context.Background(), delivery("TEST-53", "issueCreated", issueCreatedBody("TEST-53")))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessions != 1 {
		t.Fatalf("sessions = %d, want the allowlisted author's turn", fake.sessions)
	}
}

func TestNoActorNeverPassesAnAuthorAllowlist(t *testing.T) {
	// issueDeleted carries no actor, so a project with an allowlist can never
	// accept it. Asserted directly because it is the fail-closed branch.
	if authorAllowed([]string{"yusiwen"}, "") {
		t.Fatal("an empty actor must not match an allowlist")
	}
	if !authorAllowed(nil, "") {
		t.Fatal("an absent allowlist must allow every actor")
	}
	if !authorAllowed([]string{"YuSiWen"}, "yusiwen") {
		t.Fatal("the allowlist comparison must be case-insensitive")
	}
}

func TestProblemsReportsEntriesThatCannotBeDispatched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"projects":[` +
		`{"youtrack_key":"A","repo":{"path":"/tmp/a"}},` +
		`{"youtrack_key":"B","repo":{"path":"/tmp/b","default_branch":"main"},"worktrees":"/tmp/wt"},` +
		`{"youtrack_key":"C","repo":{"path":"/tmp/c"},"enabled":false}` +
		`]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(path)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}

	problems := Problems(projects, "")
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want the missing branch and the missing worktrees directory", problems)
	}
	if !strings.Contains(problems[0], "A") || !strings.Contains(problems[0], "default_branch") {
		t.Fatalf("first problem = %q", problems[0])
	}
	if !strings.Contains(problems[1], "worktrees directory") {
		t.Fatalf("second problem = %q", problems[1])
	}

	// A fallback base makes the entry above dispatchable again.
	if got := Problems(projects, "/tmp/fallback"); len(got) != 1 {
		t.Fatalf("problems with a fallback base = %v, want only the missing branch", got)
	}
	if got := Problems(nil, ""); got != nil {
		t.Fatalf("Problems(nil) = %v, want nil", got)
	}
}
