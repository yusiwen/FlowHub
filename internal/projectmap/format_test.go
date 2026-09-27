package projectmap

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/rules"
)

// writeConfig writes a configuration file body and loads it.
func writeConfig(t *testing.T, body string) *Map {
	t.Helper()
	path := writeFile(t, filepath.Join(t.TempDir(), "config.json"), body)
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m
}

// loadErr loads a body that must be refused, and returns the error text.
func loadErr(t *testing.T, body string) string {
	t.Helper()
	path := writeFile(t, filepath.Join(t.TempDir(), "config.json"), body)
	m, err := Load(path)
	if err == nil {
		t.Fatalf("Load accepted a file it must refuse; map = %+v", m)
	}
	return err.Error()
}

// TestV1AndV2RouteAndDecideIdentically is step 3's acceptance case: the file format
// changed, so an operator who migrates it must get the same routing and the same
// trigger policy. The two bodies below are the same table written both ways.
func TestV1AndV2RouteAndDecideIdentically(t *testing.T) {
	repo := t.TempDir()
	worktrees := filepath.Join(t.TempDir(), "worktrees")

	legacy := writeConfig(t, `{
	  "runtime_policy": "first-healthy",
	  "projects": [
	    {"youtrack_key": "TEST", "also_keys": ["TEST2"], "repo": {"path": "`+repo+`", "remote": "git@example.cn:me/app.git", "default_branch": "main"},
	     "worktrees": "`+worktrees+`", "runtime": "builder-a", "agent": "devops", "model": "deepseek/deepseek-v4-flash", "authors": ["yusiwen"]},
	    {"youtrack_key": "OFF", "repo": {"path": "`+repo+`"}, "enabled": false}
	  ]
	}`)

	current := writeConfig(t, `{
	  "version": 2,
	  "runtime_policy": "first-healthy",
	  "sources": {"youtrack": {"enabled": true}},
	  "runtimes": {"builder-a": {"url": "https://builder-a.lan:4096", "agent": "devops"}},
	  "projects": [
	    {"source": "youtrack", "project": "TEST", "also": ["TEST2"],
	     "repo": {"path": "`+repo+`", "remote": "git@example.cn:me/app.git", "default_branch": "main"},
	     "worktrees": "`+worktrees+`", "runtime": "builder-a", "agent": "devops", "model": "deepseek/deepseek-v4-flash", "authors": ["yusiwen"]},
	    {"source": "youtrack", "project": "OFF", "repo": {"path": "`+repo+`"}, "enabled": false}
	  ]
	}`)

	if legacy.Format() != FormatV1 || !legacy.Translated() {
		t.Fatalf("v1 file: format = %d translated = %t", legacy.Format(), legacy.Translated())
	}
	if current.Format() != FormatV2 || current.Translated() {
		t.Fatalf("v2 file: format = %d translated = %t", current.Format(), current.Translated())
	}

	// Same routing: the same project keys resolve to the same repository, the same
	// aliases work, and the same sources miss.
	for _, project := range []string{"TEST", "test", "TEST2"} {
		left, okLeft := legacy.Match("youtrack", project)
		right, okRight := current.Match("youtrack", project)
		if !okLeft || !okRight {
			t.Fatalf("Match(%q): v1 = %t, v2 = %t", project, okLeft, okRight)
		}
		if left.Entry.Repo.Path != right.Entry.Repo.Path || left.Entry.Worktrees != right.Entry.Worktrees {
			t.Fatalf("routing differs for %q: v1 = %+v, v2 = %+v", project, left.Entry, right.Entry)
		}
		if left.Entry.Policy() != right.Entry.Policy() || left.Entry.Policy() != PolicyFirstHealthy {
			t.Fatalf("selection policy differs for %q: %q vs %q", project, left.Entry.Policy(), right.Entry.Policy())
		}
	}
	for _, project := range []string{"NOPE", ""} {
		if _, ok := legacy.Match("youtrack", project); ok {
			t.Fatalf("v1 matched %q", project)
		}
		if _, ok := current.Match("youtrack", project); ok {
			t.Fatalf("v2 matched %q", project)
		}
	}
	if _, ok := current.Match("gitea", "TEST"); ok {
		t.Fatal("v2 matched a project in a source that does not declare it")
	}

	// Same effective policy. The v2 file has a policy block and the v1 file does not,
	// so the values have to agree while their *levels* differ — which is exactly what
	// the report exists to show.
	defaults := PolicyDefaults{
		Trigger: "/opencode start", StartStates: []string{"In Progress"},
		SkipAnalyzeOnCreate: false, MaxTurns: 8,
	}
	left := legacy.Policy("youtrack", defaults)
	right := current.Policy("youtrack", defaults)
	if !samePolicy(left.Policy, right.Policy) {
		t.Fatalf("effective policy differs:\nv1 = %+v\nv2 = %+v", left.Policy, right.Policy)
	}
	if left.Levels["trigger"] != "FLOWHUB_TRIGGER" {
		t.Fatalf("v1 trigger level = %q, want the environment", left.Levels["trigger"])
	}
}

