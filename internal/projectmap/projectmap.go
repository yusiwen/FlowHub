// Package projectmap owns the FlowHub configuration file: which event sources
// exist, what starting work means for each of them, which agent runtimes are
// declared, and which repository a `(source, project)` pair maps to.
//
// The mapping is declared in a JSON file (FLOWHUB_CONFIG_FILE, previously
// FLOWHUB_PROJECTS_FILE) rather than discovered from the tracker. That is a
// measured constraint, not a preference: the webhook payload carries only
// {key, name, shortName}, and YouTrack exposes no documented REST field or endpoint
// for the VCS-integration repository URL. Routing is therefore an explicit,
// reviewable configuration, and a project that is not mapped is refused rather than
// guessed — an agent that edits the wrong repository is the worst failure mode this
// layer can have.
//
// Secrets are not here. Locks and key material stay in the environment, renamed per
// source (FLOWHUB_YOUTRACK_HOOK_KEY and friends), so the file stays safe to copy,
// diff, back up and review — and so a lock's enablement cannot drift away from its
// secret.
package projectmap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/rules"
)

// ErrNotFound reports that no configuration file exists at the configured path. The
// caller decides whether that is fatal: phase 1 only records events, so a missing
// file is a warning there, while a file that exists but does not parse is always
// fatal (a broken routing table must never degrade silently into "nothing is
// mapped").
var ErrNotFound = errors.New("configuration file not found")

// DefaultSourceName is the source a version 1 file is translated into.
//
// It is a fact about the format rather than a preference of this package: version 1
// predates the source seam, and its only possible deliveries were YouTrack's. The
// source registry still validates the name when the file is loaded, so a binary
// built without the YouTrack adapter refuses a version 1 file instead of silently
// having no source.
const DefaultSourceName = "youtrack"

// Repo describes the checkout a project maps to.
type Repo struct {
	// Path is the absolute path of the primary clone. It is canonicalised on
	// load (symlinks resolved), because the runtime treats /tmp and /private/tmp as
	// different projects.
	Path string `json:"path"`
	// Remote is the expected origin URL, compared at startup so a mapping that
	// points at the wrong clone is caught before any event is routed.
	Remote string `json:"remote,omitempty"`
	// DefaultBranch is the branch a task is based on.
	DefaultBranch string `json:"default_branch,omitempty"`
}

// Entry is one `(source, project)` to repository mapping.
type Entry struct {
	// Source names the adapter that produces this project's events, e.g. "youtrack".
	Source string
	// Project is the tracker's own project key, matched against the event subject's
	// project, case-insensitively. Together with Source it is the routing key: the
	// same project key in two sources is two different projects, which is what lets a
	// YouTrack "TEST" and a Gitea "TEST" coexist.
	Project string
	// AlsoKeys lets several tracker projects share one repository.
	AlsoKeys []string
	Repo     Repo
	// Worktrees is the directory that holds one checkout per task. It must live
	// outside the repository so the agent cannot walk into other tasks' checkouts,
	// and the workspace provider checks that before anything is created.
	Worktrees string
	// Agent and Model are passed to the runtime when the task session is created.
	// A project may name them; the runtime block is where they normally live.
	Agent string
	Model string
	// Runtime names one agent host that may serve this project, and Runtimes names
	// several: their union is the eligibility set, not an ordered preference list.
	// Naming none means "any configured runtime may serve this project".
	Runtime  string
	Runtimes []string
	// RuntimePolicy overrides the table's runtime_policy for this project. It only
	// decides which runtime takes a *new* task; a task that is already bound stays
	// where it is, whatever the policy says.
	RuntimePolicy string
	// Authors restricts which tracker logins may trigger work for this project. The
	// source block may narrow it further; an empty list here means "everyone the
	// source allows".
	Authors []string
	// Enabled defaults to true when absent; a disabled entry is never matched.
	Enabled *bool

	keys     []string
	runtimes []string
	policy   string
}

