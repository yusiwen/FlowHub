package projectmap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRepo builds a minimal git work tree on disk: validation only needs a .git
// directory and a config file, so the tests stay hermetic and never shell out.
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

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileIsNotFound(t *testing.T) {
	m, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if m == nil || m.Len() != 0 {
		t.Fatalf("map = %+v, want an empty map", m)
	}
}

func TestLoadParsesEntriesAndAliases(t *testing.T) {
	repo := writeRepo(t, filepath.Join(t.TempDir(), "repo"), "git@git.example.cn:me/app.git")
	path := writeFile(t, filepath.Join(t.TempDir(), "projects.json"), `{
	  "projects": [
	    {
	      "youtrack_key": "TEST",
	      "also_keys": ["TEST2", "test"],
	      "repo": {"path": "`+repo+`", "remote": "git@git.example.cn:me/app.git", "default_branch": "main"},
	      "worktrees": "`+filepath.Join(t.TempDir(), "wt")+`",
	      "agent": "devops",
	      "model": "deepseek/deepseek-v4-flash",
	      "authors": ["yusiwen"]
	    }
	  ]
	}`)

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Len() != 1 {
		t.Fatalf("len = %d, want 1", m.Len())
	}
	entry, ok := m.Resolve("TEST")
	if !ok {
		t.Fatal("TEST did not resolve")
	}
	if entry.Agent != "devops" || entry.Repo.DefaultBranch != "main" {
		t.Fatalf("entry = %+v", entry)
	}
	if !entry.IsEnabled() {
		t.Fatal("enabled defaults to true")
	}
	if len(entry.Keys()) != 2 {
		t.Fatalf("keys = %v, want the primary key plus the distinct alias", entry.Keys())
	}
	if problems := m.Validate(); len(problems) != 0 {
		t.Fatalf("Validate: %v", problems)
	}
}

