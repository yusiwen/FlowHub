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

	agentruntime "github.com/yusiwen/flowhub/internal/agent/opencode"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/source/youtrack"
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
	messages []agentruntime.Message

	// askPermission makes the fake raise one bash permission request before
	// finishing, to exercise the arbiter path.
	askPermission bool
	// postComment controls whether the turn "posts" its reply.
	postComment bool
	// ruleset records what the session was created with.
	ruleset []agentruntime.PermissionRule
	// sessionBody and promptBody keep the whole request so an entry's agent and
	// model override can be asserted.
	sessionBody agentruntime.CreateSessionRequest
	promptBody  agentruntime.PromptRequest
	// silentFinish completes the message with tools only and no text, which is what a
	// turn that ends without a final message looks like.
	silentFinish bool
	// hold, when set, blocks every prompt until release is called. It is how a test
	// proves that two runtimes are running turns at the same time rather than in
	// sequence.
	hold        chan struct{}
	releaseOnce sync.Once
}

// release unblocks every held prompt, exactly once. Tests defer it so a failed
// assertion cannot leave a handler blocked and hang server shutdown.
func (f *fakeOpencode) release() {
	f.releaseOnce.Do(func() {
		if f.hold != nil {
			close(f.hold)
		}
	})
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
		var body agentruntime.CreateSessionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.ruleset = body.Permission
		f.sessionBody = body
		writeJSON(w, agentruntime.Session{ID: "ses_fake", Directory: f.directory})
	})
	handler.HandleFunc("/session/ses_fake/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body agentruntime.PromptRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.promptBody = body
		if len(body.Parts) > 0 {
			f.prompts = append(f.prompts, body.Parts[0].Text)
		}
		if len(f.prompts) > 1 {
			f.continued++
		}
		parts := []agentruntime.Part{}
		if f.askPermission {
			parts = append(parts, agentruntime.Part{
				Type: "tool", Tool: "bash",
				State: &agentruntime.ToolState{Status: "completed", Output: "ok"},
			})
		}
		if f.postComment {
			parts = append(parts, agentruntime.Part{
				Type: "tool", Tool: "youtrack_add_issue_comment",
				State: &agentruntime.ToolState{Status: "completed"},
			})
			f.toolCalls = append(f.toolCalls, "youtrack_add_issue_comment")
		}
		if !f.silentFinish {
			parts = append(parts, agentruntime.Part{Type: "text", Text: "analysis done"})
		}
		if f.hold != nil {
			// Block here, with the prompt recorded and no assistant message yet, so the
			// turn really is in flight. The mutex is released first: a second prompt must
			// be able to arrive, which is what this is here to observe.
			hold := f.hold
			f.mu.Unlock()
			<-hold
			f.mu.Lock()
		}
		id := len(f.messages)
		f.messages = append(f.messages,
			agentruntime.Message{Info: agentruntime.MessageInfo{ID: "u", Role: "user", Time: agentruntime.MessageTime{Created: int64(id)}}},
			agentruntime.Message{Info: agentruntime.MessageInfo{
				ID: "a", Role: "assistant", Finish: "stop", Cost: 0.004,
				Tokens: agentruntime.Tokens{Input: 100, Output: 20},
				Time:   agentruntime.MessageTime{Created: int64(id), Completed: int64(id) + 1},
			}, Parts: parts})
		w.WriteHeader(http.StatusNoContent)
	})
	handler.HandleFunc("/session/ses_fake/message", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, f.messages)
	})
	handler.HandleFunc("/session/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]agentruntime.Status{"ses_fake": {Type: "idle"}})
	})
	handler.HandleFunc("/permission", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, []agentruntime.PermissionRequest{})
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
		NewRuntime:     runtimeFactory(5 * time.Second),
		DefaultRuntime: newTestRuntime(DefaultRuntimeName, server.URL, 5*time.Second),
		NewWorkspace:   testWorkspaces,
		Source:         youtrack.New(rules.Policy{}, ""),
		Registry:       reg,
		Projects:       projects,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agent:          "flowhub-default-agent",
		Deadline:       5 * time.Second,
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

	task, ok := reg.Get(youtrack.SourceName, "TEST-40")
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

	task, ok := reg.Get(youtrack.SourceName, "TEST-41")
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
	task, _ := reg.Get(youtrack.SourceName, "TEST-42")

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
	after, _ := reg.Get(youtrack.SourceName, "TEST-42")
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
		NewRuntime:     runtimeFactory(time.Second),
		DefaultRuntime: newTestRuntime(DefaultRuntimeName, server.URL, time.Second),
		NewWorkspace:   testWorkspaces,
		Source:         youtrack.New(rules.Policy{}, ""),
		Registry:       reg, Projects: projects,
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

	task, ok := reg.Get(youtrack.SourceName, "TEST-46")
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
	if _, ok := reg.Get(youtrack.SourceName, "TEST-52"); ok {
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

// TestTaskRecordsThePinnedBaseline covers ADR 0001 step 5's registry contract: the
// commit a task starts from is resolved once, at creation, and recorded next to the
// task so a later turn — and a human reading the registry — can see where it began.
func TestTaskRecordsThePinnedBaseline(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, reg, repo := newTestDispatcher(t, fake, server)

	dispatcher.handle(context.Background(), delivery("TEST-41", "issueCreated", issueCreatedBody("TEST-41")))

	head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	task, ok := reg.Get(youtrack.SourceName, "TEST-41")
	if !ok {
		t.Fatal("no registry row for the task")
	}
	if task.BaseCommit != head {
		t.Fatalf("base_commit = %q, want the repository HEAD %q", task.BaseCommit, head)
	}
	if task.Worktree == "" || task.Runtime == "" {
		t.Fatalf("task = %+v, want a worktree and a runtime as well", task)
	}

	// A second delivery for the same task must not re-resolve the baseline, even
	// when the branch has moved on: a long-running task's patches stay reviewable
	// against a fixed base.
	if commit := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); commit != head {
		t.Fatalf("the test moved HEAD unexpectedly: %s != %s", commit, head)
	}
	dispatcher.handle(context.Background(), delivery("TEST-41", "issueCreated", issueCreatedBody("TEST-41")))
	after, _ := reg.Get(youtrack.SourceName, "TEST-41")
	if after.BaseCommit != head {
		t.Fatalf("base_commit changed to %q on a later turn", after.BaseCommit)
	}
}

