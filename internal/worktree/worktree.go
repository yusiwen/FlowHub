// Package worktree gives every task its own git worktree.
//
// Isolation is the point: an agent that edits code must not be able to touch
// another task's checkout, and must never work in the shared clone. The design
// documents put this in the core of the defence (one worktree per task, with the
// permission arbiter as the only other layer), so a failure here is a failure of
// the containment story, not a convenience bug.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DefaultBranchPrefix namespaces the branches FlowHub creates, so they are easy
// to list and delete without touching a human's branches.
const DefaultBranchPrefix = "flowhub/"

// commitPattern accepts a full or abbreviated commit id. Git itself decides whether
// the object exists; this only rejects a caller that passed a branch name, a path or
// an empty string where a pinned commit belongs.
var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ignoreEntry is added to the worktree's own exclude file (not the repository's
// .gitignore) so downloaded attachments and other task scratch never show up as
// untracked files in the agent's `git status`.
const ignoreEntry = ".flowhub/"

// ScratchDir is where a task's non-repository files live, inside the worktree.
const ScratchDir = ".flowhub"

// gitTimeout bounds one git invocation. `git worktree add` on a large repository
// can take a while, but it must never hang a worker forever.
const gitTimeout = 2 * time.Minute

// Options configures a Manager.
type Options struct {
	// Base is the directory that holds one subdirectory per task. It must be an
	// absolute path outside any repository; Validate rejects anything else.
	Base string
	// BranchPrefix defaults to DefaultBranchPrefix.
	BranchPrefix string
	// Fetch runs `git fetch --prune origin` before creating a worktree, so the
	// task starts from the newest default branch. Off by default: it needs
	// network access and credentials, and a failure must not block a task.
	Fetch bool
}

// Manager creates and removes task worktrees.
type Manager struct {
	opts Options
	// run is the git runner, replaced in tests.
	run func(ctx context.Context, dir string, args ...string) (string, error)
}

// Request describes one task's worktree.
type Request struct {
	// Repo is the canonical path of the shared clone.
	Repo string
	// TaskKey is the YouTrack issue ID, e.g. BEAP_BE-20.
	TaskKey string
	// DefaultBranch is the branch the task branches from, e.g. master. It must
	// already exist in the clone: guessing one would silently base the task on
	// the wrong code.
	DefaultBranch string
	// BaseCommit pins the commit the task starts from, as resolved from the origin
	// by Resolve. When it is set, Prepare produces exactly this commit — fetching
	// it if this clone has never seen it — and refuses to attach to an existing
	// branch that is not its descendant. Empty keeps the older behaviour of
	// branching from DefaultBranch as this clone happens to have it, which is only
	// correct for a single host.
	BaseCommit string
}

// Base is a resolved baseline.
type Base struct {
	// Commit is the full commit id the base ref points at.
	Commit string
	// Ref is the fully qualified ref it was resolved from, e.g. refs/heads/main.
	Ref string
	// Source is "origin" when `git ls-remote origin` answered, and "local" when the
	// repository has no origin remote to ask. The difference matters: only the
	// first is the same answer on every host.
	Source string
}

// Worktree is a prepared checkout.
type Worktree struct {
	Path   string
	Branch string
	Repo   string
	// Reused reports that the worktree already existed and was not created now.
	Reused bool
}

// New builds a Manager.
func New(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.Base) == "" {
		return nil, errors.New("worktree: Base is required")
	}
	if !filepath.IsAbs(opts.Base) {
		return nil, fmt.Errorf("worktree: Base %q must be absolute", opts.Base)
	}
	if opts.BranchPrefix == "" {
		opts.BranchPrefix = DefaultBranchPrefix
	}
	// Canonicalise the base once: opencode keys sessions by the literal directory
	// string, so /tmp and /private/tmp must not both appear.
	if resolved, err := filepath.EvalSymlinks(filepath.Dir(opts.Base)); err == nil {
		opts.Base = filepath.Join(resolved, filepath.Base(opts.Base))
	}
	return &Manager{opts: opts, run: runGit}, nil
}

// Base returns the directory that holds the task worktrees.
func (m *Manager) Base() string { return m.opts.Base }