// samePolicy compares two resolved policies field by field: rules.Policy holds a
// slice, so it is not comparable with ==.
func samePolicy(a, b rules.Policy) bool {
	return a.Trigger == b.Trigger &&
		a.SkipAnalyzeOnCreate == b.SkipAnalyzeOnCreate &&
		a.MaxTurns == b.MaxTurns &&
		a.WriteAccess == b.WriteAccess &&
		strings.Join(a.StartStates, "\x00") == strings.Join(b.StartStates, "\x00") &&
		strings.Join(a.SelfMarkers, "\x00") == strings.Join(b.SelfMarkers, "\x00")
}

// TestV2RefusesTheVersionOneSpellings is the migration mistake worth its own error:
// a file that says version 2 but still writes `youtrack_key`.
func TestV2RefusesTheVersionOneSpellings(t *testing.T) {
	err := loadErr(t, `{"version": 2, "projects": [{"youtrack_key": "TEST", "repo": {"path": "/tmp/x"}}]}`)
	for _, want := range []string{"youtrack_key", "version 1 field", `"source"`, `"project"`} {
		if !strings.Contains(err, want) {
			t.Fatalf("error does not name %q: %s", want, err)
		}
	}

	err = loadErr(t, `{"version": 2, "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "/tmp/x"}, "also_keys": ["A"]}]}`)
	if !strings.Contains(err, "also_keys") || !strings.Contains(err, "`also`") {
		t.Fatalf("error does not name the replacement for also_keys: %s", err)
	}
}

func TestV2RefusesAnUnknownVersionAndUnknownField(t *testing.T) {
	if err := loadErr(t, `{"version": 3, "projects": []}`); !strings.Contains(err, "version 3 is not a configuration format") {
		t.Fatalf("unknown version: %s", err)
	}
	if err := loadErr(t, `{"version": 2, "projects": [], "sorces": {}}`); !strings.Contains(err, "sorces") {
		t.Fatalf("a typo in a top-level key was not refused: %s", err)
	}
}

// TestSourceIndexIsPerSource is the mechanical reason for the format change: a
// YouTrack project and a Gitea project may share a key, but not within one source.
func TestSourceIndexIsPerSource(t *testing.T) {
	one := filepath.Join(t.TempDir(), "one")
	two := filepath.Join(t.TempDir(), "two")
	m := writeConfig(t, `{
	  "version": 2,
	  "projects": [
	    {"source": "youtrack", "project": "TEST", "repo": {"path": "`+one+`"}},
	    {"source": "gitea", "project": "TEST", "repo": {"path": "`+two+`"}}
	  ]
	}`)
	left, ok := m.Resolve("youtrack", "TEST")
	if !ok || left.Repo.Path != canonicalize(one) {
		t.Fatalf("youtrack:TEST = %+v", left)
	}
	right, ok := m.Resolve("gitea", "TEST")
	if !ok || right.Repo.Path != canonicalize(two) {
		t.Fatalf("gitea:TEST = %+v", right)
	}
	if left.Index() == right.Index() {
		t.Fatalf("both entries share the index key %q", left.Index())
	}

	err := loadErr(t, `{
	  "version": 2,
	  "projects": [
	    {"source": "youtrack", "project": "TEST", "repo": {"path": "`+one+`"}},
	    {"source": "youtrack", "project": "test", "repo": {"path": "`+two+`"}}
	  ]
	}`)
	if !strings.Contains(err, "claimed by both") {
		t.Fatalf("a duplicate inside one source was not refused: %s", err)
	}
}

