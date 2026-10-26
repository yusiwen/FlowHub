package projectmap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	lockKeyA   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	lockKeyB   = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	lockTokenA = "1111111111111111111111111111111111111111111111111111111111111111"
	lockTokenB = "2222222222222222222222222222222222222222222222222222222222222222"
)

// sourceObject renders the per-project binding the receiver resolves a delivery
// with. Tests build the JSON by concatenation because the fixtures also splice
// temporary repository paths into the body.
func sourceObject(name, key, token string) string {
	return `{"name": "` + name + `", "hook_key": "` + key + `", "token": "` + token + `"}`
}

// The URL key is what attributes a delivery to a project, so the map must hand out
// exactly one lock per enabled entry of the served source, with the entry's own
// token and every tracker project it answers to.
func TestEntryLocksAreScopedToTheProject(t *testing.T) {
	repoA, repoB, repoOff := t.TempDir(), t.TempDir(), t.TempDir()
	m := writeConfig(t, `{"version": 2, "projects": [
	  {"source": `+sourceObject("youtrack", lockKeyA, lockTokenA)+`, "project": "A", "repo": {"path": "`+repoA+`"}},
	  {"source": `+sourceObject("youtrack", lockKeyB, lockTokenB)+`, "project": "B", "also": ["B2"], "repo": {"path": "`+repoB+`"}},
	  {"source": `+sourceObject("youtrack", lockKeyA, lockTokenA)+`, "project": "OFF", "repo": {"path": "`+repoOff+`"}, "enabled": false}
	]}`)

	locks := m.EntryLocks("YouTrack")
	if len(locks) != 2 {
		t.Fatalf("EntryLocks = %+v, want the two enabled entries", locks)
	}
	if locks[0].Entry != "youtrack:A" || locks[0].Key != lockKeyA || locks[0].Token != lockTokenA {
		t.Fatalf("first lock = %+v", locks[0])
	}
	if got := locks[1].Projects; len(got) != 2 || got[0] != "B" || got[1] != "B2" {
		t.Fatalf("second lock projects = %v, want B and its alias", got)
	}
	if got := m.EntryLocks("gitea"); len(got) != 0 {
		t.Fatalf("EntryLocks(gitea) = %+v, want none: a lock belongs to its own source", got)
	}
}

// The string form predates the locks and still parses, but it cannot address a
// project: LockProblems is what turns that into a startup refusal for the source
// this process actually receives.
func TestStringSourceNeedsLockProblemsButStillLoads(t *testing.T) {
	repo := t.TempDir()
	body := `{"version": 2, "projects": [
	  {"source": "youtrack", "project": "A", "repo": {"path": "` + repo + `"}},
	  {"source": "youtrack", "project": "OFF", "repo": {"path": "` + repo + `"}, "enabled": false}
	]}`
	m := writeConfig(t, body)

	if got := m.EntryLocks("youtrack"); len(got) != 0 {
		t.Fatalf("EntryLocks = %+v, want none without lock material", got)
	}
	if report := m.Report(); !strings.Contains(report, "locks MISSING") {
		t.Fatalf("report claims locks an entry does not have:\n%s", report)
	}
	problems := m.LockProblems("youtrack")
	if len(problems) != 2 {
		t.Fatalf("LockProblems = %v, want hook_key and token for the enabled entry only", problems)
	}
	for _, want := range []string{"youtrack:A", "source.hook_key", "source.token"} {
		if !strings.Contains(strings.Join(problems, "\n"), want) {
			t.Fatalf("problems are missing %q: %v", want, problems)
		}
	}
	if problems := m.LockProblems("gitea"); len(problems) != 0 {
		t.Fatalf("LockProblems(gitea) = %v, want none: this process does not serve it", problems)
	}
}

// A version 1 file cannot express the locks. The loader still translates it so the
// report can explain the table, but the startup check must name the migration.
func TestVersionOneExplainsTheLockMigration(t *testing.T) {
	repo := t.TempDir()
	m := writeConfig(t, `{"projects": [
	  {"youtrack_key": "A", "repo": {"path": "`+repo+`"}}
	]}`)
	if report := m.Report(); !strings.Contains(report, "locks MISSING") {
		t.Fatalf("report claims locks an entry does not have:\n%s", report)
	}
	problems := m.LockProblems("youtrack")
	if len(problems) != 2 {
		t.Fatalf("LockProblems = %v", problems)
	}
	for _, problem := range problems {
		if !strings.Contains(problem, "version 1 file cannot express entry locks") {
			t.Fatalf("problem does not explain the version 1 migration: %q", problem)
		}
	}
}

