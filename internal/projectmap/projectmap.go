// Package projectmap resolves a YouTrack project to the local repository that
// opencode should operate on.
//
// The mapping is declared in a JSON file (FLOWHUB_PROJECTS_FILE) rather than
// discovered from YouTrack. That is a measured constraint, not a preference:
// the webhook payload carries only {key, name, shortName}, and YouTrack exposes
// no documented REST field or endpoint for the VCS-integration repository URL.
// Routing is therefore an explicit, reviewable configuration, and a project that
// is not mapped is refused rather than guessed — an agent that edits the wrong
// repository is the worst failure mode this layer can have.
package projectmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNotFound reports that no projects file exists at the configured path. The
// caller decides whether that is fatal: phase 1 only records events, so a missing
// file is a warning there, while a file that exists but does not parse is always
// fatal (a broken routing table must never degrade silently into "nothing is
// mapped").
var ErrNotFound = errors.New("projects file not found")

// How a delivery was matched to a mapping entry, for the audit trail.
const (
	ViaProjectKey    = "project.key"
	ViaIssueIDPrefix = "issue_id_prefix"
)

// Repo describes the local checkout a project maps to.
type Repo struct {
	// Path is the absolute path of the primary clone. It is canonicalised on
	// load (symlinks resolved), because opencode treats /tmp and /private/tmp as
	// different projects.
	Path string `json:"path"`
	// Remote is the expected origin URL, compared at startup so a mapping that
	// points at the wrong clone is caught before any event is routed.
	Remote string `json:"remote,omitempty"`
	// DefaultBranch is the branch a task worktree is based on.
	DefaultBranch string `json:"default_branch,omitempty"`
}

// Entry is one YouTrack project to repository mapping.
type Entry struct {
	// YouTrackKey is the primary mapping key, matched against the payload's
	// project.key (case-insensitively).
	YouTrackKey string `json:"youtrack_key"`
	// AlsoKeys lets several YouTrack projects share one repository.
	AlsoKeys []string `json:"also_keys,omitempty"`
	Repo     Repo     `json:"repo"`
	// Worktrees is the directory that holds one git worktree per task. It must
	// live outside the repository so the agent cannot walk into other tasks'
	// checkouts, and it is required once dispatch is enabled.
	Worktrees string `json:"worktrees,omitempty"`
	// Agent and Model are passed to opencode when the task session is created.
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	// Authors restricts which YouTrack logins may trigger work for this project.
	// It is empty by default: the global allowlist still applies.
	Authors []string `json:"authors,omitempty"`
	// Enabled defaults to true when absent; a disabled entry is never matched.
	Enabled *bool `json:"enabled,omitempty"`

	keys []string
}

// IsEnabled reports whether the entry participates in routing.
func (e *Entry) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

// Keys returns every key this entry answers to.
func (e *Entry) Keys() []string { return e.keys }

// Match describes a successful routing decision.
type Match struct {
	Entry *Entry
	// Key is the value that matched (the payload's project key or the issue-ID
	// prefix), recorded verbatim for audit.
	Key string
	// Via is ViaProjectKey or ViaIssueIDPrefix.
	Via string
}

// Map is the loaded routing table.
type Map struct {
	path    string
	entries []*Entry
	byKey   map[string]*Entry
}

type fileFormat struct {
	Projects []Entry `json:"projects"`
}

// Load reads and validates the structure of the projects file.
//
// It returns ErrNotFound (wrapped) when the file does not exist, and any other
// error means the file exists but must not be trusted.
func Load(path string) (*Map, error) {
	m := &Map{path: path, byKey: map[string]*Entry{}}
	if strings.TrimSpace(path) == "" {
		return m, fmt.Errorf("%w: no path configured", ErrNotFound)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return m, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("read projects file %s: %w", path, err)
	}

	cleaned, err := stripDocumentationKeys(raw)
	if err != nil {
		return nil, fmt.Errorf("parse projects file %s: %w", path, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(cleaned))
	decoder.DisallowUnknownFields()
	var file fileFormat
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse projects file %s: %w", path, err)
	}
	if len(file.Projects) == 0 {
		return nil, fmt.Errorf("projects file %s declares no projects", path)
	}

	for index := range file.Projects {
		entry := &file.Projects[index]
		if err := m.register(entry, index); err != nil {
			return nil, fmt.Errorf("projects file %s: %w", path, err)
		}
	}
	return m, nil
}

