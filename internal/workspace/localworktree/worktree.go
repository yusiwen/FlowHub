// Package localworktree gives every task its own git worktree on the machine
// FlowHub itself runs on, behind the workspace seam.
//
// Isolation is the point: an agent that edits code must not be able to touch
// another task's checkout, and must never work in the shared clone. The design
// documents put this in the core of the defence (one worktree per task, with the
// permission arbiter as the only other layer), so a failure here is a failure of
// the containment story, not a convenience bug.
//
// It is the "co-located" provider of ADR 0001: FlowHub and the runtime share a
// filesystem, so the routing entry's repository path is real here. A provider that
// lives on another host answers the same interface without a local path at all.
package localworktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/workspace"
)

// Name is the provider's name, recorded with every task it prepares.
const Name = "localworktree"

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

// Options configures a Provider.
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

// Provider creates and removes task worktrees on this host.
type Provider struct {
	opts Options
	// run is the git runner, replaced in tests.
	run func(ctx context.Context, dir string, args ...string) (string, error)
}

// New builds a Provider.
func New(opts Options) (*Provider, error) {
	if strings.TrimSpace(opts.Base) == "" {
		return nil, errors.New("localworktree: Base is required")
	}
	if !filepath.IsAbs(opts.Base) {
		return nil, fmt.Errorf("localworktree: Base %q must be absolute", opts.Base)
	}
	if opts.BranchPrefix == "" {
		opts.BranchPrefix = DefaultBranchPrefix
	}
	// Canonicalise the base once: the runtime keys sessions by the literal directory
	// string, so /tmp and /private/tmp must not both appear.
	if resolved, err := filepath.EvalSymlinks(filepath.Dir(opts.Base)); err == nil {
		opts.Base = filepath.Join(resolved, filepath.Base(opts.Base))
	}
	return &Provider{opts: opts, run: runGit}, nil
}

// Name implements workspace.Workspace.
func (p *Provider) Name() string { return Name }

// Base returns the directory that holds the task worktrees.
func (p *Provider) Base() string { return p.opts.Base }