// Selection policies for a new task when a project is served by more than one
// runtime. The names are part of the operator's surface: they appear in
// `-print-config`, in the log line that records why a runtime was chosen, and in
// the ADR.
const (
	// PolicySpread balances new tasks across the eligible runtimes, so a second
	// machine is used rather than kept as pure failover.
	PolicySpread = "spread"
	// PolicyFirstHealthy takes the first eligible runtime in the declared order that
	// answers, which is what an operator wants when one host should take everything.
	PolicyFirstHealthy = "first-healthy"
)

// runtimeNamePattern mirrors the inventory's rule for a runtime name. A name that
// could never be enrolled is a typo, and a typo has to fail when the table is
// loaded instead of quietly making a project unroutable.
var runtimeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// IsEnabled reports whether the entry participates in routing.
func (e *Entry) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

// Keys returns every key this entry answers to.
func (e *Entry) Keys() []string { return e.keys }

// RuntimeSet is the runtime names this project may be served by, in declaration
// order and without duplicates. An empty set means "any configured runtime".
func (e *Entry) RuntimeSet() []string { return e.runtimes }

// Policy is the selection policy in force for this project. Load always fills it,
// so it is either PolicySpread or PolicyFirstHealthy.
func (e *Entry) Policy() string { return e.policy }

// Index is the entry's key in the routing index: the source and the project,
// normalised. It is also what the report and the log use to name an entry when two
// sources could claim the same project key.
func (e *Entry) Index() string { return indexKey(e.Source, e.Project) }

// Label names the entry in an operator-facing message.
func (e *Entry) Label() string {
	if strings.TrimSpace(e.Source) == "" {
		return e.Project
	}
	return e.Source + ":" + e.Project
}

// Match describes a successful routing decision.
type Match struct {
	Entry *Entry
	// Key is the project key that matched, recorded verbatim for audit.
	Key string
	// Source is the adapter the match came from, so the audit says which index
	// answered.
	Source string
}

// PolicyBlock is one source's trigger policy as the file may write it.
type PolicyBlock = v2SourcePolicy

// RuntimeBlock is one runtime declaration.
type RuntimeBlock = v2Runtime

// Map is the loaded configuration file.
type Map struct {
	path string
	// format is the version the file was written in, and translated reports that it
	// was a version 1 file without a `version` key.
	format     int
	translated bool

	sources map[string]v2Source
	// sourceOrder keeps the declaration order for the report.
	sourceOrder []string
	runtimes    map[string]v2Runtime
	// runtimeOrder keeps the declaration order for the report.
	runtimeOrder []string

	entries []*Entry
	// byIndex is keyed by `source\x00PROJECT`: the same project key in two sources is
	// two entries, and a duplicate inside one source is still an error.
	byIndex map[string]*Entry
	// policy is the table-wide default selection policy.
	policy string
}

// PolicyDefaults is the outermost level for a source's trigger policy: today the
// environment (FLOWHUB_TRIGGER and friends). It is passed in rather than read here,
// so this package owns the file and not the environment.
//
// The zero value is valid: every field then falls back to rules.Policy.Defaults,
// which is what an operator who configured nothing should get.
type PolicyDefaults struct {
	Trigger             string
	StartStates         []string
	SkipAnalyzeOnCreate bool
	MaxTurns            int
	SelfMarkers         []string
}

// ResolvedPolicy is a source's effective trigger policy together with the level each
// value came from, which is what `-print-config` reports.
type ResolvedPolicy struct {
	Policy rules.Policy
	// Levels names, per field, the level that supplied the value: an environment
	// variable name, or "sources.<name>.policy".
	Levels map[string]string
}

// DefaultSourcePolicy is the policy a source with no block and no environment
// configuration gets.
func DefaultSourcePolicy() rules.Policy { return rules.Policy{}.Defaults() }