// TestSourceProblemsNamesWhatTheBinaryCanBuild covers the fail-closed rule for a
// source the file names: refusing by name, with the list of what exists, is one
// line for the operator instead of a delivery that can never be decoded.
func TestSourceProblemsNamesWhatTheBinaryCanBuild(t *testing.T) {
	m := writeConfig(t, `{
	  "version": 2,
	  "sources": {"gitlab": {"enabled": true}},
	  "projects": [{"source": "gitlab", "project": "T", "repo": {"path": "/tmp/x"}}]
	}`)
	problems := m.SourceProblems([]string{"youtrack"})
	if len(problems) != 1 {
		t.Fatalf("problems = %v", problems)
	}
	if !strings.Contains(problems[0], "sources.gitlab") || !strings.Contains(problems[0], "youtrack") {
		t.Fatalf("problem does not name the source and what is known: %s", problems[0])
	}

}

// TestRuntimeBlocksCarryPolicyAndRefuseWhatCannotBeHonoured pins the runtime table:
// where a host is and what to ask of it, with the two things that must not be
// accepted silently.
func TestRuntimeBlocksCarryPolicyAndRefuseWhatCannotBeHonoured(t *testing.T) {
	m := writeConfig(t, `{
	  "version": 2,
	  "runtimes": {
	    "local": {"url": "http://127.0.0.1:4096", "agent": "devops", "deadline": "20m", "max_concurrent": 1,
	              "auth": {"user": "flowhub", "password_env": "FLOWHUB_LOCAL_PASSWORD"}}
	  },
	  "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "/tmp/x"}, "runtime": "local"}]
	}`)
	block, ok := m.RuntimeBlock("local")
	if !ok {
		t.Fatal("the local runtime block was not parsed")
	}
	if block.URL != "http://127.0.0.1:4096" || block.Agent != "devops" || block.Deadline != "20m" {
		t.Fatalf("block = %+v", block)
	}
	if block.Auth == nil || block.Auth.PasswordEnv != "FLOWHUB_LOCAL_PASSWORD" {
		t.Fatalf("auth = %+v", block.Auth)
	}
	if problems := m.RuntimeProblems(); len(problems) != 0 {
		t.Fatalf("RuntimeProblems = %v", problems)
	}
	if got := m.Runtimes(); len(got) != 1 || got[0] != "local" {
		t.Fatalf("Runtimes = %v", got)
	}

	if got, _ := m.RuntimeBlock("local"); got.MaxConcurrentTasks() != 1 {
		t.Fatalf("MaxConcurrentTasks = %d, want 1", got.MaxConcurrentTasks())
	}
	// A breadth above one is accepted (ADR 0003): it is how many distinct tasks the
	// host may serve at once, not how many prompts one session may get.
	wider := writeConfig(t, `{"version": 2, "runtimes": {"local": {"url": "http://127.0.0.1:4096", "max_concurrent": 4}},
	  "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "/tmp/x"}, "runtime": "local"}]}`)
	widerBlock, ok := wider.RuntimeBlock("local")
	if !ok || widerBlock.MaxConcurrentTasks() != 4 {
		t.Fatalf("a breadth of four was not carried: %+v", widerBlock)
	}

	cases := map[string]string{
		"a breadth of zero":           `{"max_concurrent": 0}`,
		"a negative breadth":          `{"max_concurrent": -2}`,
		"a breadth above the maximum": `{"max_concurrent": 65}`,
		"a bare host":                 `{"url": "builder-a.lan:4096"}`,
		"an unparsable deadline":      `{"url": "http://h:1", "deadline": "soon"}`,
		"half a credential pair":      `{"url": "http://h:1", "auth": {"user": "flowhub"}}`,
		"an empty user":               `{"url": "http://h:1", "auth": {"password_env": "X"}}`,
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			err := loadErr(t, `{"version": 2, "runtimes": {"builder-a": `+block+`},
			  "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "/tmp/x"}, "runtime": "builder-a"}]}`)
			if !strings.Contains(err, "builder-a") {
				t.Fatalf("the refusal does not name the runtime: %s", err)
			}
		})
	}
}