// Resolve reports the commit BaseRef points at *as the origin sees it*, not as this
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
func (p *Provider) Resolve(ctx context.Context, req workspace.Request) (workspace.Base, error) {
	repo := canonical(req.Repo)
	ref := strings.TrimSpace(req.BaseRef)
	if ref == "" {
		return workspace.Base{}, errors.New("localworktree: a base ref is required to pin a baseline")
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		return workspace.Base{}, fmt.Errorf("localworktree: %s is not a git work tree", repo)
	}

	hasOrigin, err := p.hasOrigin(ctx, repo)
	if err != nil {
		return workspace.Base{}, err
	}
	if !hasOrigin {
		out, err := p.run(ctx, repo, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return workspace.Base{}, fmt.Errorf("localworktree: %s has no origin remote and no local %s to pin", repo, ref)
		}
		return workspace.Base{Commit: strings.TrimSpace(out), Ref: ref, Source: "local"}, nil
	}

	out, err := p.run(ctx, repo, "ls-remote", "origin", ref)
	if err != nil {
		return workspace.Base{}, fmt.Errorf("localworktree: resolve %s on origin: %w", ref, err)
	}
	commit, err := parseLsRemote(out, ref)
	if err != nil {
		return workspace.Base{}, fmt.Errorf("localworktree: %s in %s: %w", ref, repo, err)
	}
	return workspace.Base{Commit: commit, Ref: ref, Source: "origin"}, nil
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
func (p *Provider) hasOrigin(ctx context.Context, repo string) (bool, error) {
	out, err := p.run(ctx, repo, "remote")
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
func (p *Provider) ensureCommit(ctx context.Context, req workspace.Request, commit string) error {
	if _, err := p.run(ctx, req.Repo, "cat-file", "-e", commit+"^{commit}"); err == nil {
		return nil
	}
	// Asking for the commit by id works on a server that allows it; fetching the
	// branch is the fallback, and it brings the commit when the commit is an
	// ancestor of that branch — which it is, unless the branch was rewritten.
	if _, err := p.run(ctx, req.Repo, "fetch", "origin", commit); err != nil {
		if _, branchErr := p.run(ctx, req.Repo, "fetch", "origin", req.DefaultBranch); branchErr != nil {
			return fmt.Errorf("localworktree: the pinned commit %s is not in %s and could not be fetched: %w", commit, req.Repo, branchErr)
		}
	}
	if _, err := p.run(ctx, req.Repo, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Errorf("localworktree: the pinned commit %s is not reachable from origin in %s", commit, req.Repo)
	}
	return nil
}

// isAncestor reports whether ancestor is reachable from descendant. It is how the
// attach path decides that a branch which already exists belongs to this task.
func (p *Provider) isAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	if _, err := p.run(ctx, repo, "merge-base", "--is-ancestor", ancestor, descendant); err != nil {
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
func (p *Provider) Prepare(ctx context.Context, req workspace.Request) (workspace.Handle, error) {
	req.Repo = canonical(req.Repo)
	if err := p.validate(req); err != nil {
		return workspace.Handle{}, err
	}

	name := dirName(req.TaskKey)
	path := filepath.Join(p.opts.Base, name)
	branch := p.opts.BranchPrefix + name

	// Reuse an existing worktree: a second event for the same task must land in
	// the same checkout, otherwise the agent loses its own previous work.
	//
	// Git's bookkeeping is not proof that the directory is still there. A human
	// running `rm -rf`, or a cleanup that died halfway, leaves the worktree
	// registered and missing; reusing it then fails inside a directory that does not
	// exist ("not a git repository"), and the task can never start again. So the
	// registration is verified, and a stale one is pruned rather than trusted.
	if registered, err := p.registered(ctx, req.Repo, path); err != nil {
		return workspace.Handle{}, err
	} else if registered {
		switch usable, err := p.usable(ctx, path); {
		case err != nil:
			return workspace.Handle{}, err
		case usable:
			if err := p.ensureScratch(path); err != nil {
				return workspace.Handle{}, err
			}
			return p.handle(path, req.Repo, req.BaseCommit, true), nil
		default:
			// Prune clears the bookkeeping for every worktree whose directory is
			// gone, which is what makes recreating this one possible.
			if _, err := p.run(ctx, req.Repo, "worktree", "prune"); err != nil {
				return workspace.Handle{}, err
			}
		}
	}

	if base := strings.TrimSpace(req.BaseCommit); base != "" {
		if err := p.ensureCommit(ctx, req, base); err != nil {
			return workspace.Handle{}, err
		}
	}

	if err := os.MkdirAll(p.opts.Base, 0o700); err != nil {
		return workspace.Handle{}, fmt.Errorf("localworktree: create base %s: %w", p.opts.Base, err)
	}
	if p.opts.Fetch {
		// A failed fetch is not fatal: the task should still start, from whatever
		// the clone already has.
		_, _ = p.run(ctx, req.Repo, "fetch", "--prune", "origin")
	}

	// A directory can be sitting at the target path without belonging to a worktree
	// git knows about: a half-finished cleanup, or the scratch directory an earlier
	// failed run created. `git worktree add` refuses to write into it, so it has to
	// be dealt with — and never by deleting content FlowHub does not recognise.
	if moved, err := p.clearForCreate(path); err != nil {
		return workspace.Handle{}, err
	} else if moved != "" {
		// Nothing is thrown away: the operator can look at what was there.
		_ = moved
	}

	if exists, err := p.branchExists(ctx, req.Repo, branch); err != nil {
		return workspace.Handle{}, err
	} else if exists {
		// The branch survived a previous task; attach to it rather than failing —
		// but only when it descends from the pinned baseline. A branch with this
		// name from an unrelated run would otherwise be adopted silently, and the
		// agent's work would land on top of code the task never chose.
		if base := strings.TrimSpace(req.BaseCommit); base != "" {
			descends, err := p.isAncestor(ctx, req.Repo, base, branch)
			if err != nil {
				return workspace.Handle{}, err
			}
			if !descends {
				return workspace.Handle{}, fmt.Errorf("localworktree: branch %s already exists in %s but is not a descendant of the pinned base %s; remove it or start a new task",
					branch, req.Repo, base)
			}
		}
		if _, err := p.run(ctx, req.Repo, "worktree", "add", path, branch); err != nil {
			return workspace.Handle{}, err
		}
	} else {
		// The pinned commit wins over the branch name: the branch may have moved on
		// since the task was created, and the task's base must not.
		start := req.DefaultBranch
		if base := strings.TrimSpace(req.BaseCommit); base != "" {
			start = base
		}
		if _, err := p.run(ctx, req.Repo, "worktree", "add", "-b", branch, path, start); err != nil {
			return workspace.Handle{}, err
		}
	}

	if err := p.ensureScratch(path); err != nil {
		return workspace.Handle{}, err
	}
	return p.handle(path, req.Repo, req.BaseCommit, false), nil
}

// dirName maps a task key to one directory and branch name on this filesystem.
//
// The rules, in order: keep letters, digits, underscore, dot and dash; turn every
// other run of characters into a single dash (with runs collapsed, and no dash at
// the start); trim dots and dashes from the ends. A tracker key like
// `owner/repo#42` therefore becomes `owner-repo-42`. A key with nothing usable in it
// at all still gets a stable name, `task-<digest>`.
//
// Mapping is lossy — `a/b` and `a-b` would collide — so a key that had to change
// gets a short digest of the *original* appended, which makes two different keys
// produce two different names without making the readable case ugly. A key that is
// already a safe segment is used verbatim, so YouTrack's `TEST-17` keeps the branch
// name `flowhub/TEST-17` it has always had.
//
// The result is guaranteed to have no separator, no `..` and no leading dot, which
// is what keeps the directory inside the base.
func dirName(taskKey string) string {
	key := strings.TrimSpace(taskKey)
	if key == "" {
		return ""
	}
	var builder strings.Builder
	previousDash := false
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			builder.WriteRune(r)
			previousDash = false
		case r == '.' || r == '-':
			// A dot or a dash is kept as it was written, but never at the start of the
			// name and never doubled: the trimming pass below removes a trailing one.
			if builder.Len() == 0 || previousDash {
				continue
			}
			builder.WriteRune(r)
			previousDash = r == '-'
		default:
			if builder.Len() == 0 || previousDash {
				continue
			}
			builder.WriteRune('-')
			previousDash = true
		}
	}
	name := strings.Trim(builder.String(), ".-")
	if name == key {
		return name
	}
	if name == "" {
		// Nothing usable survived: fall back to a pure digest so the task still has a
		// stable, safe name instead of no name at all.
		return "task-" + shortDigest(key)
	}
	return name + "-" + shortDigest(key)
}

// shortDigest is a short, stable fingerprint of a task key, used only to keep two
// keys that map to the same readable name apart.
func shortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}

