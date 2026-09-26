package localworktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/workspace"
)

// writeRepo builds a minimal git work tree on disk: the validator only needs a .git
// directory and, when a remote is given, a config file naming it. No git binary and
// no commit are involved.
func writeRepo(t *testing.T, dir, remote string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "[core]\n\trepositoryformatversion = 0\n"
	if remote != "" {
		config += "[remote \"origin\"]\n\turl = " + remote + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestValidatorReportsFilesystemProblems covers the checks ADR 0001 moved off the
// control plane and behind this provider: they are facts of the machine that will
// prepare the checkout, so a provider that lives elsewhere answers them from that
// host's attestation instead.
func TestValidatorReportsFilesystemProblems(t *testing.T) {
	base := t.TempDir()
	good := writeRepo(t, filepath.Join(base, "good"), "git@example.cn:me/app.git")
	notGit := filepath.Join(base, "not-git")
	if err := os.MkdirAll(notGit, 0o755); err != nil {
		t.Fatal(err)
	}
	wrongRemote := writeRepo(t, filepath.Join(base, "wrong-remote"), "git@example.cn:me/other.git")
	worktrees := filepath.Join(base, "worktrees")

	cases := map[string]struct {
		entry workspace.Entry
		want  string
	}{
		"missing path": {
			entry: workspace.Entry{Repo: filepath.Join(base, "absent"), DefaultBranch: "main", Base: worktrees},
			want:  "no such file",
		},
		"no path configured": {
			entry: workspace.Entry{DefaultBranch: "main", Base: worktrees},
			want:  "repo.path is not configured",
		},
		"not a git work tree": {
			entry: workspace.Entry{Repo: notGit, DefaultBranch: "main", Base: worktrees},
			want:  "not a git work tree",
		},
		"remote mismatch": {
			entry: workspace.Entry{Repo: wrongRemote, Remote: "git@example.cn:me/app.git", DefaultBranch: "main", Base: worktrees},
			want:  "but the mapping declares",
		},
		"declared remote but none configured": {
			entry: workspace.Entry{Repo: writeRepo(t, filepath.Join(base, "no-remote"), ""), Remote: "git@example.cn:me/app.git", DefaultBranch: "main", Base: worktrees},
			want:  "has no remote.origin.url",
		},
		"worktrees inside the repo": {
			entry: workspace.Entry{Repo: good, DefaultBranch: "main", Base: filepath.Join(good, "wt")},
			want:  "is inside the repository",
		},
		"worktrees equal to the repo": {
			entry: workspace.Entry{Repo: good, DefaultBranch: "main", Base: good},
			want:  "must differ from repo.path",
		},
		"no worktrees directory": {
			entry: workspace.Entry{Repo: good, DefaultBranch: "main"},
			want:  "no directory is configured for task checkouts",
		},
		"no default branch": {
			entry: workspace.Entry{Repo: good, Base: worktrees},
			want:  "repo.default_branch is empty",
		},
		"worktrees path is a file": {
			entry: workspace.Entry{Repo: good, DefaultBranch: "main", Base: writeFileIn(t, base, "not-a-dir")},
			want:  "exists but is not a directory",
		},
		"ok": {
			entry: workspace.Entry{Repo: good, Remote: "git@example.cn:me/app.git", DefaultBranch: "main", Base: worktrees},
			want:  "",
		},
		"ok without a declared remote": {
			entry: workspace.Entry{Repo: good, DefaultBranch: "main", Base: worktrees},
			want:  "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			problems := workspace.ValidateEntries(Validator{}, []workspace.Entry{withLabel(tc.entry, "TEST")})
			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("Validate = %v, want none", problems)
				}
				return
			}
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("Validate = %v, want %q", problems, tc.want)
			}
			// The label is what turns a problem into "which project is this about",
			// so it is part of the contract rather than decoration.
			if !strings.HasPrefix(joined, "TEST: ") {
				t.Fatalf("Validate = %v, want the entry label prefixed", problems)
			}
		})
	}
}

