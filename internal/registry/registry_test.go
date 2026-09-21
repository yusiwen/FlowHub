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
	if _, ok := r.Get("TEST-1"); ok {
		t.Fatal("an empty registry returned a task")
	}
}

func TestPutGetUpdateAndList(t *testing.T) {
	r := openTest(t, filepath.Join(t.TempDir(), "registry.jsonl"))

	if err := r.Put(Task{Key: "TEST-1", Repo: "/repo", SessionID: "ses_1", State: StateAnalyzing, Plan: PlanNone}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	task, ok := r.Get("TEST-1")
	if !ok {
		t.Fatal("Get did not find the task")
	}
	if task.SessionID != "ses_1" || task.State != StateAnalyzing {
		t.Fatalf("task = %+v", task)
	}
	if task.CreatedAt.IsZero() || task.UpdatedAt.IsZero() {
		t.Fatal("timestamps were not stamped")
	}

	updated, err := r.Update("TEST-1", func(t *Task) {
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

	if _, err := r.Update("NOPE", func(*Task) {}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Update on an unknown task = %v, want ErrTaskNotFound", err)
	}

	if err := r.Put(Task{Key: "TEST-2", Repo: "/repo2", State: StateDone, Plan: PlanConfirmed}); err != nil {
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
		Key: "TEST-9", Repo: "/repo", Worktree: "/wt/TEST-9", SessionID: "ses_9",
		Agent: "devops", State: StateExecuting, Plan: PlanConfirmed, Turns: 3, Cost: 0.25,
		LastReplyHash: "abc123",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Update is the partial-change API: it must not clear the fields the first
	// write established.
	if _, err := first.Update("TEST-9", func(task *Task) {
		task.State = StateDone
		task.Turns = 4
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reopened := openTest(t, path)
	task, ok := reopened.Get("TEST-9")
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
}

// Put is documented as a whole-record write, so pin that behaviour here: a
// caller that wants to change one field must use Update instead.
func TestPutReplacesTheWholeSnapshot(t *testing.T) {
	r := openTest(t, filepath.Join(t.TempDir(), "registry.jsonl"))
	if err := r.Put(Task{Key: "A", Repo: "/repo", SessionID: "ses_1", Agent: "devops", Turns: 2}); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(Task{Key: "A", State: StateDone}); err != nil {
		t.Fatal(err)
	}
	task, _ := r.Get("A")
	if task.Repo != "" || task.SessionID != "" || task.Agent != "" || task.Turns != 0 {
		t.Fatalf("Put merged instead of replacing: %+v", task)
	}
	if task.State != StateDone {
		t.Fatalf("state = %q", task.State)
	}
}

func TestEnsureCreatesOnlyOnce(t *testing.T) {
	r := openTest(t, filepath.Join(t.TempDir(), "registry.jsonl"))

	task, created, err := r.Ensure("TEST-4", func(t *Task) { t.Repo = "/repo"; t.Agent = "devops" })
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !created || task.State != StateAnalyzing || task.Plan != PlanNone {
		t.Fatalf("task = %+v created=%t", task, created)
	}

	again, created, err := r.Ensure("TEST-4", func(t *Task) { t.Repo = "/other" })
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
	if err := r.Put(Task{Key: "A", State: StateDone}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Update("A", func(t *Task) { t.Turns = 1 }); err != nil {
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