// handle renders the workspace handle a caller records. Repo is this provider's own
// attestation: the path it actually prepared the task in, which the dispatcher
// compares against the routing entry rather than trusting a string that matched once.
func (p *Provider) handle(path, repo, baseCommit string, reused bool) workspace.Handle {
	return workspace.Handle{
		Provider:   Name,
		Path:       path,
		Repo:       repo,
		BaseCommit: strings.TrimSpace(baseCommit),
		Reused:     reused,
	}
}

// Check reports drift: the directory is gone, git can no longer use it, or it is no
// longer a worktree of the repository the task was created against.
//
// Nothing calls it yet: the dispatcher still trusts the recorded path, which is
// correct while the provider shares FlowHub's filesystem and wrong as soon as the
// disk belongs to another host. Implementing it now is what makes that switch a
// wiring change instead of a redesign.
func (p *Provider) Check(ctx context.Context, handle workspace.Handle) error {
	path := strings.TrimSpace(handle.Path)
	if path == "" {
		return errors.New("localworktree: the workspace handle has no path")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("localworktree: %s is gone: %w", path, err)
	}
	usable, err := p.usable(ctx, path)
	if err != nil {
		return err
	}
	if !usable {
		return fmt.Errorf("localworktree: %s is not a usable git work tree", path)
	}
	repo := strings.TrimSpace(handle.Repo)
	if repo == "" {
		// No attestation to compare against: the caller recorded a path without a
		// repository, which is honest only for a single-host deployment.
		return nil
	}
	registered, err := p.registered(ctx, canonical(repo), path)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("localworktree: %s is not a worktree of %s", path, repo)
	}
	return nil
}