func withLabel(entry workspace.Entry, label string) workspace.Entry {
	entry.Label = label
	return entry
}

func writeFileIn(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSameRemoteIgnoresSpelling(t *testing.T) {
	for _, pair := range [][2]string{
		{"git@example.cn:me/app.git", "git@example.cn:me/app"},
		{"https://example.cn/me/app.git", "https://example.cn/me/app/"},
		{" git@example.cn:me/app.git ", "git@example.cn:me/app.git"},
	} {
		if !sameRemote(pair[0], pair[1]) {
			t.Errorf("sameRemote(%q, %q) = false, want true", pair[0], pair[1])
		}
	}
	if sameRemote("git@example.cn:me/app.git", "git@example.cn:me/other.git") {
		t.Error("different repositories compared equal")
	}
}

// TestCheckReportsDrift exercises the capability nothing calls yet. It has to be
// right before a provider whose disk belongs to another host can rely on it: "the
// directory is gone" and "the directory belongs to a different repository" are the
// two ways a recorded workspace stops being the task's own.
func TestCheckReportsDrift(t *testing.T) {
	repo := testRepo(t)
	provider := newProvider(t, filepath.Join(t.TempDir(), "worktrees"))
	handle, err := provider.Prepare(context.Background(), workspace.Request{
		Repo: repo, TaskKey: "TEST-CHECK", DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := provider.Check(context.Background(), handle); err != nil {
		t.Fatalf("Check on a fresh workspace: %v", err)
	}

	// Gone.
	if err := os.RemoveAll(handle.Path); err != nil {
		t.Fatal(err)
	}
	if err := provider.Check(context.Background(), handle); err == nil {
		t.Fatal("Check accepted a workspace whose directory is gone")
	}

	// Present, but not a worktree of the repository the task was created against.
	other := testRepo(t)
	provider2 := newProvider(t, filepath.Join(t.TempDir(), "worktrees2"))
	moved, err := provider2.Prepare(context.Background(), workspace.Request{
		Repo: other, TaskKey: "TEST-CHECK", DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("Prepare in the second repository: %v", err)
	}
	moved.Repo = handle.Repo
	if err := provider2.Check(context.Background(), moved); err == nil {
		t.Fatal("Check accepted a worktree that belongs to a different repository")
	}

	// A handle with no path is a caller bug, not a drift.
	if err := provider.Check(context.Background(), workspace.Handle{Repo: repo}); err == nil {
		t.Fatal("Check accepted a handle with no path")
	}
}

// TestPreparePublishesWhatTheCallerRecords pins the fields the dispatcher stores:
// without them a task's workspace cannot be attested or continued.
func TestPreparePublishesWhatTheCallerRecords(t *testing.T) {
	repo := testRepo(t)
	provider := newProvider(t, filepath.Join(t.TempDir(), "worktrees"))
	pinned := gitIn(t, repo, "rev-parse", "HEAD")

	handle, err := provider.Prepare(context.Background(), workspace.Request{
		Repo: repo, TaskKey: "TEST-4", DefaultBranch: "main", BaseCommit: pinned,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if handle.Provider != Name {
		t.Errorf("Provider = %q, want %q", handle.Provider, Name)
	}
	if handle.BaseCommit != pinned {
		t.Errorf("BaseCommit = %q, want the pin %q", handle.BaseCommit, pinned)
	}
	if handle.Reused {
		t.Error("a freshly created workspace reported itself as reused")
	}
	if !strings.HasSuffix(handle.Path, string(filepath.Separator)+"TEST-4") {
		t.Errorf("Path = %q, want it to end in the task key", handle.Path)
	}
}

// ExampleValidator shows the composition a caller uses: the provider answers for one
// entry and workspace.ValidateEntries supplies the label. It uses an entry with no
// repository at all, so the output does not depend on the machine running the test.
func ExampleValidator() {
	problems := workspace.ValidateEntries(Validator{}, []workspace.Entry{{Label: "TEST"}})
	fmt.Println(problems[0])
	// Output: TEST: repo.path is not configured, so there is no repository to prepare a task in
}