func TestDuplicateHookKeysAreRefused(t *testing.T) {
	repo := t.TempDir()
	err := loadErr(t, `{"version": 2, "projects": [
	  {"source": `+sourceObject("youtrack", lockKeyA, lockTokenA)+`, "project": "A", "repo": {"path": "`+repo+`"}},
	  {"source": `+sourceObject("youtrack", lockKeyA, lockTokenB)+`, "project": "B", "repo": {"path": "`+repo+`"}}
	]}`)
	if !strings.Contains(err, "same hook_key") {
		t.Fatalf("error = %q, want the duplicate key named", err)
	}
	if strings.Contains(err, lockKeyA) {
		t.Fatalf("error leaks the key value: %q", err)
	}
}

func TestHookKeyMustBeUsableInAURL(t *testing.T) {
	repo := t.TempDir()
	for name, key := range map[string]string{
		"too short": "abc123",
		"comma":     lockKeyA[:31] + ",",
		"slash":     lockKeyA[:31] + "/",
		"space":     lockKeyA[:31] + " ",
	} {
		t.Run(name, func(t *testing.T) {
			err := loadErr(t, `{"version": 2, "projects": [
			  {"source": `+sourceObject("youtrack", key, lockTokenA)+`, "project": "A", "repo": {"path": "`+repo+`"}}
			]}`)
			if !strings.Contains(err, "source.hook_key") {
				t.Fatalf("error = %q, want it to name source.hook_key", err)
			}
			if strings.Contains(err, key) {
				t.Fatalf("error leaks the key value: %q", err)
			}
		})
	}
}

func TestTokenShapeIsChecked(t *testing.T) {
	repo := t.TempDir()
	for name, token := range map[string]string{
		"too short":      "abc123",
		"trailing space": lockTokenA[:63] + " ",
		"newline":        lockTokenA[:62] + `\n`,
	} {
		t.Run(name, func(t *testing.T) {
			err := loadErr(t, `{"version": 2, "projects": [
			  {"source": `+sourceObject("youtrack", lockKeyA, token)+`, "project": "A", "repo": {"path": "`+repo+`"}}
			]}`)
			if !strings.Contains(err, "source.token") {
				t.Fatalf("error = %q, want it to name source.token", err)
			}
		})
	}
}

// The object is decoded strictly inside its own UnmarshalJSON; without that a typo
// would drop a lock silently instead of refusing the file.
func TestSourceObjectRejectsUnknownFields(t *testing.T) {
	repo := t.TempDir()
	err := loadErr(t, `{"version": 2, "projects": [
	  {"source": {"name": "youtrack", "hook_key": "`+lockKeyA+`", "token": "`+lockTokenA+`", "hook_keyy": "x"}, "project": "A", "repo": {"path": "`+repo+`"}}
	]}`)
	if !strings.Contains(err, "hook_keyy") {
		t.Fatalf("error = %q, want the unknown field named", err)
	}
}

// The report is pasted into tickets; it may show that a lock exists, never its
// value.
func TestReportShowsLocksWithoutValues(t *testing.T) {
	repo := t.TempDir()
	m := writeConfig(t, `{"version": 2, "projects": [
	  {"source": `+sourceObject("youtrack", lockKeyA, lockTokenA)+`, "project": "A", "repo": {"path": "`+repo+`"}}
	]}`)
	report := m.Report()
	if !strings.Contains(report, "locks url_key,header_token") {
		t.Fatalf("report does not show the locks:\n%s", report)
	}
	if strings.Contains(report, lockKeyA) || strings.Contains(report, lockTokenA) {
		t.Fatalf("report leaks a lock value:\n%s", report)
	}
}

func TestHasInlineLockMaterialTracksTheSourceObject(t *testing.T) {
	repo := t.TempDir()
	inline := writeConfig(t, `{"version": 2, "projects": [
	  {"source": `+sourceObject("youtrack", lockKeyA, lockTokenA)+`, "project": "A", "repo": {"path": "`+repo+`"}}
	]}`)
	if !inline.HasInlineLockMaterial() {
		t.Fatal("a file carrying an inline lock must be treated as a secret")
	}

	stringForm := writeConfig(t, `{"version": 2, "projects": [
	  {"source": "youtrack", "project": "A", "repo": {"path": "`+repo+`"}}
	]}`)
	if stringForm.HasInlineLockMaterial() {
		t.Fatal("a file naming only the source carries no secret")
	}
}

// The environment form is what keeps the routing table free of secrets: the value
// lives in the environment (or the environment file, which is applied before the
// configuration is loaded) and the entry only names the variable.
func TestEntryLocksFromEnvironment(t *testing.T) {
	const (
		keyVar   = "FLOWHUB_TEST_HOOK_KEY"
		tokenVar = "FLOWHUB_TEST_TOKEN"
	)
	t.Setenv(keyVar, lockKeyA)
	t.Setenv(tokenVar, lockTokenA)

	repo := t.TempDir()
	m := writeConfig(t, `{"version": 2, "projects": [
	  {"source": {"name": "youtrack", "hook_key_env": "`+keyVar+`", "token_env": "`+tokenVar+`"},
	   "project": "A", "repo": {"path": "`+repo+`"}}
	]}`)

	locks := m.EntryLocks("youtrack")
	if len(locks) != 1 || locks[0].Key != lockKeyA || locks[0].Token != lockTokenA {
		t.Fatalf("EntryLocks = %+v, want the values read from the environment", locks)
	}
	if m.HasInlineLockMaterial() {
		t.Fatal("a table that only names variables is not itself a secret")
	}
	report := m.Report()
	if !strings.Contains(report, "env "+keyVar) || !strings.Contains(report, "env "+tokenVar) {
		t.Fatalf("report does not show the environment provenance:\n%s", report)
	}
	if strings.Contains(report, lockKeyA) || strings.Contains(report, lockTokenA) {
		t.Fatalf("report leaks a lock value:\n%s", report)
	}
}

