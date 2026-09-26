package projectmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/yusiwen/flowhub/internal/rules"
)

// The configuration file has two versions. Version 2 splits the file into
// `sources`, `runtimes` and `projects`, and names a project's tracker in a field
// instead of in the field's name. Version 1 could only mean YouTrack, so it is
// translated on read (ADR 0001 step 3).
const (
	// FormatV1 is the original routing table: `projects[].youtrack_key`, one global
	// key index, and every source fact in the environment.
	FormatV1 = 1
	// FormatV2 is the current format: `sources`, `runtimes`, and
	// `projects[].source` + `projects[].project` in a `(source, project)` index.
	FormatV2 = 2
)

// fileHead is the lenient peek that decides which version to decode. It is decoded
// without DisallowUnknownFields on purpose: everything except the version is
// checked by the strict decode of the version's own shape, so an unknown field is
// still an error — it just gets the right error.
type fileHead struct {
	Version *int `json:"version"`
}

// v1File is the original shape, still accepted so that an upgrade cannot silently
// stop a live receiver.
type v1File struct {
	Projects      []v1Project `json:"projects"`
	RuntimePolicy string      `json:"runtime_policy,omitempty"`
}

type v1Project struct {
	YouTrackKey   string   `json:"youtrack_key"`
	AlsoKeys      []string `json:"also_keys,omitempty"`
	Repo          Repo     `json:"repo"`
	Worktrees     string   `json:"worktrees,omitempty"`
	Agent         string   `json:"agent,omitempty"`
	Model         string   `json:"model,omitempty"`
	Runtime       string   `json:"runtime,omitempty"`
	Runtimes      []string `json:"runtimes,omitempty"`
	RuntimePolicy string   `json:"runtime_policy,omitempty"`
	Authors       []string `json:"authors,omitempty"`
	Enabled       *bool    `json:"enabled,omitempty"`
}

// v2File is the current shape.
type v2File struct {
	Version       *int                 `json:"version,omitempty"`
	Sources       map[string]v2Source  `json:"sources,omitempty"`
	Runtimes      map[string]v2Runtime `json:"runtimes,omitempty"`
	Projects      []v2Project          `json:"projects"`
	RuntimePolicy string               `json:"runtime_policy,omitempty"`
}

// v2Source is one `sources.<name>` block: whether the source exists at all, what
// starting work means for it, and where its prompt comes from.
//
// Lock material is deliberately absent: which address the gateway forwards from,
// which URL is published and what the shared secret is are deployment facts, and
// they stay in the environment (FLOWHUB_YOUTRACK_HOOK_KEY and friends) so a lock's
// enablement cannot be separated from its secret.
type v2Source struct {
	Enabled    *bool          `json:"enabled,omitempty"`
	Policy     v2SourcePolicy `json:"policy,omitempty"`
	PromptFile string         `json:"prompt_file,omitempty"`
	Authors    []string       `json:"authors,omitempty"`
}

// v2SourcePolicy is a source's trigger policy as written in the file. Every field
// is a pointer so that "absent" is distinguishable from "explicitly false or zero":
// the environment supplies the outermost default, and a deliberate zero must not
// look like silence.
type v2SourcePolicy struct {
	Trigger             *string   `json:"trigger,omitempty"`
	StartStates         *[]string `json:"start_states,omitempty"`
	SkipAnalyzeOnCreate *bool     `json:"skip_analyze_on_create,omitempty"`
	MaxTurns            *int      `json:"max_turns,omitempty"`
	SelfMarkers         *[]string `json:"self_markers,omitempty"`
}

// v2Runtime is one `runtimes.<name>` block: the operator's declaration of a host.
//
// It is policy, not enrolment: the control-plane inventory (ADR 0002) records which
// hosts were prepared and verified, their state and the models they reported, while
// this block says what the dispatcher should *ask* of that host. A name that exists
// only here is a local runtime — usable without enrolment, which is the single-host
// case.
type v2Runtime struct {
	URL string `json:"url,omitempty"`
	// Auth keeps the secret in the environment and only its variable name here, so
	// the file stays safe to copy, diff and review.
	Auth  *v2RuntimeAuth `json:"auth,omitempty"`
	Agent string         `json:"agent,omitempty"`
	Model string         `json:"model,omitempty"`
	// Deadline bounds one turn on this host.
	Deadline string `json:"deadline,omitempty"`
	// MaxConcurrent is accepted as 1 and refused otherwise: a session belongs to one
	// runtime and a prompt sent to a busy session is swallowed, so raising this needs
	// per-task locking that does not exist yet.
	MaxConcurrent *int `json:"max_concurrent,omitempty"`
}

type v2RuntimeAuth struct {
	User string `json:"user"`
	// PasswordEnv names the environment variable holding the password. An empty name
	// with a user is refused: half a credential pair fails every turn, late.
	PasswordEnv string `json:"password_env"`
}