// Policy resolves one source's trigger policy: the environment is the outermost
// default, the `sources.<name>.policy` block overrides what it names, and the
// adapter's own defaults fill whatever neither level mentions.
//
// A source the file never declares still resolves: a delivery can only arrive from
// a source this binary registered, and requiring a declaration would make the file
// mandatory for a receiver that only wants to record.
func (m *Map) Policy(sourceName string, defaults PolicyDefaults) ResolvedPolicy {
	resolved := rules.Policy{
		Trigger:             strings.TrimSpace(defaults.Trigger),
		StartStates:         append([]string(nil), defaults.StartStates...),
		SkipAnalyzeOnCreate: defaults.SkipAnalyzeOnCreate,
		MaxTurns:            defaults.MaxTurns,
		SelfMarkers:         append([]string(nil), defaults.SelfMarkers...),
	}
	levels := map[string]string{
		"trigger":                "FLOWHUB_TRIGGER",
		"start_states":           "FLOWHUB_START_STATES",
		"skip_analyze_on_create": "FLOWHUB_SKIP_ANALYZE_ON_CREATE",
		"max_turns":              "FLOWHUB_MAX_TURNS",
		"self_markers":           "rules defaults",
	}

	block, ok := m.sourceBlock(sourceName)
	if ok {
		level := "sources." + sourceName + ".policy"
		if block.Policy.Trigger != nil {
			resolved.Trigger, levels["trigger"] = *block.Policy.Trigger, level
		}
		if block.Policy.StartStates != nil {
			resolved.StartStates, levels["start_states"] = append([]string(nil), (*block.Policy.StartStates)...), level
		}
		if block.Policy.SkipAnalyzeOnCreate != nil {
			resolved.SkipAnalyzeOnCreate, levels["skip_analyze_on_create"] = *block.Policy.SkipAnalyzeOnCreate, level
		}
		if block.Policy.MaxTurns != nil {
			resolved.MaxTurns, levels["max_turns"] = *block.Policy.MaxTurns, level
		}
		if block.Policy.SelfMarkers != nil {
			resolved.SelfMarkers, levels["self_markers"] = append([]string(nil), (*block.Policy.SelfMarkers)...), level
		}
	}

	// The adapter fills what neither level named and never leaves a value it cannot
	// work with; it is idempotent for the values that are already set.
	resolved = resolved.Defaults()
	return ResolvedPolicy{Policy: resolved, Levels: levels}
}

// sourceBlock finds a source declaration by name, case-insensitively.
func (m *Map) sourceBlock(name string) (v2Source, bool) {
	if m == nil {
		return v2Source{}, false
	}
	wanted := strings.ToLower(strings.TrimSpace(name))
	if block, ok := m.sources[wanted]; ok {
		return block, true
	}
	return v2Source{}, false
}

// SourceEnabled reports whether the file declares a source as enabled. A source the
// file does not mention is enabled: the receiver has to accept deliveries from the
// adapter it was built with (`FLOWHUB_HOOK_KEY` is the lock, not a file entry).
func (m *Map) SourceEnabled(name string) bool {
	block, ok := m.sourceBlock(name)
	if !ok || block.Enabled == nil {
		return true
	}
	return *block.Enabled
}

// SourceAuthors is a source-level author allowlist, empty when the file names none.
func (m *Map) SourceAuthors(name string) []string {
	block, ok := m.sourceBlock(name)
	if !ok {
		return nil
	}
	return block.Authors
}

// Sources lists the declared source names in declaration order.
func (m *Map) Sources() []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.sourceOrder...)
}

// Runtimes lists the declared runtime names in declaration order.
func (m *Map) Runtimes() []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.runtimeOrder...)
}

// RuntimeBlock returns a declared runtime block.
func (m *Map) RuntimeBlock(name string) (RuntimeBlock, bool) {
	if m == nil {
		return RuntimeBlock{}, false
	}
	block, ok := m.runtimes[strings.TrimSpace(name)]
	return block, ok
}

// Format is the version the file was written in, and Translated reports that it was
// a version 1 file with no `version` key, which the loader converted on read.
func (m *Map) Format() int {
	if m == nil || m.format == 0 {
		return FormatV2
	}
	return m.format
}