// A disabled entry is not addressable: an unset variable it names must not keep
// the receiver from starting, because the operator has already turned the entry off.
func TestDisabledEntryDoesNotResolveItsLocks(t *testing.T) {
	repo := t.TempDir()
	m := writeConfig(t, `{"version": 2, "projects": [
	  {"source": {"name": "youtrack", "hook_key_env": "FLOWHUB_UNUSED_HOOK_KEY", "token_env": "FLOWHUB_UNUSED_TOKEN"},
	   "project": "OFF", "repo": {"path": "`+repo+`"}, "enabled": false}
	]}`)
	if locks := m.EntryLocks("youtrack"); len(locks) != 0 {
		t.Fatalf("EntryLocks = %+v, want none for a disabled entry", locks)
	}
	if problems := m.LockProblems("youtrack"); len(problems) != 0 {
		t.Fatalf("LockProblems = %v, want none for a disabled entry", problems)
	}
}

func TestUnsetLockVariableRefusesTheLoad(t *testing.T) {
	const keyVar = "FLOWHUB_TEST_HOOK_KEY_UNSET"
	repo := t.TempDir()
	err := loadErr(t, `{"version": 2, "projects": [
	  {"source": {"name": "youtrack", "hook_key_env": "`+keyVar+`", "token": "`+lockTokenA+`"},
	   "project": "A", "repo": {"path": "`+repo+`"}}
	]}`)
	if !strings.Contains(err, keyVar) || !strings.Contains(err, "unset or empty") {
		t.Fatalf("error = %q, want it to name %s and say it is unset", err, keyVar)
	}
}

// A resolved value is held to the same shape rules as an inline one: an environment
// variable cannot smuggle in a weak key.
func TestEnvironmentLockIsValidatedLikeAnInlineOne(t *testing.T) {
	const keyVar = "FLOWHUB_TEST_SHORT_HOOK_KEY"
	t.Setenv(keyVar, "short")
	repo := t.TempDir()
	err := loadErr(t, `{"version": 2, "projects": [
	  {"source": {"name": "youtrack", "hook_key_env": "`+keyVar+`", "token": "`+lockTokenA+`"},
	   "project": "A", "repo": {"path": "`+repo+`"}}
	]}`)
	if !strings.Contains(err, "source.hook_key") || !strings.Contains(err, "32 character floor") {
		t.Fatalf("error = %q, want the key floor named", err)
	}
}

func TestTwoLockSourcesForOneFieldAreRefused(t *testing.T) {
	t.Setenv("FLOWHUB_TEST_HOOK_KEY_BOTH", lockKeyB)
	repo := t.TempDir()
	err := loadErr(t, `{"version": 2, "projects": [
	  {"source": {"name": "youtrack", "hook_key": "`+lockKeyA+`", "hook_key_env": "FLOWHUB_TEST_HOOK_KEY_BOTH", "token": "`+lockTokenA+`"},
	   "project": "A", "repo": {"path": "`+repo+`"}}
	]}`)
	if !strings.Contains(err, "more than one source") {
		t.Fatalf("error = %q, want the ambiguity refused", err)
	}
}

func TestEntryLocksFromAFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "hook.key")
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(keyFile, []byte(lockKeyA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(lockTokenA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	body := `{"version": 2, "projects": [
	  {"source": {"name": "youtrack", "hook_key_file": "` + keyFile + `", "token_file": "` + tokenFile + `"},
	   "project": "A", "repo": {"path": "` + repo + `"}}
	]}`
	m := writeConfig(t, body)
	locks := m.EntryLocks("youtrack")
	if len(locks) != 1 || locks[0].Key != lockKeyA || locks[0].Token != lockTokenA {
		t.Fatalf("EntryLocks = %+v, want the values read from the files", locks)
	}
	if !strings.Contains(m.Report(), "file "+keyFile) {
		t.Fatalf("report does not show the file provenance:\n%s", m.Report())
	}

	// A secret file the group or the world can read is refused, like every other
	// secret file in this project.
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadErr(t, body); !strings.Contains(err, "chmod 600") {
		t.Fatalf("error = %q, want the permission fix named", err)
	}
}