// Remove drops a task's worktree. The branch is kept: it holds the agent's work
// and is what a human reviews.
func (p *Provider) Remove(ctx context.Context, handle workspace.Handle) error {
	repo := canonical(handle.Repo)
	if _, err := os.Stat(handle.Path); err != nil {
		// Already gone: prune the bookkeeping and move on.
		_, _ = p.run(ctx, repo, "worktree", "prune")
		return nil
	}
	if _, err := p.run(ctx, repo, "worktree", "remove", handle.Path); err != nil {
		return err
	}
	return nil
}

// List returns the worktrees git knows about for a repository.
func (p *Provider) List(ctx context.Context, repo string) ([]string, error) {
	out, err := p.run(ctx, repo, "worktree", "list", "--porcelain")
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

func (p *Provider) validate(req workspace.Request) error {
	if strings.TrimSpace(req.Repo) == "" {
		return errors.New("localworktree: Request.Repo is required")
	}
	if strings.TrimSpace(req.TaskKey) == "" {
		return errors.New("localworktree: Request.TaskKey is required")
	}
	// A task key becomes a path segment and a branch name. A tracker's own key is
	// not always a safe segment — Gitea identifies an issue as `owner/repo#42` — so
	// the key is *mapped* rather than refused, and what has to stay safe is the
	// result: see dirName. This is the provider's job because the filesystem naming
	// is the provider's fact, and two providers may name the same task differently.
	if name := dirName(req.TaskKey); name == "" || name == "." || name == ".." {
		return fmt.Errorf("localworktree: TaskKey %q has no usable characters for a path or branch segment", req.TaskKey)
	}
	if strings.TrimSpace(req.DefaultBranch) == "" {
		return errors.New("localworktree: Request.DefaultBranch is required (guessing one would base the task on the wrong code)")
	}
	if base := strings.TrimSpace(req.BaseCommit); base != "" && !commitPattern.MatchString(base) {
		return fmt.Errorf("localworktree: Request.BaseCommit %q is not a commit id", req.BaseCommit)
	}
	if !filepath.IsAbs(req.Repo) {
		return fmt.Errorf("localworktree: Repo %q must be absolute", req.Repo)
	}
	if _, err := os.Stat(filepath.Join(req.Repo, ".git")); err != nil {
		return fmt.Errorf("localworktree: %s is not a git work tree", req.Repo)
	}
	target := filepath.Join(p.opts.Base, dirName(req.TaskKey))
	if isBeneath(target, req.Repo) {
		return fmt.Errorf("localworktree: %s would be created inside the repository %s; keep task checkouts outside", target, req.Repo)
	}
	if isBeneath(p.opts.Base, req.Repo) {
		return fmt.Errorf("localworktree: Base %s is inside the repository %s", p.opts.Base, req.Repo)
	}
	exists, err := p.branchExists(context.Background(), req.Repo, req.DefaultBranch)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("localworktree: default branch %q does not exist in %s", req.DefaultBranch, req.Repo)
	}
	return nil
}

// Validator checks a routing entry before any provider instance exists, which is
// what startup needs: a refused entry must leave nothing behind, so the check cannot
// depend on a constructed base directory.
//
// The checks are the ones ADR 0001 moves off the control plane: whether the
// repository is real, whether its origin is the one the entry declares, and whether
// the checkout directory can be used at all. A provider on another host answers the
// same interface from that host's attestation instead.
type Validator struct{}