// Translated reports whether the file was a version 1 file that had to be
// converted.
func (m *Map) Translated() bool { return m != nil && m.translated }

// FormatNote renders the format for an operator-facing report.
func (m *Map) FormatNote() string {
	switch {
	case m == nil:
		return "none"
	case m.Translated():
		return fmt.Sprintf("%d (translated; migrate the file to version %d)", m.Format(), FormatV2)
	default:
		return strconv.Itoa(m.Format())
	}
}

// SelectionPolicy is the table-wide default for how a new task picks among the
// runtimes its project names.
func (m *Map) SelectionPolicy() string {
	if m == nil || m.policy == "" {
		return PolicySpread
	}
	return m.policy
}

// normalisePolicy validates a policy value and returns it lowercased.
func normalisePolicy(raw string) (string, error) {
	switch value := strings.ToLower(strings.TrimSpace(raw)); value {
	case "":
		return PolicySpread, nil
	case PolicySpread, PolicyFirstHealthy:
		return value, nil
	default:
		return "", fmt.Errorf("runtime_policy %q: want %s or %s", raw, PolicySpread, PolicyFirstHealthy)
	}
}

// Load reads, translates and validates the configuration file.
//
// It returns ErrNotFound (wrapped) when the file does not exist, and any other error
// means the file exists but must not be trusted.
func Load(path string) (*Map, error) {
	m := &Map{path: path, byIndex: map[string]*Entry{}, format: FormatV2}
	if strings.TrimSpace(path) == "" {
		return m, fmt.Errorf("%w: no path configured", ErrNotFound)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return m, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("read configuration file %s: %w", path, err)
	}
	cleaned, err := stripDocumentationKeys(raw)
	if err != nil {
		return nil, fmt.Errorf("parse configuration file %s: %w", path, err)
	}
	file, translated, err := decodeFile(path, cleaned)
	if err != nil {
		return nil, err
	}
	return m.load(file, path, translated)
}

// load builds the map from a decoded file. It is separate from Load so the version
// translation, the index and the validation can be tested without a file on disk.
func (m *Map) load(file *v2File, path string, translated bool) (*Map, error) {
	if len(file.Projects) == 0 {
		return nil, fmt.Errorf("configuration file %s declares no projects", path)
	}
	m.translated = translated
	if translated {
		m.format = FormatV1
	} else {
		m.format = FormatV2
	}

	m.sources = file.Sources
	m.sourceOrder = sortedSourceNames(file.Sources)
	m.runtimes = file.Runtimes
	m.runtimeOrder = sortedRuntimeNames(file.Runtimes)

	policy, err := normalisePolicy(file.RuntimePolicy)
	if err != nil {
		return nil, fmt.Errorf("configuration file %s: %w", path, err)
	}
	m.policy = policy

	for index := range file.Projects {
		project := file.Projects[index]
		if err := checkV2(project, index); err != nil {
			return nil, fmt.Errorf("configuration file %s: %w", path, err)
		}
		entry := &Entry{
			Source:        strings.ToLower(strings.TrimSpace(project.Source)),
			Project:       strings.TrimSpace(project.Project),
			AlsoKeys:      project.Also,
			Repo:          project.Repo,
			Worktrees:     project.Worktrees,
			Agent:         strings.TrimSpace(project.Agent),
			Model:         strings.TrimSpace(project.Model),
			Runtime:       strings.TrimSpace(project.Runtime),
			Runtimes:      project.Runtimes,
			RuntimePolicy: project.RuntimePolicy,
			Authors:       project.Authors,
			Enabled:       project.Enabled,
		}
		if err := m.register(entry, index); err != nil {
			return nil, fmt.Errorf("configuration file %s: %w", path, err)
		}
	}
	if err := m.validateRuntimeReferences(); err != nil {
		return nil, fmt.Errorf("configuration file %s: %w", path, err)
	}
	return m, nil
}