// runGit runs one git command in dir and returns its stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return string(out)
}

// TestATextlessTurnKeepsTheRecordedReply: the last reply is what recognises our own
// comment coming back as a webhook when the marker is lost. A turn that ends without
// a final message — a tool-only loop, a crash — must not erase it.
func TestATextlessTurnKeepsTheRecordedReply(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, reg, _ := newTestDispatcher(t, fake, server)

	dispatcher.handle(context.Background(), delivery("TEST-42", "issueCreated", issueCreatedBody("TEST-42")))
	first, _ := reg.Get(youtrack.SourceName, "TEST-42")
	if first.LastReply == "" {
		t.Fatal("the first turn recorded no reply")
	}

	fake.mu.Lock()
	fake.silentFinish = true
	fake.mu.Unlock()
	dispatcher.handle(context.Background(), delivery("TEST-42", "issueCreated", issueCreatedBody("TEST-42")))

	second, _ := reg.Get(youtrack.SourceName, "TEST-42")
	if second.LastReply != first.LastReply {
		t.Fatalf("last reply = %q, want the previous %q", second.LastReply, first.LastReply)
	}
	if second.Turns != 2 {
		t.Fatalf("turns = %d, want the turn to have been counted", second.Turns)
	}
}

// promptCount reports how many prompts the fake has received.
func (f *fakeOpencode) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