// TestProvenanceNamesTheLevelOfEveryValue is the second acceptance criterion: the
// report has to answer "which level supplied this" for each value, because the
// whole point of three levels is that the answer is not obvious.
func TestProvenanceNamesTheLevelOfEveryValue(t *testing.T) {
	m := writeConfig(t, `{
	  "version": 2,
	  "sources": {"youtrack": {"policy": {"trigger": "/flowhub go", "start_states": ["Doing"]}}},
	  "runtimes": {"local": {"url": "http://127.0.0.1:4096", "agent": "devops"}},
	  "projects": [{"source": "youtrack", "project": "TEST", "repo": {"path": "/tmp/x"},
	                "runtime": "local", "model": "deepseek/deepseek-v4-flash", "runtimes": ["local", "builder-a"]}]
	}`)
	defaults := PolicyDefaults{Trigger: "/opencode start", StartStates: []string{"In Progress"}, MaxTurns: 8}

	levels := map[string]string{}
	values := map[string]string{}
	for _, value := range m.Provenance(defaults) {
		levels[value.Scope+"."+value.Field] = value.Level
		values[value.Scope+"."+value.Field] = value.Value
	}

	for key, want := range map[string]string{
		"sources.youtrack.trigger":         "sources.youtrack.policy",
		"sources.youtrack.start_states":    "sources.youtrack.policy",
		"sources.youtrack.max_turns":       "FLOWHUB_MAX_TURNS",
		"runtimes.local.url":               "runtimes.local",
		"runtimes.local.agent":             "runtimes.local",
		"projects[youtrack:TEST].model":    "projects[youtrack:TEST]",
		"projects[youtrack:TEST].runtimes": "projects[youtrack:TEST]",
	} {
		if got, ok := levels[key]; !ok {
			t.Fatalf("no provenance for %s (have %v)", key, levels)
		} else if got != want {
			t.Fatalf("%s came from %q, want %q", key, got, want)
		}
	}
	// The file's value is the effective one, and it is reported as such.
	if values["sources.youtrack.trigger"] != `"/flowhub go"` {
		t.Fatalf("effective trigger = %s", values["sources.youtrack.trigger"])
	}
	resolved := m.Policy("youtrack", defaults)
	if resolved.Policy.Trigger != "/flowhub go" || len(resolved.Policy.StartStates) != 1 || resolved.Policy.StartStates[0] != "Doing" {
		t.Fatalf("resolved policy = %+v", resolved.Policy)
	}
	if resolved.Policy.MaxTurns != 8 {
		t.Fatalf("max_turns = %d, want the environment default 8", resolved.Policy.MaxTurns)
	}

	report := m.Report()
	for _, want := range []string{"config_format:      2", "youtrack:TEST"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report is missing %q:\n%s", want, report)
		}
	}
}

// TestSourceEnabledAndAuthors covers the two source-level gates a project inherits:
// a disabled source and a source-level author allowlist.
func TestSourceEnabledAndAuthors(t *testing.T) {
	m := writeConfig(t, `{
	  "version": 2,
	  "sources": {"youtrack": {"enabled": false, "authors": ["yusiwen"]}},
	  "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "/tmp/x"}}]
	}`)
	if m.SourceEnabled("youtrack") {
		t.Fatal("a disabled source reports itself enabled")
	}
	if got := m.SourceAuthors("youtrack"); len(got) != 1 || got[0] != "yusiwen" {
		t.Fatalf("authors = %v", got)
	}
	// A source the file never mentions stays enabled: the hook key is the lock, and
	// requiring a block would make the file mandatory for a record-only receiver.
	if !m.SourceEnabled("gitea") {
		t.Fatal("an undeclared source reports itself disabled")
	}
	if got := m.SourceAuthors("gitea"); got != nil {
		t.Fatalf("authors for an undeclared source = %v", got)
	}
}