// validateRuntimeReferences refuses a project that names a runtime the file
// declares with a malformed block. A name the file does not declare at all stays
// legal: it may be an enrolled host whose policy lives only in the inventory.
func (m *Map) validateRuntimeReferences() error {
	for _, entry := range m.entries {
		for _, name := range entry.runtimes {
			block, declared := m.runtimes[name]
			if !declared {
				continue
			}
			if err := validateRuntimeBlock(name, block); err != nil {
				return fmt.Errorf("project %s: %w", entry.Label(), err)
			}
		}
	}
	return nil
}

// validateRuntimeBlock checks one declaration. It is exported through
// RuntimeProblems so the start-up gate can refuse a bad block even for a runtime no
// project names yet.
func validateRuntimeBlock(name string, block v2Runtime) error {
	if url := strings.TrimSpace(block.URL); url != "" {
		if _, err := parseRuntimeURL(url); err != nil {
			return fmt.Errorf("runtimes.%s.url: %w", name, err)
		}
	}
	if block.Auth != nil {
		if strings.TrimSpace(block.Auth.User) == "" {
			return fmt.Errorf("runtimes.%s.auth: a user is required (it decides the Basic Auth header)", name)
		}
		if strings.TrimSpace(block.Auth.PasswordEnv) == "" {
			return fmt.Errorf("runtimes.%s.auth: password_env is required: the secret stays in the environment and the file only names the variable", name)
		}
	}
	if block.MaxConcurrent != nil && *block.MaxConcurrent != 1 {
		return fmt.Errorf("runtimes.%s.max_concurrent: %d is not supported; a session belongs to one runtime and a prompt sent to a busy session is swallowed, so raising this needs per-task locking that does not exist yet",
			name, *block.MaxConcurrent)
	}
	if deadline := strings.TrimSpace(block.Deadline); deadline != "" {
		if _, err := time.ParseDuration(deadline); err != nil {
			return fmt.Errorf("runtimes.%s.deadline %q: %w", name, deadline, err)
		}
	}
	return nil
}

// RuntimeProblems checks every declared runtime block, including the ones no project
// names: a block that cannot be honoured is a configuration mistake whether or not
// something currently routes to it.
func (m *Map) RuntimeProblems() []string {
	if m == nil {
		return nil
	}
	var problems []string
	for _, name := range m.runtimeOrder {
		if err := validateRuntimeBlock(name, m.runtimes[name]); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}

// runtimeURL is a parsed runtime address.
type runtimeURL struct {
	Scheme string
	Host   string
	Path   string
}

// parseRuntimeURL checks a runtime address without importing net/url into the
// report path: a runtime URL is http(s) and has a host, and anything else is a typo
// that must fail at load time.
func parseRuntimeURL(raw string) (runtimeURL, error) {
	scheme, rest, found := strings.Cut(raw, "://")
	if !found {
		return runtimeURL{}, fmt.Errorf("%q wants an http or https URL", raw)
	}
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return runtimeURL{}, fmt.Errorf("%q wants an http or https URL", raw)
	}
	host, path, _ := strings.Cut(rest, "/")
	if strings.TrimSpace(host) == "" {
		return runtimeURL{}, fmt.Errorf("%q has no host", raw)
	}
	return runtimeURL{Scheme: scheme, Host: host, Path: path}, nil
}