// v2Project is one `projects[]` entry.
//
// `repo.path` and `worktrees` are still per project: they are facts of the host
// that holds the checkout, and until a provider exists that runs on *another* host
// (step 6) moving them under `runtimes.<name>.workspace` would relocate a path
// without removing it. The provider already owns the *checks* (step 2).
type v2Project struct {
	Source        string   `json:"source"`
	Project       string   `json:"project"`
	Also          []string `json:"also,omitempty"`
	Repo          Repo     `json:"repo"`
	Worktrees     string   `json:"worktrees,omitempty"`
	Runtime       string   `json:"runtime,omitempty"`
	Runtimes      []string `json:"runtimes,omitempty"`
	RuntimePolicy string   `json:"runtime_policy,omitempty"`
	Agent         string   `json:"agent,omitempty"`
	Model         string   `json:"model,omitempty"`
	Authors       []string `json:"authors,omitempty"`
	Enabled       *bool    `json:"enabled,omitempty"`

	// The version 1 spellings are declared only so that a version 2 file which uses
	// them is refused with the replacement named, instead of with "unknown field".
	YouTrackKey string   `json:"youtrack_key,omitempty"`
	AlsoKeys    []string `json:"also_keys,omitempty"`
}

// decodeFile reads the file, decides which version it is, and decodes it strictly
// into that version's shape.
func decodeFile(path string, cleaned []byte) (file *v2File, translated bool, err error) {
	var head fileHead
	if err := json.Unmarshal(cleaned, &head); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	version := FormatV1
	if head.Version != nil {
		version = *head.Version
	}

	switch version {
	case FormatV1:
		var legacy v1File
		if err := strictDecode(cleaned, &legacy); err != nil {
			return nil, false, fmt.Errorf("parse %s as version 1: %w", path, err)
		}
		converted, err := translateV1(legacy)
		if err != nil {
			return nil, false, err
		}
		// A file without a version is translated; a file that *says* 1 is already in
		// the only format that ever meant YouTrack, and is reported as such.
		return converted, head.Version == nil, nil
	case FormatV2:
		var current v2File
		if err := strictDecode(cleaned, &current); err != nil {
			return nil, false, fmt.Errorf("parse %s as version 2: %w", path, err)
		}
		return &current, false, nil
	default:
		return nil, false, fmt.Errorf("parse %s: version %d is not a configuration format this binary knows (it knows 1 and 2)", path, version)
	}
}

// strictDecode refuses an unknown field. A configuration file that says something
// the binary does not understand must not be half-honoured: a typo in a key is how
// a policy silently stops applying.
func strictDecode(cleaned []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(cleaned))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// translateV1 renders a version 1 file in the version 2 model.
//
// Version 1 could only mean YouTrack and had no `sources`/`runtimes` blocks, so the
// translation is mechanical: the source is named, the project key moves out of the
// field name, and the alternative keys are renamed.
func translateV1(legacy v1File) (*v2File, error) {
	converted := &v2File{
		RuntimePolicy: legacy.RuntimePolicy,
		Sources:       map[string]v2Source{},
		Projects:      make([]v2Project, 0, len(legacy.Projects)),
	}
	for index, project := range legacy.Projects {
		if strings.TrimSpace(project.YouTrackKey) == "" {
			// Named in the file's own vocabulary: an operator with a version 1 file
			// should be told about `youtrack_key`, not about the field it becomes.
			return nil, fmt.Errorf("project #%d: youtrack_key is required", index+1)
		}
		converted.Projects = append(converted.Projects, v2Project{
			Source:        DefaultSourceName,
			Project:       project.YouTrackKey,
			Also:          project.AlsoKeys,
			Repo:          project.Repo,
			Worktrees:     project.Worktrees,
			Runtime:       project.Runtime,
			Runtimes:      project.Runtimes,
			RuntimePolicy: project.RuntimePolicy,
			Agent:         project.Agent,
			Model:         project.Model,
			Authors:       project.Authors,
			Enabled:       project.Enabled,
		})
	}
	return converted, nil
}

// checkV2 refuses the version 1 spellings with the replacement named. This is the
// one conversion error worth its own code path: it is the mistake a migration
// actually makes.
func checkV2(project v2Project, index int) error {
	if project.YouTrackKey != "" {
		return fmt.Errorf("project #%d: `youtrack_key` is a version 1 field; in version 2 the tracker is named separately, so write `\"source\": %q` and `\"project\": %q` instead",
			index+1, DefaultSourceName, project.YouTrackKey)
	}
	if project.AlsoKeys != nil {
		return fmt.Errorf("project #%d: `also_keys` is a version 1 field; in version 2 it is called `also`", index+1)
	}
	return nil
}