func (m *Map) register(entry *Entry, index int) error {
	entry.YouTrackKey = strings.TrimSpace(entry.YouTrackKey)
	if entry.YouTrackKey == "" {
		return fmt.Errorf("project #%d: youtrack_key is required", index+1)
	}
	entry.Repo.Path = strings.TrimSpace(entry.Repo.Path)
	if entry.Repo.Path == "" {
		return fmt.Errorf("project %s: repo.path is required", entry.YouTrackKey)
	}
	if !filepath.IsAbs(entry.Repo.Path) {
		return fmt.Errorf("project %s: repo.path %q must be absolute (a relative path would depend on the caller's working directory)", entry.YouTrackKey, entry.Repo.Path)
	}
	if entry.Worktrees != "" {
		entry.Worktrees = strings.TrimSpace(entry.Worktrees)
		if !filepath.IsAbs(entry.Worktrees) {
			return fmt.Errorf("project %s: worktrees %q must be absolute", entry.YouTrackKey, entry.Worktrees)
		}
	}
	entry.Repo.Path = canonicalize(entry.Repo.Path)
	if entry.Worktrees != "" {
		entry.Worktrees = canonicalize(entry.Worktrees)
	}

	keys := append([]string{entry.YouTrackKey}, entry.AlsoKeys...)
	seen := map[string]bool{}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		normalized := normalizeKey(key)
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		if existing, taken := m.byKey[normalized]; taken {
			return fmt.Errorf("key %q is claimed by both %s and %s", key, existing.YouTrackKey, entry.YouTrackKey)
		}
		m.byKey[normalized] = entry
		entry.keys = append(entry.keys, key)
	}
	m.entries = append(m.entries, entry)
	return nil
}

// Path returns the file the map was loaded from ("" when nothing was loaded).
func (m *Map) Path() string {
	if m == nil {
		return ""
	}
	return m.path
}

// Entries returns the configured entries in declaration order.
func (m *Map) Entries() []*Entry {
	if m == nil {
		return nil
	}
	return m.entries
}

// Len returns the number of configured entries.
func (m *Map) Len() int {
	if m == nil {
		return 0
	}
	return len(m.entries)
}

// Keys returns every key of the ENABLED entries, sorted, including alias keys.
//
// Disabled entries are excluded: this feeds the startup banner and /healthz, where
// listing a key that can never match a delivery would be misleading.
func (m *Map) Keys() []string {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m.byKey))
	for _, entry := range m.entries {
		if !entry.IsEnabled() {
			continue
		}
		keys = append(keys, entry.keys...)
	}
	sort.Strings(keys)
	return keys
}

// Routable returns the number of entries that can actually match a delivery.
func (m *Map) Routable() int {
	if m == nil {
		return 0
	}
	count := 0
	for _, entry := range m.entries {
		if entry.IsEnabled() {
			count++
		}
	}
	return count
}

// Resolve looks up one project key.
func (m *Map) Resolve(projectKey string) (*Entry, bool) {
	if m == nil {
		return nil, false
	}
	entry, ok := m.byKey[normalizeKey(projectKey)]
	if !ok || !entry.IsEnabled() {
		return nil, false
	}
	return entry, true
}

// Match resolves a delivery. The payload's project key is authoritative; the
// issue-ID prefix is used only when the payload carries no project object at all
// (every measured delivery carried it, so the fallback should stay unused).
//
// A non-empty project key that is not mapped is a hard miss rather than a reason
// to try the prefix: a payload whose key and prefix disagree is not safe to route
// on a guess.
func (m *Map) Match(projectKey, issueID string) (Match, bool) {
	if key := strings.TrimSpace(projectKey); key != "" {
		entry, ok := m.Resolve(key)
		if !ok {
			return Match{}, false
		}
		return Match{Entry: entry, Key: key, Via: ViaProjectKey}, true
	}
	if prefix := IssueIDPrefix(issueID); prefix != "" {
		if entry, ok := m.Resolve(prefix); ok {
			return Match{Entry: entry, Key: prefix, Via: ViaIssueIDPrefix}, true
		}
	}
	return Match{}, false
}

// IssueIDPrefix returns the project part of a readable issue ID
// ("TEST-11" -> "TEST"). It returns "" when the ID has no such prefix.
func IssueIDPrefix(issueID string) string {
	index := strings.LastIndex(issueID, "-")
	if index <= 0 {
		return ""
	}
	return issueID[:index]
}

// normalizeKey makes matching case-insensitive: YouTrack renders project keys in
// upper case in issue IDs, and a case typo in the configuration should not
// silently disable routing.
func normalizeKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// canonicalize resolves symlinks so the path handed to opencode is the one the
// operating system reports. Paths that do not exist yet (a worktrees directory)
// are rebuilt from their deepest existing ancestor.
func canonicalize(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	dir, rest := path, ""
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
	}
}