func TestLoadIgnoresDocumentationKeys(t *testing.T) {
	repo := writeRepo(t, filepath.Join(t.TempDir(), "repo"), "")
	path := writeFile(t, filepath.Join(t.TempDir(), "projects.json"), `{
	  "_comment": "explains the file",
	  "projects": [
	    {
	      "_comment": "one project",
	      "youtrack_key": "TEST",
	      "repo": {"_comment": "the checkout", "path": "`+repo+`"}
	    }
	  ]
	}`)

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Len() != 1 {
		t.Fatalf("len = %d, want 1", m.Len())
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	repo := writeRepo(t, filepath.Join(t.TempDir(), "repo"), "")
	path := writeFile(t, filepath.Join(t.TempDir(), "projects.json"), `{
	  "projects": [{"youtrack_key": "TEST", "repo": {"path": "`+repo+`"}, "youtrack_keys": "typo"}]
	}`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "youtrack_keys") {
		t.Fatalf("err = %v, want an unknown-field error naming the typo", err)
	}
}

func TestLoadRejectsStructuralMistakes(t *testing.T) {
	repo := writeRepo(t, filepath.Join(t.TempDir(), "repo"), "")
	cases := map[string]struct {
		body string
		want string
	}{
		"no projects": {
			body: `{"projects": []}`,
			want: "declares no projects",
		},
		"missing key": {
			body: `{"projects": [{"repo": {"path": "` + repo + `"}}]}`,
			want: "youtrack_key is required",
		},
		"missing repo path": {
			body: `{"projects": [{"youtrack_key": "TEST"}]}`,
			want: "repo.path is required",
		},
		"relative repo path": {
			body: `{"projects": [{"youtrack_key": "TEST", "repo": {"path": "relative/dir"}}]}`,
			want: "must be absolute",
		},
		"relative worktrees": {
			body: `{"projects": [{"youtrack_key": "TEST", "repo": {"path": "` + repo + `"}, "worktrees": "wt"}]}`,
			want: "worktrees \"wt\" must be absolute",
		},
		"duplicate key": {
			body: `{"projects": [
			  {"youtrack_key": "TEST", "repo": {"path": "` + repo + `"}},
			  {"youtrack_key": "test", "repo": {"path": "` + repo + `"}}
			]}`,
			want: "claimed by both",
		},
		"alias collides with another entry": {
			body: `{"projects": [
			  {"youtrack_key": "A", "also_keys": ["B"], "repo": {"path": "` + repo + `"}},
			  {"youtrack_key": "B", "repo": {"path": "` + repo + `"}}
			]}`,
			want: "claimed by both",
		},
		"malformed json": {
			body: `{"projects": [`,
			want: "parse projects file",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "projects.json"), tc.body)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load succeeded, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestMatchPrefersProjectKeyAndFallsBackToIssuePrefix(t *testing.T) {
	repo := writeRepo(t, filepath.Join(t.TempDir(), "repo"), "")
	path := writeFile(t, filepath.Join(t.TempDir(), "projects.json"), `{
	  "projects": [{"youtrack_key": "TEST", "repo": {"path": "`+repo+`"}}]
	}`)
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	match, ok := m.Match("TEST", "TEST-12")
	if !ok || match.Via != ViaProjectKey || match.Key != "TEST" {
		t.Fatalf("match = %+v ok=%t, want project.key", match, ok)
	}

	// A payload without a project object must still route, via the issue prefix.
	match, ok = m.Match("", "TEST-12")
	if !ok || match.Via != ViaIssueIDPrefix || match.Key != "TEST" {
		t.Fatalf("match = %+v ok=%t, want issue_id_prefix", match, ok)
	}

	// Matching is case-insensitive: a key typo must not silently disable routing.
	if _, ok := m.Match("test", ""); !ok {
		t.Fatal("lower-case key did not match")
	}

	// An unknown project key must NOT silently fall back to a known prefix: a
	// payload that names a different project is a different project, so this is a
	// hard miss rather than a routing guess.
	if match, ok := m.Match("OTHER", "TEST-12"); ok {
		t.Fatalf("unknown project key fell back to the issue prefix: %+v", match)
	}
	for _, issue := range []string{"NOPE-1", "no-dash", ""} {
		if _, ok := m.Match("", issue); ok {
			t.Fatalf("Match(\"\", %q) matched, want no match", issue)
		}
	}
}

func TestMatchSkipsDisabledEntries(t *testing.T) {
	repo := writeRepo(t, filepath.Join(t.TempDir(), "repo"), "")
	path := writeFile(t, filepath.Join(t.TempDir(), "projects.json"), `{
	  "projects": [{"youtrack_key": "TEST", "repo": {"path": "`+repo+`"}, "enabled": false}]
	}`)
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := m.Match("TEST", "TEST-1"); ok {
		t.Fatal("a disabled entry must never match")
	}
	// A disabled entry is not validated: it may point at a checkout this host
	// does not have.
	if problems := m.Validate(); len(problems) != 0 {
		t.Fatalf("Validate = %v, want none for a disabled entry", problems)
	}
}

func TestValidateReportsFilesystemProblems(t *testing.T) {
	base := t.TempDir()
	good := writeRepo(t, filepath.Join(base, "good"), "git@example.cn:me/app.git")
	notGit := filepath.Join(base, "not-git")
	if err := os.MkdirAll(notGit, 0o755); err != nil {
		t.Fatal(err)
	}
	wrongRemote := writeRepo(t, filepath.Join(base, "wrong-remote"), "git@example.cn:me/other.git")

	cases := map[string]struct {
		entry Entry
		want  string
	}{
		"missing path": {
			entry: Entry{YouTrackKey: "A", Repo: Repo{Path: filepath.Join(base, "absent")}},
			want:  "no such file",
		},
		"not a git work tree": {
			entry: Entry{YouTrackKey: "B", Repo: Repo{Path: notGit}},
			want:  "not a git work tree",
		},
		"remote mismatch": {
			entry: Entry{YouTrackKey: "C", Repo: Repo{Path: wrongRemote, Remote: "git@example.cn:me/app.git"}},
			want:  "but the mapping declares",
		},
		"worktrees inside the repo": {
			entry: Entry{YouTrackKey: "D", Repo: Repo{Path: good}, Worktrees: filepath.Join(good, "wt")},
			want:  "is inside the repository",
		},
		"worktrees equal to the repo": {
			entry: Entry{YouTrackKey: "E", Repo: Repo{Path: good}, Worktrees: good},
			want:  "must differ from repo.path",
		},
		"bad agent name": {
			entry: Entry{YouTrackKey: "F", Repo: Repo{Path: good}, Agent: "dev ops/../x"},
			want:  "not a valid agent name",
		},
		"bare model name": {
			entry: Entry{YouTrackKey: "H", Repo: Repo{Path: good}, Model: "gpt-5"},
			want:  "must be spelled provider/model-id",
		},
		"model without a name": {
			entry: Entry{YouTrackKey: "I", Repo: Repo{Path: good}, Model: "deepseek/"},
			want:  "must be spelled provider/model-id",
		},
		"provider qualified model": {
			entry: Entry{YouTrackKey: "J", Repo: Repo{Path: good}, Model: "deepseek/deepseek-v4-flash"},
			want:  "",
		},
		"ok": {
			entry: Entry{YouTrackKey: "G", Repo: Repo{Path: good, Remote: "git@example.cn:me/app.git"}, Agent: "devops"},
			want:  "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &Map{byKey: map[string]*Entry{}, entries: []*Entry{&tc.entry}}
			problems := m.Validate()
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
		})
	}
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

func TestCanonicalizeResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got := canonicalize(link); got != canonicalize(real) {
		t.Fatalf("canonicalize(%q) = %q, want %q", link, got, canonicalize(real))
	}
	// A path that does not exist yet is rebuilt from its deepest existing parent.
	future := filepath.Join(link, "worktrees", "TEST-1")
	got := canonicalize(future)
	if !strings.HasPrefix(got, canonicalize(real)) {
		t.Fatalf("canonicalize(%q) = %q, want it under %q", future, got, canonicalize(real))
	}
}

func TestIssueIDPrefix(t *testing.T) {
	cases := map[string]string{
		"TEST-11": "TEST",
		"TEST-1":  "TEST",
		"2-123":   "2",
		"no-dash": "no",
		"":        "",
		"-odd":    "",
	}
	for id, want := range cases {
		if got := IssueIDPrefix(id); got != want {
			t.Errorf("IssueIDPrefix(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestReportRendersTheTable(t *testing.T) {
	repo := writeRepo(t, filepath.Join(t.TempDir(), "repo"), "git@example.cn:me/app.git")
	worktrees := filepath.Join(t.TempDir(), "wt")
	path := writeFile(t, filepath.Join(t.TempDir(), "projects.json"), `{
	  "projects": [
	    {"youtrack_key": "TEST", "also_keys": ["DEV"], "repo": {"path": "`+repo+`", "remote": "git@example.cn:me/app.git", "default_branch": "main"},
	     "worktrees": "`+worktrees+`", "agent": "devops", "authors": ["yusiwen"]},
	    {"youtrack_key": "OLD", "repo": {"path": "`+repo+`"}, "enabled": false}
	  ]
	}`)
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	report := m.Report()
	for _, want := range []string{
		"projects_file:      " + path,
		"projects:           2 mapping(s)",
		"TEST,DEV",
		"remote git@example.cn:me/app.git",
		"branch main",
		"agent=devops authors=yusiwen",
		"[disabled]",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report is missing %q:\n%s", want, report)
		}
	}
}

func TestReportOnEmptyMapExplainsItself(t *testing.T) {
	var m *Map
	report := m.Report()
	if !strings.Contains(report, "never routed") {
		t.Fatalf("empty report = %q", report)
	}
}

// A disabled entry must not be advertised as routable: the startup banner and
// /healthz read these, and listing a key that can never match would mislead.
func TestKeysAndRoutableExcludeDisabledEntries(t *testing.T) {
	repo := writeRepo(t, filepath.Join(t.TempDir(), "repo"), "")
	path := writeFile(t, filepath.Join(t.TempDir(), "projects.json"), `{
	  "projects": [
	    {"youtrack_key": "LIVE", "also_keys": ["DEV"], "repo": {"path": "`+repo+`"}},
	    {"youtrack_key": "PLACEHOLDER", "repo": {"path": "/absolute/path/to/your/repo"}, "enabled": false}
	  ]
	}`)
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := m.Len(), 2; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
	if got, want := m.Routable(), 1; got != want {
		t.Fatalf("Routable() = %d, want %d", got, want)
	}
	if got := strings.Join(m.Keys(), ","); got != "DEV,LIVE" {
		t.Fatalf("Keys() = %q, want the enabled keys only", got)
	}
	if _, ok := m.Match("PLACEHOLDER", "PLACEHOLDER-1"); ok {
		t.Fatal("a disabled entry matched")
	}
	// The disabled entry still needs no valid path, so validation passes.
	if problems := m.Validate(); len(problems) != 0 {
		t.Fatalf("Validate = %v, want none", problems)
	}
	report := m.Report()
	if !strings.Contains(report, "2 mapping(s), 1 routable") {
		t.Fatalf("report does not distinguish routable entries:\n%s", report)
	}
}

// TestRuntimeAddressingIsParsedAndResolved covers ADR 0001 step 5's configuration
// surface: a project may name one runtime or several, and the selection policy is
// either the table's default or its own.
func TestRuntimeAddressingIsParsedAndResolved(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "config.json"), `{
		"runtime_policy": "first-healthy",
		"projects": [
			{"youtrack_key":"A","repo":{"path":"/tmp/a"},"runtime":"builder-a"},
			{"youtrack_key":"B","repo":{"path":"/tmp/b"},"runtimes":["builder-a","builder-b","builder-a"]},
			{"youtrack_key":"C","repo":{"path":"/tmp/c"},"runtime":"builder-c","runtimes":["builder-d"],"runtime_policy":"spread"}
		]
	}`)
	projects, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if projects.Policy() != PolicyFirstHealthy {
		t.Fatalf("table policy = %q", projects.Policy())
	}

	byKey := map[string]*Entry{}
	for _, entry := range projects.Entries() {
		byKey[entry.YouTrackKey] = entry
	}
	if got := byKey["A"].RuntimeSet(); len(got) != 1 || got[0] != "builder-a" {
		t.Fatalf("A runtimes = %v", got)
	}
	// A single name and a list are the same set, and duplicates collapse.
	if got := byKey["B"].RuntimeSet(); len(got) != 2 || got[0] != "builder-a" || got[1] != "builder-b" {
		t.Fatalf("B runtimes = %v", got)
	}
	// Both spellings together union, and the entry's policy wins over the table's.
	if got := byKey["C"].RuntimeSet(); len(got) != 2 || got[0] != "builder-c" || got[1] != "builder-d" {
		t.Fatalf("C runtimes = %v", got)
	}
	if byKey["C"].Policy() != PolicySpread {
		t.Fatalf("C policy = %q, want the entry to override the table", byKey["C"].Policy())
	}
	if byKey["A"].Policy() != PolicyFirstHealthy {
		t.Fatalf("A policy = %q, want the table default", byKey["A"].Policy())
	}
	// The report names them, because that is what an operator reads.
	report := projects.Report()
	for _, want := range []string{"builder-a", "runtime_policy", PolicyFirstHealthy} {
		if !strings.Contains(report, want) {
			t.Fatalf("report does not mention %q:\n%s", want, report)
		}
	}
}

// TestRuntimeAddressingDefaultsToSpread: a table that says nothing spreads, which is
// the ADR's default and the only policy that uses a second machine.
func TestRuntimeAddressingDefaultsToSpread(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "config.json"),
		`{"projects":[{"youtrack_key":"A","repo":{"path":"/tmp/a"}}]}`)
	projects, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if projects.Policy() != PolicySpread {
		t.Fatalf("table policy = %q, want spread", projects.Policy())
	}
	if entry := projects.Entries()[0]; entry.Policy() != PolicySpread || len(entry.RuntimeSet()) != 0 {
		t.Fatalf("entry = %+v, want the default policy and no declared runtimes", entry)
	}
}

