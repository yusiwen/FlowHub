package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T, path string) *Registry {
	t.Helper()
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return r
}

func TestOpenOnAMissingFileIsEmptyNotAnError(t *testing.T) {
	r := openTest(t, filepath.Join(t.TempDir(), "registry.jsonl"))
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0", r.Len())
	}
	if _, ok := r.Get(DefaultSource, "TEST-1"); ok {
		t.Fatal("an empty registry returned a task")
	}
}

func TestPutGetUpdateAndList(t *testing.T) {
	r := openTest(t, filepath.Join(t.TempDir(), "registry.jsonl"))

	if err := r.Put(Task{Source: DefaultSource, Key: "TEST-1", Repo: "/repo", SessionID: "ses_1", State: StateAnalyzing, Plan: PlanNone}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	task, ok := r.Get(DefaultSource, "TEST-1")
	if !ok {
		t.Fatal("Get did not find the task")
	}
	if task.SessionID != "ses_1" || task.State != StateAnalyzing {
		t.Fatalf("task = %+v", task)
	}
	if task.CreatedAt.IsZero() || task.UpdatedAt.IsZero() {
		t.Fatal("timestamps were not stamped")
	}

	updated, err := r.Update(DefaultSource, "TEST-1", func(t *Task) {
		t.State = StateAwaitingInput
		t.Plan = PlanDraft
		t.Turns = 1
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.State != StateAwaitingInput || updated.Plan != PlanDraft || updated.Turns != 1 {
		t.Fatalf("updated = %+v", updated)
	}
	if updated.CreatedAt.IsZero() {
		t.Fatal("Update dropped CreatedAt")
	}

	if _, err := r.Update(DefaultSource, "NOPE", func(*Task) {}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Update on an unknown task = %v, want ErrTaskNotFound", err)
	}

	if err := r.Put(Task{Source: DefaultSource, Key: "TEST-2", Repo: "/repo2", State: StateDone, Plan: PlanConfirmed}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := len(r.List()); got != 2 {
		t.Fatalf("List returned %d tasks", got)
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.jsonl")
	first := openTest(t, path)
	if err := first.Put(Task{
		Source: DefaultSource,
		Key:    "TEST-9", Repo: "/repo", Worktree: "/wt/TEST-9", SessionID: "ses_9",
		Agent: "devops", State: StateExecuting, Plan: PlanConfirmed, Turns: 3, Cost: 0.25,
		LastReply: "the analysis text",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Update is the partial-change API: it must not clear the fields the first
	// write established.
	if _, err := first.Update(DefaultSource, "TEST-9", func(task *Task) {
		task.State = StateDone
		task.Turns = 4
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reopened := openTest(t, path)
	task, ok := reopened.Get(DefaultSource, "TEST-9")
	if !ok {
		t.Fatal("the task did not survive a reopen")
	}
	// Last write wins on replay.
	if task.State != StateDone || task.Turns != 4 {
		t.Fatalf("replayed task = %+v", task)
	}
	if task.SessionID != "ses_9" || task.Worktree != "/wt/TEST-9" {
		t.Fatalf("replay lost fields written by an earlier snapshot: %+v", task)
	}
	if task.LastReply != "the analysis text" {
		t.Fatalf("the last reply did not survive a restart: %+v", task)
	}
}

// Put is documented as a whole-record write, so pin that behaviour here: a
// caller that wants to change one field must use Update instead.
func TestPutReplacesTheWholeSnapshot(t *testing.T) {
	r := openTest(t, filepath.Join(t.TempDir(), "registry.jsonl"))
	if err := r.Put(Task{Source: DefaultSource, Key: "A", Repo: "/repo", SessionID: "ses_1", Agent: "devops", Turns: 2}); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(Task{Source: DefaultSource, Key: "A", State: StateDone}); err != nil {
		t.Fatal(err)
	}
	task, _ := r.Get(DefaultSource, "A")
	if task.Repo != "" || task.SessionID != "" || task.Agent != "" || task.Turns != 0 {
		t.Fatalf("Put merged instead of replacing: %+v", task)
	}
	if task.State != StateDone {
		t.Fatalf("state = %q", task.State)
	}
}

func TestEnsureCreatesOnlyOnce(t *testing.T) {
	r := openTest(t, filepath.Join(t.TempDir(), "registry.jsonl"))

	task, created, err := r.Ensure(DefaultSource, "TEST-4", func(t *Task) { t.Repo = "/repo"; t.Agent = "devops" })
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !created || task.State != StateAnalyzing || task.Plan != PlanNone {
		t.Fatalf("task = %+v created=%t", task, created)
	}

	again, created, err := r.Ensure(DefaultSource, "TEST-4", func(t *Task) { t.Repo = "/other" })
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if created {
		t.Fatal("Ensure recreated an existing task")
	}
	if again.Repo != "/repo" {
		t.Fatalf("Ensure overwrote the seed: %+v", again)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
}

func TestFilePermissionsAndSingleLinePerPut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.jsonl")
	r := openTest(t, path)
	if err := r.Put(Task{Source: DefaultSource, Key: "A", State: StateDone}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Update(DefaultSource, "A", func(t *Task) { t.Turns = 1 }); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("registry file mode = %o, want 600", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; got != 2 {
		t.Fatalf("file has %d lines, want one per write", got)
	}
}

// A corrupt line must stop the open rather than be skipped: forgetting a session
// would make FlowHub start a second one for the same task, which is the exact
// failure this table exists to prevent.
func TestCorruptLineIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.jsonl")
	content := `{"task_key":"A","state":"done"}` + "\n" + "{not json}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open succeeded on a corrupt registry")
	} else if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want it to name the bad line", err)
	}
}

func TestOpenRequiresAPath(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Fatal("want an error for an empty path")
	}
}

// TestTasksAreIndexedBySourceAndKey is ADR 0001 open question 4, decided: two
// trackers may have a project with the same key, and a task must never be continued
// on the wrong one. The registry's index is (source, key), so the same key in two
// sources is two tasks with two sessions.
func TestTasksAreIndexedBySourceAndKey(t *testing.T) {
	r := openTest(t, filepath.Join(t.TempDir(), "registry.jsonl"))

	if err := r.Put(Task{Source: "youtrack", Key: "TEST-17", Repo: "/repo/one",
		SessionID: "ses_youtrack", State: StateAnalyzing, Plan: PlanNone}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := r.Put(Task{Source: "gitea", Key: "TEST-17", Repo: "/repo/two",
		SessionID: "ses_gitea", State: StateAnalyzing, Plan: PlanNone}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want two tasks", r.Len())
	}

	left, ok := r.Get("youtrack", "TEST-17")
	if !ok || left.SessionID != "ses_youtrack" || left.Repo != "/repo/one" {
		t.Fatalf("youtrack task = %+v", left)
	}
	right, ok := r.Get("gitea", "TEST-17")
	if !ok || right.SessionID != "ses_gitea" || right.Repo != "/repo/two" {
		t.Fatalf("gitea task = %+v", right)
	}
	// A source that never wrote the key has no task, which is what keeps an unknown
	// tracker from adopting another one's session.
	if _, ok := r.Get("drone", "TEST-17"); ok {
		t.Fatal("an unrelated source found a task it never wrote")
	}
	// The source is part of the identity a caller writes down.
	if left.Qualified() != "youtrack:TEST-17" || right.Qualified() != "gitea:TEST-17" {
		t.Fatalf("Qualified = %q / %q", left.Qualified(), right.Qualified())
	}
	// A partial update touches only its own task.
	if _, err := r.Update("gitea", "TEST-17", func(task *Task) { task.Turns = 3 }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	left, _ = r.Get("youtrack", "TEST-17")
	right, _ = r.Get("gitea", "TEST-17")
	if left.Turns != 0 || right.Turns != 3 {
		t.Fatalf("turns after a gitea update: youtrack=%d gitea=%d", left.Turns, right.Turns)
	}
	// A task with no source is a caller bug, not a task: refusing it here is what
	// keeps an anonymous row from colliding with every source at once.
	if err := r.Put(Task{Key: "TEST-18"}); err == nil {
		t.Fatal("Put accepted a task without a source")
	}
}

// TestAPreSeamRowIsReadAsYouTrack is the migration: the registry on disk has rows
// written before the source seam existed, and they must keep working. Reading one as
// YouTrack is what it meant when it was written; the field is added the next time the
// task is written, so the file migrates as it is used rather than being rewritten.
func TestAPreSeamRowIsReadAsYouTrack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.jsonl")
	legacy := `{"task_key":"TEST-20","repo":"/repo/old","session_id":"ses_old","state":"awaiting_input","plan_state":"draft"}`
	if err := os.WriteFile(path, []byte(legacy+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := openTest(t, path)

	task, ok := r.Get("youtrack", "TEST-20")
	if !ok {
		t.Fatal("a pre-seam row was not readable as YouTrack")
	}
	if task.Source != DefaultSource || task.SessionID != "ses_old" {
		t.Fatalf("task = %+v", task)
	}
	if task.Qualified() != "youtrack:TEST-20" {
		t.Fatalf("Qualified = %q", task.Qualified())
	}
	if _, ok := r.Get("gitea", "TEST-20"); ok {
		t.Fatal("a pre-seam row answered for a source that did not exist then")
	}

	// The next write carries the source, so the row is migrated by use.
	if _, err := r.Update("youtrack", "TEST-20", func(task *Task) { task.Turns++ }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	task, _ = r.Get("youtrack", "TEST-20")
	if task.Source != DefaultSource {
		t.Fatalf("the written row has source %q, want %q", task.Source, DefaultSource)
	}
}