// Validate implements workspace.Validator. The returned messages carry no entry
// label: workspace.ValidateEntries prefixes them, so one message reads
// "TEST: <problem>".
func (Validator) Validate(entry workspace.Entry) []string {
	repo := strings.TrimSpace(entry.Repo)
	if repo == "" {
		return []string{"repo.path is not configured, so there is no repository to prepare a task in"}
	}
	info, err := os.Stat(repo)
	if err != nil {
		return []string{fmt.Sprintf("repo.path %s: %v", repo, err)}
	}
	if !info.IsDir() {
		return []string{fmt.Sprintf("repo.path %s is not a directory", repo)}
	}

	var problems []string
	gitInfo, err := os.Stat(filepath.Join(repo, ".git"))
	if err != nil {
		return []string{fmt.Sprintf("%s is not a git work tree (no .git)", repo)}
	}
	// When .git is a directory the shared config is right there. When it is a file
	// (a linked worktree or a submodule) the config lives elsewhere, so the remote
	// check is skipped rather than guessed.
	if entry.Remote != "" && gitInfo.IsDir() {
		actual, ok := originRemote(repo)
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s has no remote.origin.url to compare with repo.remote %q", repo, entry.Remote))
		case !sameRemote(actual, entry.Remote):
			problems = append(problems, fmt.Sprintf("%s origin is %q but the mapping declares %q", repo, actual, entry.Remote))
		}
	}

	if strings.TrimSpace(entry.DefaultBranch) == "" {
		problems = append(problems, "repo.default_branch is empty, so no task checkout can be based on it")
	}

	base := strings.TrimSpace(entry.Base)
	switch {
	case base == "":
		problems = append(problems, "no directory is configured for task checkouts: set the entry's worktrees directory or FLOWHUB_WORKTREE_BASE")
	case samePath(base, repo):
		problems = append(problems, "worktrees must differ from repo.path")
	case isBeneath(base, repo):
		problems = append(problems, fmt.Sprintf("worktrees %s is inside the repository; keep task checkouts outside the tree the agent reads", base))
	default:
		if info, err := os.Stat(base); err == nil && !info.IsDir() {
			problems = append(problems, fmt.Sprintf("worktrees %s exists but is not a directory", base))
		}
		// A missing base directory is fine: the provider creates it.
	}
	return problems
}

// originRemote reads remote.origin.url straight from .git/config, so validation
// does not depend on the git binary being installed.
func originRemote(repoPath string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(repoPath, ".git", "config"))
	if err != nil {
		return "", false
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Trim(line, "[] \t"))
			continue
		}
		if section != `remote "origin"` {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if strings.TrimSpace(strings.ToLower(key)) == "url" {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// sameRemote compares two remotes while ignoring a trailing slash or ".git", so
// the ssh and https spellings of the same repository can be written either way.
func sameRemote(a, b string) bool {
	return normalizeRemote(a) == normalizeRemote(b)
}

func normalizeRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	remote = strings.TrimSuffix(remote, "/")
	remote = strings.TrimSuffix(remote, ".git")
	return remote
}

// canonical resolves symlinks so that the paths compared here are the ones the
// runtime will see: it keys sessions and projects by the literal directory
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
func (p *Provider) ensureScratch(path string) error {
	scratch := filepath.Join(path, ScratchDir)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return fmt.Errorf("localworktree: create %s: %w", scratch, err)
	}
	// Ask git where this worktree's exclude file lives: for a linked worktree it
	// is inside the repository's .git/worktrees/<name>/, not .git/info/exclude.
	exclude, err := p.run(context.Background(), path, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(path, exclude)
	}
	existing, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("localworktree: read %s: %w", exclude, err)
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
		return fmt.Errorf("localworktree: write %s: %w", exclude, err)
	}
	return nil
}

// clearForCreate makes the target path available for a fresh checkout.
//
// A directory holding nothing, or holding only FlowHub's own scratch, is removed:
// neither can be anybody's work. Anything else is moved aside rather than deleted,
// because a checkout that git no longer recognises may still hold uncommitted work
// and this code cannot tell. The moved path is returned so the caller can report it.
func (p *Provider) clearForCreate(path string) (string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("localworktree: inspect %s: %w", path, err)
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
			return "", fmt.Errorf("localworktree: clear %s: %w", path, err)
		}
		return "", nil
	}
	moved := fmt.Sprintf("%s.stale-%d", path, time.Now().Unix())
	if err := os.Rename(path, moved); err != nil {
		return "", fmt.Errorf("localworktree: %s is in the way and could not be moved aside: %w", path, err)
	}
	return moved, nil
}

// usable reports whether the path is a working tree git can actually use. It is
// the check that turns a stale registration into a recreate instead of a failure.
func (p *Provider) usable(ctx context.Context, path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	out, err := p.run(ctx, path, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		// The directory is there but git cannot use it: treat it as unusable rather
		// than failing the task forever.
		return false, nil
	}
	return strings.TrimSpace(out) == "true", nil
}

func (p *Provider) registered(ctx context.Context, repo, path string) (bool, error) {
	paths, err := p.List(ctx, repo)
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

func (p *Provider) branchExists(ctx context.Context, repo, branch string) (bool, error) {
	_, err := p.run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
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