// Resolve reports the commit baseRef points at *as the origin sees it*, not as this
// clone happens to have it.
//
// This is the whole point of pinning a baseline: with more than one host there is
// more than one clone, and a clone's `origin/<branch>` records whatever it last
// fetched. Asking the origin makes the answer the same on every host, so two tasks
// for one project spread across two machines start from the same commit. A stale
// clone is healed by Prepare, which fetches the pinned commit only when the object
// is missing locally.
//
// A repository with **no** origin remote has no shared truth to ask: the local ref
// is the honest answer there, and Source says so, because a caller that logs which
// one happened is the difference between "pinned" and "assumed".
func (m *Manager) Resolve(ctx context.Context, repo, baseRef string) (Base, error) {
	repo = canonical(repo)
	ref := strings.TrimSpace(baseRef)
	if ref == "" {
		return Base{}, errors.New("worktree: a base ref is required to pin a baseline")
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		return Base{}, fmt.Errorf("worktree: %s is not a git work tree", repo)
	}

	hasOrigin, err := m.hasOrigin(ctx, repo)
	if err != nil {
		return Base{}, err
	}
	if !hasOrigin {
		out, err := m.run(ctx, repo, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return Base{}, fmt.Errorf("worktree: %s has no origin remote and no local %s to pin", repo, ref)
		}
		return Base{Commit: strings.TrimSpace(out), Ref: ref, Source: "local"}, nil
	}

	out, err := m.run(ctx, repo, "ls-remote", "origin", ref)
	if err != nil {
		return Base{}, fmt.Errorf("worktree: resolve %s on origin: %w", ref, err)
	}
	commit, err := parseLsRemote(out, ref)
	if err != nil {
		return Base{}, fmt.Errorf("worktree: %s in %s: %w", ref, repo, err)
	}
	return Base{Commit: commit, Ref: ref, Source: "origin"}, nil
}

