package projectmap

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// agentNamePattern keeps the agent name safe to hand to opencode as a JSON field
// and to read back in logs.
var agentNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Validate checks the filesystem side of the mapping. An empty result means the
// table is safe to route with.
//
// Every problem blocks startup: a mapping that claims to route but points at the
// wrong clone is exactly how an agent ends up editing the wrong repository.
// Disabled entries are skipped, because they may deliberately point at a checkout
// that is not present on this host.
func (m *Map) Validate() []string {
	if m == nil {
		return nil
	}
	var problems []string
	for _, entry := range m.entries {
		if !entry.IsEnabled() {
			continue
		}
		problems = append(problems, entry.validate()...)
	}
	return problems
}

func (e *Entry) validate() []string {
	label := e.YouTrackKey

	info, err := os.Stat(e.Repo.Path)
	if err != nil {
		return []string{fmt.Sprintf("%s: repo.path %s: %v", label, e.Repo.Path, err)}
	}
	if !info.IsDir() {
		return []string{fmt.Sprintf("%s: repo.path %s is not a directory", label, e.Repo.Path)}
	}

	var problems []string
	gitInfo, err := os.Stat(filepath.Join(e.Repo.Path, ".git"))
	if err != nil {
		return []string{fmt.Sprintf("%s: %s is not a git work tree (no .git)", label, e.Repo.Path)}
	}
	// When .git is a directory the shared config is right there. When it is a file
	// (a linked worktree or a submodule) the config lives elsewhere, so the remote
	// check is skipped rather than guessed.
	if e.Repo.Remote != "" && gitInfo.IsDir() {
		actual, ok := originRemote(e.Repo.Path)
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: %s has no remote.origin.url to compare with repo.remote %q", label, e.Repo.Path, e.Repo.Remote))
		case !sameRemote(actual, e.Repo.Remote):
			problems = append(problems, fmt.Sprintf("%s: %s origin is %q but the mapping declares %q", label, e.Repo.Path, actual, e.Repo.Remote))
		}
	}

	if e.Worktrees != "" {
		switch {
		case samePath(e.Worktrees, e.Repo.Path):
			problems = append(problems, fmt.Sprintf("%s: worktrees must differ from repo.path", label))
		case isBeneath(e.Worktrees, e.Repo.Path):
			problems = append(problems, fmt.Sprintf("%s: worktrees %s is inside the repository; keep task checkouts outside the tree the agent reads", label, e.Worktrees))
		}
		if info, err := os.Stat(e.Worktrees); err == nil && !info.IsDir() {
			problems = append(problems, fmt.Sprintf("%s: worktrees %s exists but is not a directory", label, e.Worktrees))
		}
		// A missing worktrees directory is fine: the dispatcher creates it.
	}

	if e.Agent != "" && !agentNamePattern.MatchString(e.Agent) {
		problems = append(problems, fmt.Sprintf("%s: agent %q is not a valid agent name", label, e.Agent))
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

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

// isBeneath reports whether path lies inside parent.
func isBeneath(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	if err != nil || relative == "." {
		return false
	}
	return !strings.HasPrefix(relative, "..")
}

// Report renders the routing table for `flowhub -print-config`.
func (m *Map) Report() string {
	var b strings.Builder
	source := "<unset>"
	if m != nil && m.Path() != "" {
		source = m.Path()
	}
	fmt.Fprintf(&b, "projects_file:      %s\n", source)

	if m == nil || m.Len() == 0 {
		fmt.Fprintf(&b, "projects:           <none> — deliveries are recorded but never routed\n")
		return b.String()
	}

	fmt.Fprintf(&b, "projects:           %d mapping(s)\n", m.Len())
	for _, entry := range m.entries {
		state := ""
		if !entry.IsEnabled() {
			state = " [disabled]"
		}
		fmt.Fprintf(&b, "  %-14s -> %s%s\n", strings.Join(entry.Keys(), ","), entry.Repo.Path, state)
		if entry.Repo.Remote != "" {
			fmt.Fprintf(&b, "  %-14s    remote %s\n", "", entry.Repo.Remote)
		}
		if entry.Repo.DefaultBranch != "" {
			fmt.Fprintf(&b, "  %-14s    branch %s\n", "", entry.Repo.DefaultBranch)
		}
		if entry.Worktrees != "" {
			fmt.Fprintf(&b, "  %-14s    worktrees %s\n", "", entry.Worktrees)
		}
		var details []string
		if entry.Agent != "" {
			details = append(details, "agent="+entry.Agent)
		}
		if entry.Model != "" {
			details = append(details, "model="+entry.Model)
		}
		if len(entry.Authors) > 0 {
			details = append(details, "authors="+strings.Join(entry.Authors, ","))
		}
		if len(details) > 0 {
			fmt.Fprintf(&b, "  %-14s    %s\n", "", strings.Join(details, " "))
		}
	}
	return b.String()
}