// sessionCount and continuedCount are how a test sees whether a second turn reused
// the task's session or silently started another one.
func (f *fakeOpencode) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions
}

func (f *fakeOpencode) continuedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.continued
}

// runtimeEntry is one host's row of the /healthz snapshot, or nil when that host has
// no work and therefore no scheduler.
func runtimeEntry(t *testing.T, dispatcher *Dispatcher, name string) map[string]any {
	t.Helper()
	runtimes, _ := dispatcher.Snapshot()["runtimes"].(map[string]map[string]any)
	return runtimes[name]
}

// newScheduledDispatcher wires a dispatcher over a real repository and two enrolled
// runtimes on the fake server, which is what the scheduler tests need: a real
// worktree per task, and a policy that spreads them.
func newScheduledDispatcher(t *testing.T, fake *fakeOpencode, server *httptest.Server, extra string) (*Dispatcher, *registry.Registry) {
	t.Helper()
	return newScheduledDispatcherWith(t, fake, server, func(repo, base string) string {
		return `{"projects":[{"youtrack_key":"TEST","repo":{"path":"` + repo + `","default_branch":"main"},"worktrees":"` + base + `"` + extra + `}]}`
	})
}

// newScheduledDispatcherWith is the same wiring with the configuration file supplied
// by the test, because a v2 file has to name the runtime block the breadth lives in.
func newScheduledDispatcherWith(t *testing.T, fake *fakeOpencode, server *httptest.Server, build func(repo, base string) string) (*Dispatcher, *registry.Registry) {
	t.Helper()
	repo := testRepo(t)
	base := filepath.Join(t.TempDir(), "worktrees")
	projectsFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(projectsFile, []byte(build(repo, base)), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(projectsFile)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// The file's runtime blocks travel to the dispatcher the way main's
	// declaredRuntimes does it: enrolment answers where a host is, the file answers
	// what to ask of it (the breadth included).
	var declared []DeclaredRuntime
	for _, name := range projects.Runtimes() {
		block, _ := projects.RuntimeBlock(name)
		declared = append(declared, DeclaredRuntime{
			Name:          name,
			URL:           strings.TrimSpace(block.URL),
			Agent:         strings.TrimSpace(block.Agent),
			Model:         strings.TrimSpace(block.Model),
			MaxConcurrent: block.MaxConcurrentTasks(),
		})
	}
	fake.directory = repo
	dispatcher, err := New(Options{
		NewRuntime:     runtimeFactory(5 * time.Second),
		DefaultRuntime: newTestRuntime(DefaultRuntimeName, server.URL, 5*time.Second),
		NewWorkspace:   testWorkspaces,
		Source:         youtrack.New(rules.Policy{}, ""),
		Runtimes:       testInventory(t, server.URL, "builder-a", "builder-b"),
		Declared:       declared,
		Registry:       reg,
		Projects:       projects,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agent:          "devops",
		Deadline:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return dispatcher, reg
}

// TestRuntimesRunTurnsInParallel is the point of one queue per runtime: with two
// runtimes eligible, the second turn starts while the first is still running. Under
// the single worker this replaced, the second prompt could not arrive until the
// first turn ended, and this test would time out.
func TestRuntimesRunTurnsInParallel(t *testing.T) {
	fake := newFakeOpencode(t)
	fake.hold = make(chan struct{})
	server := fake.start()
	// Order matters: Close waits for the held handlers, so the release is registered
	// last and therefore runs first.
	defer server.Close()
	defer fake.release()
	dispatcher, reg := newScheduledDispatcher(t, fake, server, `,"runtimes":["builder-a","builder-b"],"runtime_policy":"spread"`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dispatcher.Run(ctx)

	dispatcher.Dispatch(delivery("TEST-50", "issueCreated", issueCreatedBody("TEST-50")))
	dispatcher.Dispatch(delivery("TEST-51", "issueCreated", issueCreatedBody("TEST-51")))

	// Both turns have to be in flight at once. The deadline is generous; what matters
	// is that a sequential dispatcher can never reach two.
	deadline := time.Now().Add(5 * time.Second)
	for fake.promptCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := fake.promptCount(); got != 2 {
		t.Fatalf("prompts in flight = %d, want 2: two runtimes must run turns in parallel", got)
	}

	// And the per-runtime view says so.
	runtimes, _ := dispatcher.Snapshot()["runtimes"].(map[string]map[string]any)
	if len(runtimes) != 2 {
		t.Fatalf("snapshot runtimes = %+v, want one entry per busy runtime", runtimes)
	}
	for name, entry := range runtimes {
		if inFlight, _ := entry["in_flight"].(int); inFlight != 1 {
			t.Fatalf("%s in_flight = %v, want 1", name, entry["in_flight"])
		}
	}

	fake.release()
	waitFor(t, 10*time.Second, func() bool {
		return dispatcher.Snapshot()["handled"].(int64) == 2
	})

	// The two tasks landed on different runtimes, which is what spread promised.
	first, _ := reg.Get(youtrack.SourceName, "TEST-50")
	second, _ := reg.Get(youtrack.SourceName, "TEST-51")
	if first.Runtime == "" || second.Runtime == "" || first.Runtime == second.Runtime {
		t.Fatalf("runtimes = %q and %q, want two different hosts", first.Runtime, second.Runtime)
	}
}

// TestOneRuntimeStillRunsOneTurnAtATime is the other half: parallel across runtimes
// must not become parallel within one. A project that names a single runtime keeps
// the old guarantee, because a session belongs to that runtime.
func TestOneRuntimeStillRunsOneTurnAtATime(t *testing.T) {
	fake := newFakeOpencode(t)
	fake.hold = make(chan struct{})
	server := fake.start()
	defer server.Close()
	defer fake.release()
	dispatcher, _ := newScheduledDispatcher(t, fake, server, `,"runtime":"builder-a"`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dispatcher.Run(ctx)

	dispatcher.Dispatch(delivery("TEST-52", "issueCreated", issueCreatedBody("TEST-52")))
	dispatcher.Dispatch(delivery("TEST-53", "issueCreated", issueCreatedBody("TEST-53")))

	// The first turn is held open, so a second prompt on the same runtime would mean
	// two turns on one session host at once.
	time.Sleep(500 * time.Millisecond)
	if got := fake.promptCount(); got != 1 {
		t.Fatalf("prompts in flight = %d, want 1: one runtime runs one turn at a time", got)
	}
	fake.release()
	waitFor(t, 10*time.Second, func() bool {
		return dispatcher.Snapshot()["handled"].(int64) == 2
	})
	if got := fake.promptCount(); got != 2 {
		t.Fatalf("prompts = %d, want both turns to have run", got)
	}
}

// TestOneHostRunsTwoTasksAtOnceWhenItsBreadthIsTwo is the point of the breadth: two
// *tasks* on one host at the same time, which the single-worker-per-runtime rule
// could not do.
func TestOneHostRunsTwoTasksAtOnceWhenItsBreadthIsTwo(t *testing.T) {
	fake := newFakeOpencode(t)
	fake.hold = make(chan struct{})
	server := fake.start()
	defer server.Close()
	defer fake.release()
	dispatcher, reg := newScheduledDispatcherWith(t, fake, server, func(repo, base string) string {
		return `{"version":2,
		  "runtimes":{"builder-a":{"url":"` + server.URL + `","agent":"devops","max_concurrent":2}},
		  "projects":[{"source":"youtrack","project":"TEST","repo":{"path":"` + repo + `","default_branch":"main"},"worktrees":"` + base + `","runtime":"builder-a"}]}`
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dispatcher.Run(ctx)

	dispatcher.Dispatch(delivery("TEST-60", "issueCreated", issueCreatedBody("TEST-60")))
	dispatcher.Dispatch(delivery("TEST-61", "issueCreated", issueCreatedBody("TEST-61")))

	waitFor(t, 5*time.Second, func() bool { return fake.promptCount() == 2 })
	entry := runtimeEntry(t, dispatcher, "builder-a")
	if entry == nil {
		t.Fatal("the host that ran the turns has no snapshot entry")
	}
	if got, _ := entry["running"].(int); got != 2 {
		t.Fatalf("running = %v, want 2: one host, two tasks, breadth two", entry["running"])
	}
	if got, _ := entry["max_concurrent"].(int); got != 2 {
		t.Fatalf("max_concurrent = %v, want the configured 2", entry["max_concurrent"])
	}
	if got, _ := entry["queued"].(int); got != 0 {
		t.Fatalf("queued = %v, want nothing waiting: both tasks have a worker", entry["queued"])
	}

	fake.release()
	waitFor(t, 10*time.Second, func() bool {
		return dispatcher.Snapshot()["handled"].(int64) == 2
	})
	for _, key := range []string{"TEST-60", "TEST-61"} {
		task, ok := reg.Get(youtrack.SourceName, key)
		if !ok || task.Runtime != "builder-a" {
			t.Fatalf("%s = %+v, want it on builder-a", key, task)
		}
	}
}

// TestASecondDeliveryForATaskWaitsForItsTurn is the other half of the breadth: the
// host may run two tasks, but never one task twice. The waiting delivery is held, not
// dropped, and runs the moment the first turn finishes.
func TestASecondDeliveryForATaskWaitsForItsTurn(t *testing.T) {
	fake := newFakeOpencode(t)
	fake.hold = make(chan struct{})
	server := fake.start()
	defer server.Close()
	defer fake.release()
	dispatcher, reg := newScheduledDispatcherWith(t, fake, server, func(repo, base string) string {
		return `{"version":2,
		  "runtimes":{"builder-a":{"url":"` + server.URL + `","agent":"devops","max_concurrent":2}},
		  "projects":[{"source":"youtrack","project":"TEST","repo":{"path":"` + repo + `","default_branch":"main"},"worktrees":"` + base + `","runtime":"builder-a"}]}`
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dispatcher.Run(ctx)

	first := delivery("TEST-62", "issueCreated", issueCreatedBody("TEST-62"))
	second := delivery("TEST-62", "issueCreated", issueCreatedBody("TEST-62"))
	second.RawBody = issueCreatedBody("TEST-62") + "\n"
	dispatcher.Dispatch(first)
	dispatcher.Dispatch(second)

	waitFor(t, 5*time.Second, func() bool {
		entry := runtimeEntry(t, dispatcher, "builder-a")
		return entry != nil && entry["running"] == 1 && entry["queued"] == 1
	})
	// The first turn is held open, so a second prompt can never arrive: the scheduler
	// keeps the second delivery of the same task waiting.
	time.Sleep(300 * time.Millisecond)
	if got := fake.promptCount(); got != 1 {
		t.Fatalf("prompts in flight = %d, want 1: one task is in one turn at a time", got)
	}

	fake.release()
	waitFor(t, 10*time.Second, func() bool {
		return dispatcher.Snapshot()["handled"].(int64) == 2
	})
	if got := fake.promptCount(); got != 2 {
		t.Fatalf("prompts = %d, want the waiting delivery to run rather than be dropped", got)
	}
	task, _ := reg.Get(youtrack.SourceName, "TEST-62")
	if task.Turns != 2 {
		t.Fatalf("turns = %d, want both deliveries recorded", task.Turns)
	}
}

// TestATaskWithNoRowYetGoesToExactlyOneRuntime: two deliveries of one burst reach the
// router before either has created the task row, so without the routing memo they
// could be handed to two hosts — two worktrees, two sessions, one issue. Measured
// before the fix: two prompts in flight on two hosts.
func TestATaskWithNoRowYetGoesToExactlyOneRuntime(t *testing.T) {
	fake := newFakeOpencode(t)
	fake.hold = make(chan struct{})
	server := fake.start()
	defer server.Close()
	defer fake.release()
	dispatcher, reg := newScheduledDispatcher(t, fake, server, `,"runtimes":["builder-a","builder-b"],"runtime_policy":"spread"`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dispatcher.Run(ctx)

	dispatcher.Dispatch(delivery("TEST-63", "issueCreated", issueCreatedBody("TEST-63")))
	dispatcher.Dispatch(delivery("TEST-63", "issueCreated", issueCreatedBody("TEST-63")))

	waitFor(t, 5*time.Second, func() bool {
		runtimes, _ := dispatcher.Snapshot()["runtimes"].(map[string]map[string]any)
		if len(runtimes) != 1 {
			return false
		}
		for _, entry := range runtimes {
			if entry["running"] == 1 && entry["queued"] == 1 {
				return true
			}
		}
		return false
	})
	// Only one prompt may exist even though the second delivery is already here: the
	// first turn is held open and the second waits for it.
	time.Sleep(300 * time.Millisecond)
	if got := fake.promptCount(); got != 1 {
		t.Fatalf("prompts in flight = %d, want 1: one task is one turn at a time", got)
	}

	fake.release()
	waitFor(t, 10*time.Second, func() bool {
		return dispatcher.Snapshot()["handled"].(int64) == 2
	})

	runtimes, _ := dispatcher.Snapshot()["runtimes"].(map[string]map[string]any)
	if len(runtimes) != 1 {
		t.Fatalf("runtimes = %+v, want both deliveries on one host", runtimes)
	}
	if got := fake.sessionCount(); got != 1 {
		t.Fatalf("sessions = %d, want one session for the task", got)
	}
	if got := fake.continuedCount(); got != 1 {
		t.Fatalf("continued = %d, want the second delivery to continue the session", got)
	}
	task, _ := reg.Get(youtrack.SourceName, "TEST-63")
	if task.Runtime == "" {
		t.Fatal("the task has no runtime binding")
	}
	if _, ok := runtimes[task.Runtime]; !ok {
		t.Fatalf("the task is bound to %s, which has no work in the snapshot %+v", task.Runtime, runtimes)
	}
	// The memo is a cache of one decision, not a second registry: once the burst has
	// drained there is nothing left to remember.
	waitFor(t, 5*time.Second, func() bool { return dispatcher.pendingRoutes() == 0 })
}

// TestRouteRefusesABoundTaskWhoseRuntimeIsGone: routing is where the refusal happens
// now, so it is asserted there rather than only at the turn.
func TestRouteRefusesABoundTaskWhoseRuntimeIsGone(t *testing.T) {
	fake := newFakeOpencode(t)
	server := fake.start()
	defer server.Close()
	dispatcher, reg := newScheduledDispatcher(t, fake, server, `,"runtime":"builder-a"`)

	// A task bound to a runtime this process does not have: the row outlives the host.
	if _, _, err := reg.Ensure(youtrack.SourceName, "TEST-54", func(task *registry.Task) {
		task.Runtime = "builder-gone"
		task.State = registry.StateAnalyzing
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := dispatcher.routeOne(context.Background(), delivery("TEST-54", "issueCreated", issueCreatedBody("TEST-54"))); ok {
		t.Fatal("a delivery bound to a runtime that is gone was routed")
	}
	if got := fake.promptCount(); got != 0 {
		t.Fatalf("prompts = %d, want none", got)
	}

	// A delivery for an unmapped project is refused before it can take a queue slot.
	unmapped := delivery("OTHER-1", "issueCreated", issueCreatedBody("OTHER-1"))
	unmapped.ProjectKey = "OTHER"
	if _, ok := dispatcher.routeOne(context.Background(), unmapped); ok {
		t.Fatal("a delivery for an unmapped project was routed")
	}
}

// waitFor polls a condition, failing the test if it never becomes true.
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the condition was never met")
}