// TestRuntimeAddressingRejectsTypos keeps the failure at load time: a policy value
// that does not exist, and a name that could never be enrolled, are both refused
// rather than silently making a project unroutable.
func TestRuntimeAddressingRejectsTypos(t *testing.T) {
	cases := map[string]string{
		"unknown policy":     `{"runtime_policy":"balanced","projects":[{"youtrack_key":"A","repo":{"path":"/tmp/a"}}]}`,
		"entry policy":       `{"projects":[{"youtrack_key":"A","repo":{"path":"/tmp/a"},"runtime_policy":"round-robin"}]}`,
		"uppercase name":     `{"projects":[{"youtrack_key":"A","repo":{"path":"/tmp/a"},"runtime":"Builder-A"}]}`,
		"name with a space":  `{"projects":[{"youtrack_key":"A","repo":{"path":"/tmp/a"},"runtimes":["builder a"]}]}`,
		"name with a slash":  `{"projects":[{"youtrack_key":"A","repo":{"path":"/tmp/a"},"runtime":"a/b"}]}`,
		"empty list element": `{"projects":[{"youtrack_key":"A","repo":{"path":"/tmp/a"},"runtimes":[""]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "config.json"), body)
			if name == "empty list element" {
				// An empty string is skipped, not refused: it is how a trailing comma
				// in a hand-edited list arrives, and it names nothing.
				if _, err := Load(path); err != nil {
					t.Fatalf("an empty element should be ignored, got %v", err)
				}
				return
			}
			if _, err := Load(path); err == nil {
				t.Fatal("a typo was accepted")
			}
		})
	}
}