// parseLsRemote reads one ref out of `git ls-remote` output, which is
// "<sha>\t<ref>" per line, oldest format included.
func parseLsRemote(output, ref string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == ref || strings.HasSuffix(fields[1], "/"+strings.TrimPrefix(ref, "refs/heads/")) {
			if !commitPattern.MatchString(fields[0]) {
				return "", fmt.Errorf("origin answered %q for %s", fields[0], ref)
			}
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("origin has no %s", ref)
}

// hasOrigin reports whether the clone has an origin remote to ask.
func (m *Manager) hasOrigin(ctx context.Context, repo string) (bool, error) {
	out, err := m.run(ctx, repo, "remote")
	if err != nil {
		return false, err
	}
	for _, name := range strings.Fields(out) {
		if name == "origin" {
			return true, nil
		}
	}
	return false, nil
}

// ensureCommit makes sure a pinned commit is present locally, fetching it only when
// it is not.
//
// A stale clone heals on demand rather than on every task: the object store is
// shared by every worktree of the clone, so the fetch happens once. A host that
// cannot obtain the commit fails here instead of quietly basing the task on
// whatever it already had — which is the failure the pin exists to prevent.
func (m *Manager) ensureCommit(ctx context.Context, req Request, commit string) error {
	if _, err := m.run(ctx, req.Repo, "cat-file", "-e", commit+"^{commit}"); err == nil {
		return nil
	}
	// Asking for the commit by id works on a server that allows it; fetching the
	// branch is the fallback, and it brings the commit when the commit is an
	// ancestor of that branch — which it is, unless the branch was rewritten.
	if _, err := m.run(ctx, req.Repo, "fetch", "origin", commit); err != nil {
		if _, branchErr := m.run(ctx, req.Repo, "fetch", "origin", req.DefaultBranch); branchErr != nil {
			return fmt.Errorf("worktree: the pinned commit %s is not in %s and could not be fetched: %w", commit, req.Repo, branchErr)
		}
	}
	if _, err := m.run(ctx, req.Repo, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Errorf("worktree: the pinned commit %s is not reachable from origin in %s", commit, req.Repo)
	}
	return nil
}

// isAncestor reports whether ancestor is reachable from descendant. It is how the
// attach path decides that a branch which already exists belongs to this task.
func (m *Manager) isAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	if _, err := m.run(ctx, repo, "merge-base", "--is-ancestor", ancestor, descendant); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			// Exit code 1 is git's "no": not an error, an answer.
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Prepare creates (or reuses) the worktree for one task.
//
// It is idempotent: calling it twice for the same task returns the same path, so
// a retried delivery can never create a second checkout or a second branch.
func (m *Manager) Prepare(ctx context.Context, req Request) (Worktree, error) {
	req.Repo = canonical(req.Repo)
	if err := m.validate(req); err != nil {
		return Worktree{}, err
	}

	path := filepath.Join(m.opts.Base, req.TaskKey)
	branch := m.opts.BranchPrefix + req.TaskKey

	// Reuse an existing worktree: a second event for the same task must land in
	// the same checkout, otherwise the agent loses its own previous work.
	//
	// Git's bookkeeping is not proof that the directory is still there. A human
	// running `rm -rf`, or a cleanup that died halfway, leaves the worktree
	// registered and missing; reusing it then fails inside a directory that does not
	// exist ("not a git repository"), and the task can never start again. So the
	// registration is verified, and a stale one is pruned rather than trusted.
	if registered, err := m.registered(ctx, req.Repo, path); err != nil {
		return Worktree{}, err
	} else if registered {
		switch usable, err := m.usable(ctx, path); {
		case err != nil:
			return Worktree{}, err
		case usable:
			if err := m.ensureScratch(path); err != nil {
				return Worktree{}, err
			}
			return Worktree{Path: path, Branch: branch, Repo: req.Repo, Reused: true}, nil
		default:
			// Prune clears the bookkeeping for every worktree whose directory is
			// gone, which is what makes recreating this one possible.
			if _, err := m.run(ctx, req.Repo, "worktree", "prune"); err != nil {
				return Worktree{}, err
			}
		}
	}

	if base := strings.TrimSpace(req.BaseCommit); base != "" {
		if err := m.ensureCommit(ctx, req, base); err != nil {
			return Worktree{}, err
		}
	}

	if err := os.MkdirAll(m.opts.Base, 0o700); err != nil {
		return Worktree{}, fmt.Errorf("worktree: create base %s: %w", m.opts.Base, err)
	}
	if m.opts.Fetch {
		// A failed fetch is not fatal: the task should still start, from whatever
		// the clone already has.
		_, _ = m.run(ctx, req.Repo, "fetch", "--prune", "origin")
	}

	// A directory can be sitting at the target path without belonging to a worktree
	// git knows about: a half-finished cleanup, or the scratch directory an earlier
	// failed run created. `git worktree add` refuses to write into it, so it has to
	// be dealt with — and never by deleting content FlowHub does not recognise.
	if moved, err := m.clearForCreate(path); err != nil {
		return Worktree{}, err
	} else if moved != "" {
		// Nothing is thrown away: the operator can look at what was there.
		_ = moved
	}

	if exists, err := m.branchExists(ctx, req.Repo, branch); err != nil {
		return Worktree{}, err
	} else if exists {
		// The branch survived a previous task; attach to it rather than failing —
		// but only when it descends from the pinned baseline. A branch with this
		// name from an unrelated run would otherwise be adopted silently, and the
		// agent's work would land on top of code the task never chose.
		if base := strings.TrimSpace(req.BaseCommit); base != "" {
			descends, err := m.isAncestor(ctx, req.Repo, base, branch)
			if err != nil {
				return Worktree{}, err
			}
			if !descends {
				return Worktree{}, fmt.Errorf("worktree: branch %s already exists in %s but is not a descendant of the pinned base %s; remove it or start a new task",
					branch, req.Repo, base)
			}
		}
		if _, err := m.run(ctx, req.Repo, "worktree", "add", path, branch); err != nil {
			return Worktree{}, err
		}
	} else {
		// The pinned commit wins over the branch name: the branch may have moved on
		// since the task was created, and the task's base must not.
		start := req.DefaultBranch
		if base := strings.TrimSpace(req.BaseCommit); base != "" {
			start = base
		}
		if _, err := m.run(ctx, req.Repo, "worktree", "add", "-b", branch, path, start); err != nil {
			return Worktree{}, err
		}
	}

	if err := m.ensureScratch(path); err != nil {
		return Worktree{}, err
	}
	return Worktree{Path: path, Branch: branch, Repo: req.Repo}, nil
}

// Remove drops a task's worktree. The branch is kept: it holds the agent's work
// and is what a human reviews.
func (m *Manager) Remove(ctx context.Context, repo string, wt Worktree) error {
	args := []string{"worktree", "remove"}
	if _, err := os.Stat(wt.Path); err != nil {
		// Already gone: prune the bookkeeping and move on.
		_, _ = m.run(ctx, repo, "worktree", "prune")
		return nil
	}
	if _, err := m.run(ctx, repo, append(args, wt.Path)...); err != nil {
		return err
	}
	return nil
}

// List returns the worktrees git knows about for a repository.
func (m *Manager) List(ctx context.Context, repo string) ([]string, error) {
	out, err := m.run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if after, found := strings.CutPrefix(strings.TrimSpace(line), "worktree "); found {
			paths = append(paths, after)
		}
	}
	return paths, nil
}