// TestPromptFilesAreResolvedAgainstTheConfigurationFile covers the prompt_file
// contract: the text is read when the configuration is loaded and handed to the
// adapter, so a file that is missing, empty or a directory refuses the start instead
// of quietly doing nothing.
func TestPromptFilesAreResolvedAgainstTheConfigurationFile(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	// A relative path is resolved against the configuration file's directory, not the
	// process's working directory: a prompt file belongs to the deployment.
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePrompt := filepath.Join(dir, "prompts", "source.md")
	if err := os.WriteFile(sourcePrompt, []byte("Source-wide: be brief.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectPrompt := filepath.Join(dir, "prompts", "project.md")
	if err := os.WriteFile(projectPrompt, []byte("This repository is Go: run `make test`.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "config.json")
	body := `{
	  "version": 2,
	  "sources": {"youtrack": {"prompt_file": "prompts/source.md"}},
	  "projects": [{"source": "youtrack", "project": "TEST", "repo": {"path": "` + repo + `"},
	                "prompt_file": "prompts/project.md"}]
	}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := m.SourcePromptExtra("youtrack"); !strings.Contains(got, "be brief") {
		t.Fatalf("source prompt extra = %q", got)
	}
	if got := m.Entries()[0].PromptExtra; !strings.Contains(got, "make test") {
		t.Fatalf("project prompt extra = %q", got)
	}
	// An absolute path works too, and a source the file does not declare has none.
	if err := os.WriteFile(filepath.Join(dir, "absolute.md"), []byte("Absolute.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	absolute := writeConfig(t, `{
	  "version": 2,
	  "sources": {"youtrack": {"prompt_file": "`+filepath.Join(dir, "absolute.md")+`"}},
	  "projects": [{"source": "youtrack", "project": "TEST", "repo": {"path": "`+repo+`"}}]
	}`)
	if got := absolute.SourcePromptExtra("youtrack"); !strings.Contains(got, "Absolute") {
		t.Fatalf("absolute prompt extra = %q", got)
	}
	if got := absolute.SourcePromptExtra("gitea"); got != "" {
		t.Fatalf("an undeclared source has prompt extra %q", got)
	}

	// Refusals: missing, empty, a directory, and oversize.
	cases := map[string]string{
		"missing":   "prompts/absent.md",
		"empty":     "prompts/empty.md",
		"directory": "prompts",
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "empty.md"), []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			err := loadErr(t, `{"version": 2, "sources": {"youtrack": {"prompt_file": "`+path+`"}},
			  "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "`+repo+`"}}]}`)
			if !strings.Contains(err, "prompt_file") || !strings.Contains(err, "sources.youtrack") {
				t.Fatalf("the refusal does not name the field: %s", err)
			}
		})
	}
	big := filepath.Join(dir, "prompts", "big.md")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), maxPromptFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	oversize := loadErr(t, `{"version": 2, "sources": {"youtrack": {"prompt_file": "`+big+`"}},
	  "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "`+repo+`"}}]}`)
	if !strings.Contains(oversize, "over the") {
		t.Fatalf("an oversize prompt file was not refused by size: %s", oversize)
	}

	// A version 1 file has no sources block, so it can carry no source prompt; the
	// project-level field is what a version 1 file's operator cannot express either.
	legacy := writeConfig(t, `{"projects": [{"youtrack_key": "TEST", "repo": {"path": "`+repo+`"}}]}`)
	if got := legacy.SourcePromptExtra("youtrack"); got != "" {
		t.Fatalf("a translated file produced prompt extra %q", got)
	}
	if got := legacy.Entries()[0].PromptExtra; got != "" {
		t.Fatalf("a translated entry produced prompt extra %q", got)
	}
}
