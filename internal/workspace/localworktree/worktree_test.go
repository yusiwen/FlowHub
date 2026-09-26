package localworktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/workspace"
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

func newProvider(t *testing.T, base string) *Provider {
	t.Helper()
	provider, err := New(Options{Base: base})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider
}

func TestPrepareCreatesAWorktreeOnItsOwnBranchAndIgnoresScratch(t *testing.T) {
	repo := testRepo(t)
	provider := newProvider(t, filepath.Join(t.TempDir(), "worktrees"))

	wt, err := provider.Prepare(context.Background(), workspace.Request{
		Repo: repo, TaskKey: "TEST-1", DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if wt.Reused {
		t.Fatal("first Prepare should create, not reuse")
	}
	// The branch is the task's own; asserting it from inside the checkout is what
	// makes the claim about git state rather than about the provider's own report.
	if branch := gitIn(t, wt.Path, "rev-parse", "--abbrev-ref", "HEAD"); branch != DefaultBranchPrefix+"TEST-1" {
		t.Fatalf("worktree HEAD is on %q, want %q", branch, DefaultBranchPrefix+"TEST-1")
	}
	// The handle carries the provider's attestation. The path is canonical on
	// purpose — the runtime keys sessions by the literal string — so compare it
	// with the resolved repo, not with the string the test built.
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if wt.Provider != Name || wt.Repo != canonicalRepo {
		t.Fatalf("handle = %+v, want provider %q and repo %q", wt, Name, canonicalRepo)
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
	provider := newProvider(t, filepath.Join(t.TempDir(), "worktrees"))
	req := workspace.Request{Repo: repo, TaskKey: "TEST-2", DefaultBranch: "main"}

	first, err := provider.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	second, err := provider.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if !second.Reused {
		t.Fatal("second Prepare must reuse the existing worktree")
	}
	if first.Path != second.Path {
		t.Fatalf("paths differ: %q vs %q", first.Path, second.Path)
	}

	paths, err := provider.List(context.Background(), repo)
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
	provider := newProvider(t, base)

	cases := map[string]struct {
		req  workspace.Request
		want string
	}{
		"no repo":           {workspace.Request{TaskKey: "T-1", DefaultBranch: "main"}, "Repo is required"},
		"relative repo":     {workspace.Request{Repo: "relative/repo", TaskKey: "T-1", DefaultBranch: "main"}, "must be absolute"},
		"not a git repo":    {workspace.Request{Repo: t.TempDir(), TaskKey: "T-1", DefaultBranch: "main"}, "not a git work tree"},
		"no task key":       {workspace.Request{Repo: repo, DefaultBranch: "main"}, "TaskKey is required"},
		"no default branch": {workspace.Request{Repo: repo, TaskKey: "T-1"}, "DefaultBranch is required"},
		"missing branch":    {workspace.Request{Repo: repo, TaskKey: "T-1", DefaultBranch: "nope"}, "does not exist"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := provider.Prepare(context.Background(), tc.req)
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
	provider := newProvider(t, filepath.Join(repo, "worktrees"))
	_, err := provider.Prepare(context.Background(), workspace.Request{Repo: repo, TaskKey: "T-1", DefaultBranch: "main"})
	if err == nil {
		t.Fatal("a worktree base inside the repository must be refused")
	}
	if !strings.Contains(err.Error(), "inside the repository") {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoveDropsTheWorktreeAndKeepsTheBranch(t *testing.T) {
	repo := testRepo(t)
	provider := newProvider(t, filepath.Join(t.TempDir(), "worktrees"))
	wt, err := provider.Prepare(context.Background(), workspace.Request{Repo: repo, TaskKey: "TEST-3", DefaultBranch: "main"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if err := provider.Remove(context.Background(), wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists: %v", err)
	}
	// The branch is where the agent's work lives, so it must survive.
	branch := runGitForTest(t, repo, "rev-parse", "--verify", "refs/heads/"+DefaultBranchPrefix+"TEST-3")
	if strings.TrimSpace(branch) == "" {
		t.Fatal("the task branch was deleted with the worktree")
	}

	// Removing twice is a no-op, not an error.
	if err := provider.Remove(context.Background(), wt); err != nil {
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

// TestPrepareRecreatesAWorktreeWhoseDirectoryVanished is a regression test for a
// live failure: the directory was deleted with `rm -rf` (rather than through
// Remove), git still had it registered, and every later delivery failed with
// "fatal: not a git repository" — the task could never start again. Git's
// bookkeeping is not proof that the checkout is still there.
func TestPrepareRecreatesAWorktreeWhoseDirectoryVanished(t *testing.T) {
	repo := testRepo(t)
	provider := newProvider(t, filepath.Join(t.TempDir(), "worktrees"))
	request := workspace.Request{Repo: repo, TaskKey: "TEST-9", DefaultBranch: "main"}

	first, err := provider.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	if err := os.RemoveAll(first.Path); err != nil {
		t.Fatal(err)
	}
	// Git still lists it, which is exactly the stale state.
	listed, err := provider.List(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	var stillRegistered bool
	for _, candidate := range listed {
		if candidate == first.Path {
			stillRegistered = true
		}
	}
	if !stillRegistered {
		t.Skip("git already pruned the missing worktree; nothing to regress")
	}

	second, err := provider.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare after the directory vanished: %v", err)
	}
	if second.Reused {
		t.Fatal("a missing worktree was reported as reused")
	}
	if _, err := os.Stat(filepath.Join(second.Path, ".git")); err != nil {
		t.Fatalf("the worktree was not recreated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second.Path, ScratchDir)); err != nil {
		t.Fatalf("the recreated worktree has no scratch directory: %v", err)
	}
}

// TestPrepareClearsALeftoverDirectoryWithoutLosingFiles covers the other half of a
// live failure: a directory can sit at the worktree path without git knowing about
// it (a half-finished cleanup, or scratch left by an earlier run). `git worktree
// add` refuses to write into it, so it has to be dealt with — and a directory that
// holds anything FlowHub does not recognise is moved aside, never deleted.
func TestPrepareClearsALeftoverDirectoryWithoutLosingFiles(t *testing.T) {
	repo := testRepo(t)
	base := filepath.Join(t.TempDir(), "worktrees")
	provider := newProvider(t, base)

	// Only FlowHub's own scratch: removed and recreated.
	scratchOnly := filepath.Join(base, "TEST-7")
	if err := os.MkdirAll(filepath.Join(scratchOnly, ScratchDir), 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := provider.Prepare(context.Background(), workspace.Request{Repo: repo, TaskKey: "TEST-7", DefaultBranch: "main"})
	if err != nil {
		t.Fatalf("Prepare over our own scratch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(prepared.Path, ".git")); err != nil {
		t.Fatalf("the worktree was not created: %v", err)
	}

	// Somebody else's file: moved aside, not deleted.
	occupied := filepath.Join(base, "TEST-8")
	if err := os.MkdirAll(occupied, 0o700); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(occupied, "notes.txt")
	if err := os.WriteFile(precious, []byte("do not lose me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err = provider.Prepare(context.Background(), workspace.Request{Repo: repo, TaskKey: "TEST-8", DefaultBranch: "main"})
	if err != nil {
		t.Fatalf("Prepare over an occupied directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(prepared.Path, ".git")); err != nil {
		t.Fatalf("the worktree was not created: %v", err)
	}
	matches, err := filepath.Glob(occupied + ".stale-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("the occupied directory was not moved aside: %v", matches)
	}
	content, err := os.ReadFile(filepath.Join(matches[0], "notes.txt"))
	if err != nil || string(content) != "do not lose me\n" {
		t.Fatalf("the file that was in the way did not survive: %v %q", err, content)
	}
}

// gitIn runs one git command in dir with the same hermetic environment the rest of
// these tests use.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitIn writes a file and commits it, returning the new commit id.
func commitIn(t *testing.T, repo, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "-m", "change "+name)
	return gitIn(t, repo, "rev-parse", "HEAD")
}

// originWithTwoClones builds the multi-host situation on one machine: a bare origin,
// a clone that is up to date, and a clone that is deliberately left behind.
func originWithTwoClones(t *testing.T) (origin, fresh, stale string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, origin, "init", "-q", "--bare", "-b", "main")

	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "-m", "base")
	gitIn(t, seed, "remote", "add", "origin", origin)
	gitIn(t, seed, "push", "-q", "-u", "origin", "main")

	clone := func(name string) string {
		dir := filepath.Join(root, name)
		gitIn(t, root, "clone", "-q", origin, dir)
		return dir
	}
	fresh = clone("fresh")
	stale = clone("stale")

	// The origin moves on, and only one clone hears about it.
	commitIn(t, seed, "CHANGELOG.md", "moved on\n")
	gitIn(t, seed, "push", "-q", "origin", "main")
	gitIn(t, fresh, "fetch", "-q", "origin")
	return origin, fresh, stale
}

// TestResolveAnswersFromTheOriginNotFromTheClone is the property the pinned baseline
// exists for: two hosts whose clones were fetched at different times must resolve the
// same commit, and the stale one must not answer with its own older view.
func TestResolveAnswersFromTheOriginNotFromTheClone(t *testing.T) {
	origin, fresh, stale := originWithTwoClones(t)
	want := gitIn(t, origin, "rev-parse", "refs/heads/main")

	provider := newProvider(t, filepath.Join(t.TempDir(), "worktrees"))
	for _, clone := range []struct{ name, dir string }{{"fresh", fresh}, {"stale", stale}} {
		t.Run(clone.name, func(t *testing.T) {
			base, err := provider.Resolve(context.Background(), workspace.Request{Repo: clone.dir, BaseRef: "main"})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if base.Commit != want {
				t.Fatalf("commit = %s, want the origin's %s", base.Commit, want)
			}
			if base.Source != "origin" || base.Ref != "refs/heads/main" {
				t.Fatalf("base = %+v, want the origin answer for refs/heads/main", base)
			}
		})
	}

	// The stale clone's own view really is behind, which is what makes the test
	// meaningful rather than a coincidence.
	if local := gitIn(t, stale, "rev-parse", "refs/remotes/origin/main"); local == want {
		t.Fatalf("the stale clone is not stale: %s", local)
	}
}

// TestResolveIsHonestAboutARepositoryWithNoOrigin: without an origin there is no
// shared truth, and Source says so instead of pretending the answer is pinned.
func TestResolveIsHonestAboutARepositoryWithNoOrigin(t *testing.T) {
	repo := testRepo(t)
	provider := newProvider(t, filepath.Join(t.TempDir(), "worktrees"))

	base, err := provider.Resolve(context.Background(), workspace.Request{Repo: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if base.Source != "local" || base.Commit != gitIn(t, repo, "rev-parse", "HEAD") {
		t.Fatalf("base = %+v, want the local HEAD with source=local", base)
	}
	if _, err := provider.Resolve(context.Background(), workspace.Request{Repo: repo, BaseRef: "no-such-branch"}); err == nil {
		t.Fatal("a branch that does not exist was resolved")
	}
}

// TestPrepareProducesThePinnedCommitEvenFromAStaleClone is the other half: the point
// of resolving through the origin is worth nothing if the worktree is created from
// whatever the clone has. The stale clone has never seen the pinned commit, so
// Prepare has to fetch it — and it must not fall back to the older one.
func TestPrepareProducesThePinnedCommitEvenFromAStaleClone(t *testing.T) {
	origin, _, stale := originWithTwoClones(t)
	want := gitIn(t, origin, "rev-parse", "refs/heads/main")
	if _, err := exec.Command("git", "-C", stale, "cat-file", "-e", want+"^{commit}").CombinedOutput(); err == nil {
		t.Fatal("precondition: the stale clone should not have the pinned commit yet")
	}

	provider := newProvider(t, filepath.Join(t.TempDir(), "worktrees"))
	base, err := provider.Resolve(context.Background(), workspace.Request{Repo: stale, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	wt, err := provider.Prepare(context.Background(), workspace.Request{
		Repo: stale, TaskKey: "TEST-1", DefaultBranch: "main", BaseCommit: base.Commit,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if head := gitIn(t, wt.Path, "rev-parse", "HEAD"); head != want {
		t.Fatalf("worktree HEAD = %s, want the pinned %s", head, want)
	}
}

// TestPrepareRefusesToAttachToABranchThatIsNotADescendant closes the silent-adoption
// hole: a `flowhub/<key>` branch left over from an unrelated run must not be built
// on top of, because the agent's work would land on code the task never chose.
func TestPrepareRefusesToAttachToABranchThatIsNotADescendant(t *testing.T) {
	_, _, stale := originWithTwoClones(t)
	provider := newProvider(t, filepath.Join(t.TempDir(), "worktrees"))
	base, err := provider.Resolve(context.Background(), workspace.Request{Repo: stale, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// A branch with the task's name, started from an older commit: a leftover.
	older := gitIn(t, stale, "rev-parse", "refs/remotes/origin/main~0")
	gitIn(t, stale, "branch", DefaultBranchPrefix+"TEST-2", older)
	// Rewind the pin to a commit the branch does not descend from by rewriting the
	// branch onto an unrelated root, which is what a stray branch looks like.
	gitIn(t, stale, "checkout", "-q", "--orphan", "stray")
	gitIn(t, stale, "rm", "-q", "-rf", ".")
	if err := os.WriteFile(filepath.Join(stale, "OTHER.md"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, stale, "add", "-A")
	gitIn(t, stale, "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "-m", "unrelated root")
	stray := gitIn(t, stale, "rev-parse", "HEAD")
	gitIn(t, stale, "branch", "-f", DefaultBranchPrefix+"TEST-2", stray)
	gitIn(t, stale, "checkout", "-q", "main")

	_, err = provider.Prepare(context.Background(), workspace.Request{
		Repo: stale, TaskKey: "TEST-2", DefaultBranch: "main", BaseCommit: base.Commit,
	})
	if err == nil {
		t.Fatal("Prepare attached to a branch that is not a descendant of the pinned base")
	}
	if !strings.Contains(err.Error(), "not a descendant") || !strings.Contains(err.Error(), base.Commit) {
		t.Fatalf("the refusal does not explain itself: %v", err)
	}

	// The same branch, descended from the pin, is attached to rather than refused.
	gitIn(t, stale, "checkout", "-q", "-B", DefaultBranchPrefix+"TEST-3", base.Commit)
	gitIn(t, stale, "checkout", "-q", "main")
	wt, err := provider.Prepare(context.Background(), workspace.Request{
		Repo: stale, TaskKey: "TEST-3", DefaultBranch: "main", BaseCommit: base.Commit,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if head := gitIn(t, wt.Path, "rev-parse", "HEAD"); head != base.Commit {
		t.Fatalf("attached worktree HEAD = %s, want %s", head, base.Commit)
	}
}