func (m *Manager) validate(req Request) error {
	if strings.TrimSpace(req.Repo) == "" {
		return errors.New("worktree: Request.Repo is required")
	}
	if strings.TrimSpace(req.TaskKey) == "" {
		return errors.New("worktree: Request.TaskKey is required")
	}
	// A task key becomes a path segment and a branch name, so keep it boring.
	if strings.ContainsAny(req.TaskKey, "/\\ \t\n") || strings.HasPrefix(req.TaskKey, ".") {
		return fmt.Errorf("worktree: TaskKey %q is not a safe path or branch segment", req.TaskKey)
	}
	if strings.TrimSpace(req.DefaultBranch) == "" {
		return errors.New("worktree: Request.DefaultBranch is required (guessing one would base the task on the wrong code)")
	}
	if base := strings.TrimSpace(req.BaseCommit); base != "" && !commitPattern.MatchString(base) {
		return fmt.Errorf("worktree: Request.BaseCommit %q is not a commit id", req.BaseCommit)
	}
	if !filepath.IsAbs(req.Repo) {
		return fmt.Errorf("worktree: Repo %q must be absolute", req.Repo)
	}
	if _, err := os.Stat(filepath.Join(req.Repo, ".git")); err != nil {
		return fmt.Errorf("worktree: %s is not a git work tree", req.Repo)
	}
	target := filepath.Join(m.opts.Base, req.TaskKey)
	if isBeneath(target, req.Repo) {
		return fmt.Errorf("worktree: %s would be created inside the repository %s; keep task checkouts outside", target, req.Repo)
	}
	if isBeneath(m.opts.Base, req.Repo) {
		return fmt.Errorf("worktree: Base %s is inside the repository %s", m.opts.Base, req.Repo)
	}
	exists, err := m.branchExists(context.Background(), req.Repo, req.DefaultBranch)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("worktree: default branch %q does not exist in %s", req.DefaultBranch, req.Repo)
	}
	return nil
}

// canonical resolves symlinks so that the paths compared here are the ones
// opencode will see: it keys sessions and projects by the literal directory
// string, so /tmp and /private/tmp must never both appear.
func canonical(path string) string {
	if strings.TrimSpace(path) == "" {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// ensureScratch creates the task's scratch directory and makes git ignore it via
// the worktree's own exclude file, so the repository's .gitignore stays untouched.
func (m *Manager) ensureScratch(path string) error {
	scratch := filepath.Join(path, ScratchDir)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return fmt.Errorf("worktree: create %s: %w", scratch, err)
	}
	// Ask git where this worktree's exclude file lives: for a linked worktree it
	// is inside the repository's .git/worktrees/<name>/, not .git/info/exclude.
	exclude, err := m.run(context.Background(), path, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(path, exclude)
	}
	existing, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("worktree: read %s: %w", exclude, err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == ignoreEntry {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return err
	}
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += ignoreEntry + "\n"
	if err := os.WriteFile(exclude, []byte(content), 0o644); err != nil {
		return fmt.Errorf("worktree: write %s: %w", exclude, err)
	}
	return nil
}

// clearForCreate makes the target path available for a fresh checkout.
//
// A directory holding nothing, or holding only FlowHub's own scratch, is removed:
// neither can be anybody's work. Anything else is moved aside rather than deleted,
// because a checkout that git no longer recognises may still hold uncommitted work
// and this code cannot tell. The moved path is returned so the caller can report it.
func (m *Manager) clearForCreate(path string) (string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("worktree: inspect %s: %w", path, err)
	}
	onlyScratch := true
	for _, entry := range entries {
		if entry.Name() != ScratchDir {
			onlyScratch = false
			break
		}
	}
	if len(entries) == 0 || onlyScratch {
		if err := os.RemoveAll(path); err != nil {
			return "", fmt.Errorf("worktree: clear %s: %w", path, err)
		}
		return "", nil
	}
	moved := fmt.Sprintf("%s.stale-%d", path, time.Now().Unix())
	if err := os.Rename(path, moved); err != nil {
		return "", fmt.Errorf("worktree: %s is in the way and could not be moved aside: %w", path, err)
	}
	return moved, nil
}

// usable reports whether the path is a working tree git can actually use. It is
// the check that turns a stale registration into a recreate instead of a failure.
func (m *Manager) usable(ctx context.Context, path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	out, err := m.run(ctx, path, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		// The directory is there but git cannot use it: treat it as unusable rather
		// than failing the task forever.
		return false, nil
	}
	return strings.TrimSpace(out) == "true", nil
}

func (m *Manager) registered(ctx context.Context, repo, path string) (bool, error) {
	paths, err := m.List(ctx, repo)
	if err != nil {
		return false, err
	}
	for _, candidate := range paths {
		if samePath(candidate, path) {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) branchExists(ctx context.Context, repo, branch string) (bool, error) {
	_, err := m.run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	// A missing ref is the expected "no" answer; anything else (not a repository,
	// git missing) must surface.
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	if strings.Contains(err.Error(), "exit status 1") {
		return false, nil
	}
	return false, err
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, message)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func isBeneath(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	if err != nil || relative == "." {
		return false
	}
	return !strings.HasPrefix(relative, "..")
}