func (m *Map) register(entry *Entry, index int) error {
	if entry.Source == "" {
		return fmt.Errorf("project #%d: source is required (name the adapter that produces this project's events)", index+1)
	}
	if entry.Project == "" {
		return fmt.Errorf("project #%d (%s): project is required", index+1, entry.Source)
	}
	entry.Repo.Path = strings.TrimSpace(entry.Repo.Path)
	if entry.Repo.Path == "" {
		return fmt.Errorf("project %s: repo.path is required", entry.Label())
	}
	if !filepath.IsAbs(entry.Repo.Path) {
		return fmt.Errorf("project %s: repo.path %q must be absolute (a relative path would depend on the caller's working directory)", entry.Label(), entry.Repo.Path)
	}
	if entry.Worktrees != "" {
		entry.Worktrees = strings.TrimSpace(entry.Worktrees)
		if !filepath.IsAbs(entry.Worktrees) {
			return fmt.Errorf("project %s: worktrees %q must be absolute", entry.Label(), entry.Worktrees)
		}
	}
	entry.Repo.Path = canonicalize(entry.Repo.Path)
	if entry.Worktrees != "" {
		entry.Worktrees = canonicalize(entry.Worktrees)
	}

	seenRuntimes := map[string]bool{}
	for _, name := range append([]string{entry.Runtime}, entry.Runtimes...) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !runtimeNamePattern.MatchString(name) {
			return fmt.Errorf("project %s: runtime %q is not a runtime name (want %s)",
				entry.Label(), name, runtimeNamePattern)
		}
		if seenRuntimes[name] {
			continue
		}
		seenRuntimes[name] = true
		entry.runtimes = append(entry.runtimes, name)
	}
	policy, err := normalisePolicy(entry.RuntimePolicy)
	if err != nil {
		return fmt.Errorf("project %s: %w", entry.Label(), err)
	}
	// The per-project value wins; the table default is what an entry that says
	// nothing gets. Resolving here means every reader sees one answer.
	if strings.TrimSpace(entry.RuntimePolicy) == "" {
		policy = m.SelectionPolicy()
	}
	entry.policy = policy

	keys := append([]string{entry.Project}, entry.AlsoKeys...)
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
		slot := indexKey(entry.Source, key)
		if existing, taken := m.byIndex[slot]; taken {
			return fmt.Errorf("%s:%s is claimed by both %s and %s (the same project key in two sources is two entries; the same key twice in one source is not)",
				entry.Source, key, existing.Label(), entry.Label())
		}
		m.byIndex[slot] = entry
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

// Keys returns every key of the ENABLED entries, sorted and qualified by source, so
// two sources answering to the same project key are both visible.
//
// Disabled entries are excluded: this feeds the startup banner and /healthz, where
// listing a key that can never match a delivery would be misleading.
func (m *Map) Keys() []string {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m.byIndex))
	for _, entry := range m.entries {
		if !entry.IsEnabled() {
			continue
		}
		for _, key := range entry.keys {
			keys = append(keys, entry.Source+":"+key)
		}
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

// Resolve looks up one `(source, project)` pair.
func (m *Map) Resolve(source, projectKey string) (*Entry, bool) {
	if m == nil {
		return nil, false
	}
	entry, ok := m.byIndex[indexKey(strings.ToLower(strings.TrimSpace(source)), projectKey)]
	if !ok || !entry.IsEnabled() {
		return nil, false
	}
	return entry, true
}

// Match resolves a delivery against the index for one source.
//
// The subject's project key is the only key: splitting an issue ID on its last `-`
// is a YouTrack convention, and the adapter that knows the convention fills
// Subject.Project itself. The router never guesses — a delivery with no project, or
// with one this file does not map, is a miss.
func (m *Map) Match(source, projectKey string) (Match, bool) {
	key := strings.TrimSpace(projectKey)
	if key == "" {
		return Match{}, false
	}
	entry, ok := m.Resolve(source, key)
	if !ok {
		return Match{}, false
	}
	return Match{Entry: entry, Key: key, Source: strings.ToLower(strings.TrimSpace(source))}, true
}

// indexKey is the routing index slot for one `(source, project)` pair. The separator
// cannot appear in a source name or a project key, so no pair can collide with
// another.
func indexKey(source, project string) string {
	return strings.ToLower(strings.TrimSpace(source)) + "\x00" + normalizeKey(project)
}

// normalizeKey makes matching case-insensitive: trackers render project keys in
// upper case in issue IDs, and a case typo in the configuration should not
// silently disable routing.
func normalizeKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// canonicalize resolves symlinks so the path handed to the runtime is the one the
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

func sortedSourceNames(sources map[string]v2Source) []string { return sortedKeys(sources) }

func sortedRuntimeNames(runtimes map[string]v2Runtime) []string { return sortedKeys(runtimes) }