// SourceProblems reports every `sources.<name>` block this binary cannot honour: a
// name it cannot build, named with the list of what it knows, and a `prompt_file` it
// cannot use yet.
//
// The known names are passed in rather than resolved here, because the registry is
// the binary's, not the file's: this package owns the format, `main` owns which
// adapters exist.
func (m *Map) SourceProblems(known []string) []string {
	if m == nil {
		return nil
	}
	buildable := make(map[string]bool, len(known))
	for _, name := range known {
		buildable[strings.ToLower(strings.TrimSpace(name))] = true
	}
	var problems []string
	for _, name := range m.Sources() {
		block := m.sources[name]
		if !buildable[name] {
			problems = append(problems, fmt.Sprintf("sources.%s: this binary cannot build a %q source (it knows: %s)",
				name, name, strings.Join(known, ", ")))
			continue
		}
		if strings.TrimSpace(block.PromptFile) != "" {
			// Deliberately refused rather than ignored: a declared prompt file that
			// silently does nothing is the failure this project refuses elsewhere.
			problems = append(problems, fmt.Sprintf("sources.%s.prompt_file: a prompt file is not implemented yet; remove it and the adapter's built-in prompt is used", name))
		}
	}
	return problems
}

// sortedKeys returns a map's keys in a deterministic order, so a validation error
// names the same problem on every run.
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Provenance is one effective value together with the level that supplied it.
//
// It exists because the configuration now has three levels — the environment, a
// `sources.<name>` or `runtimes.<name>` block, and a `projects[]` entry — and
// "why is this value what it is" has to be answerable from `-print-config` rather
// than by re-deriving the merge in the operator's head.
type Provenance struct {
	// Scope names what the value belongs to: "sources.youtrack", "runtimes.local" or
	// "projects[youtrack:TEST]".
	Scope string
	// Field is the value's own name, as written in the file.
	Field string
	// Value is the effective value, rendered for the report.
	Value string
	// Level names where it came from, spelled the way the operator wrote it: an
	// environment variable, or a path into the file.
	Level string
}

// Provenance lists the effective values the file takes part in, each with its level.
//
// Only values that at least one level actually names are listed: a report that
// printed every field of every entry would bury the interesting lines, and a field
// nobody set is already described by its documented default.
func (m *Map) Provenance(defaults PolicyDefaults) []Provenance {
	if m == nil {
		return nil
	}
	out := make([]Provenance, 0, 8)

	for _, name := range m.Sources() {
		resolved := m.Policy(name, defaults)
		scope := "sources." + name
		for _, field := range []string{"trigger", "start_states", "skip_analyze_on_create", "max_turns", "self_markers"} {
			level, named := resolved.Levels[field]
			if !named {
				continue
			}
			out = append(out, Provenance{
				Scope: scope, Field: field,
				Value: renderPolicyField(resolved.Policy, field),
				Level: level,
			})
		}
	}

	for _, name := range m.Runtimes() {
		block := m.runtimes[name]
		scope := "runtimes." + name
		for _, field := range []string{"url", "auth", "agent", "model", "deadline", "max_concurrent"} {
			if value, named := renderRuntimeField(block, field); named {
				out = append(out, Provenance{Scope: scope, Field: field, Value: value, Level: scope})
			}
		}
	}

	for _, entry := range m.entries {
		// The label, not Index(): the index key carries a NUL separator, and a report
		// with a NUL byte in it is a report nobody can grep.
		scope := "projects[" + entry.Label() + "]"
		if entry.Agent != "" {
			out = append(out, Provenance{Scope: scope, Field: "agent", Value: entry.Agent, Level: scope})
		}
		if entry.Model != "" {
			out = append(out, Provenance{Scope: scope, Field: "model", Value: entry.Model, Level: scope})
		}
		if len(entry.runtimes) > 0 {
			out = append(out, Provenance{Scope: scope, Field: "runtimes", Value: strings.Join(entry.runtimes, ","), Level: scope})
		}
		if strings.TrimSpace(entry.RuntimePolicy) != "" {
			out = append(out, Provenance{Scope: scope, Field: "runtime_policy", Value: entry.Policy(), Level: scope})
		}
	}
	return out
}

// renderPolicyField formats one policy field for the report.
func renderPolicyField(policy rules.Policy, field string) string {
	switch field {
	case "trigger":
		return strconv.Quote(policy.Trigger)
	case "start_states":
		return strings.Join(policy.StartStates, ",")
	case "skip_analyze_on_create":
		return strconv.FormatBool(policy.SkipAnalyzeOnCreate)
	case "max_turns":
		return strconv.Itoa(policy.MaxTurns)
	case "self_markers":
		return strconv.Itoa(len(policy.SelfMarkers)) + " marker(s)"
	default:
		return ""
	}
}

// renderRuntimeField formats one runtime-block field, and reports whether the block
// names it at all.
func renderRuntimeField(block v2Runtime, field string) (string, bool) {
	switch field {
	case "url":
		return block.URL, strings.TrimSpace(block.URL) != ""
	case "auth":
		if block.Auth == nil {
			return "", false
		}
		return block.Auth.User + " (password from $" + block.Auth.PasswordEnv + ")", true
	case "agent":
		return block.Agent, strings.TrimSpace(block.Agent) != ""
	case "model":
		return block.Model, strings.TrimSpace(block.Model) != ""
	case "deadline":
		return block.Deadline, strings.TrimSpace(block.Deadline) != ""
	case "max_concurrent":
		if block.MaxConcurrent == nil {
			return "", false
		}
		return strconv.Itoa(*block.MaxConcurrent), true
	default:
		return "", false
	}
}
