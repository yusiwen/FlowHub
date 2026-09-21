package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo builds a real, hermetic git repository. The global and system git
// configuration is disabled so the developer's own settings (for example
// commit.gpgsign, which would demand a passphrase) cannot affect the test.
func testRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = repo
		command.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
			"GIT_CONFIG_NOSYSTEM=1",
		)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "-m", "init")
	return repo
}

func newManager(t *testing.T, base string) *Manager {
	t.Helper()
	manager, err := New(Options{Base: base})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return manager
}

func TestPrepareCreatesAWorktreeOnItsOwnBranchAndIgnoresScratch(t *testing.T) {
	repo := testRepo(t)
	manager := newManager(t, filepath.Join(t.TempDir(), "worktrees"))

	wt, err := manager.Prepare(context.Background(), Request{
		Repo: repo, TaskKey: "TEST-1", DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if wt.Reused {
		t.Fatal("first Prepare should create, not reuse")
	}
	if wt.Branch != "flowhub/TEST-1" {
		t.Fatalf("branch = %q", wt.Branch)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "README.md")); err != nil {
		t.Fatalf("worktree does not contain the repository content: %v", err)
	}

	// The scratch directory exists and is excluded, so the agent's own
	// `git status` stays clean while it works.
	if _, err := os.Stat(filepath.Join(wt.Path, ScratchDir)); err != nil {
		t.Fatalf("scratch directory missing: %v", err)
	}
	status := runGitForTest(t, wt.Path, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Fatalf("worktree is not clean; scratch is not ignored:\n%s", status)
	}

	// The repository's own .gitignore must not have been touched.
	if _, err := os.Stat(filepath.Join(repo, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("the shared repository's .gitignore was modified")
	}
}

func TestPrepareIsIdempotentForTheSameTask(t *testing.T) {
	repo := testRepo(t)
	manager := newManager(t, filepath.Join(t.TempDir(), "worktrees"))
	req := Request{Repo: repo, TaskKey: "TEST-2", DefaultBranch: "main"}

	first, err := manager.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	second, err := manager.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if !second.Reused {
		t.Fatal("second Prepare must reuse the existing worktree")
	}
	if first.Path != second.Path {
		t.Fatalf("paths differ: %q vs %q", first.Path, second.Path)
	}

	paths, err := manager.List(context.Background(), repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("worktree list = %v, want the clone plus one task", paths)
	}
}

func TestPrepareRejectsUnsafeRequests(t *testing.T) {
	repo := testRepo(t)
	base := filepath.Join(t.TempDir(), "worktrees")
	manager := newManager(t, base)

	cases := map[string]struct {
		req  Request
		want string
	}{
		"no repo":             {Request{TaskKey: "T-1", DefaultBranch: "main"}, "Repo is required"},
		"relative repo":       {Request{Repo: "relative/repo", TaskKey: "T-1", DefaultBranch: "main"}, "must be absolute"},
		"not a git repo":      {Request{Repo: t.TempDir(), TaskKey: "T-1", DefaultBranch: "main"}, "not a git work tree"},
		"no task key":         {Request{Repo: repo, DefaultBranch: "main"}, "TaskKey is required"},
		"task key with slash": {Request{Repo: repo, TaskKey: "a/b", DefaultBranch: "main"}, "not a safe path"},
		"task key with space": {Request{Repo: repo, TaskKey: "a b", DefaultBranch: "main"}, "not a safe path"},
		"no default branch":   {Request{Repo: repo, TaskKey: "T-1"}, "DefaultBranch is required"},
		"missing branch":      {Request{Repo: repo, TaskKey: "T-1", DefaultBranch: "nope"}, "does not exist"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := manager.Prepare(context.Background(), tc.req)
			if err == nil {
				t.Fatalf("Prepare succeeded, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestBaseInsideTheRepositoryIsRejected(t *testing.T) {
	repo := testRepo(t)
	manager := newManager(t, filepath.Join(repo, "worktrees"))
	_, err := manager.Prepare(context.Background(), Request{Repo: repo, TaskKey: "T-1", DefaultBranch: "main"})
	if err == nil {
		t.Fatal("a worktree base inside the repository must be refused")
	}
	if !strings.Contains(err.Error(), "inside the repository") {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoveDropsTheWorktreeAndKeepsTheBranch(t *testing.T) {
	repo := testRepo(t)
	manager := newManager(t, filepath.Join(t.TempDir(), "worktrees"))
	wt, err := manager.Prepare(context.Background(), Request{Repo: repo, TaskKey: "TEST-3", DefaultBranch: "main"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if err := manager.Remove(context.Background(), repo, wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists: %v", err)
	}
	// The branch is where the agent's work lives, so it must survive.
	branch := runGitForTest(t, repo, "rev-parse", "--verify", "refs/heads/"+wt.Branch)
	if strings.TrimSpace(branch) == "" {
		t.Fatal("the task branch was deleted with the worktree")
	}

	// Removing twice is a no-op, not an error.
	if err := manager.Remove(context.Background(), repo, wt); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func runGitForTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}
