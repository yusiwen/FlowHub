package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeEnvFile writes a file with an explicit mode: two cases here are about
// permissions, and the process umask must not decide whether they pass.
func writeEnvFile(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestParseEnvFileReadsTheSupportedSubset pins the syntax. Everything not listed here
// is refused on purpose: a parser with surprises is how a variable silently becomes
// something the operator did not write.
func TestParseEnvFileReadsTheSupportedSubset(t *testing.T) {
	pairs, err := parseEnvFile([]byte(`
# a comment
FLOWHUB_ADDR=127.0.0.1:8080

export FLOWHUB_DISPATCH=1
FLOWHUB_TOKEN="quoted value"
FLOWHUB_HOOK_KEY='single quoted'
FLOWHUB_SPACED=value with spaces
FLOWHUB_EMPTY=
   FLOWHUB_INDENTED=1
FLOWHUB_HASH=value#notacomment
`))
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}

	got := map[string]string{}
	for _, pair := range pairs {
		got[pair.Name] = pair.Value
	}
	want := map[string]string{
		"FLOWHUB_ADDR":     "127.0.0.1:8080",
		"FLOWHUB_DISPATCH": "1",
		"FLOWHUB_TOKEN":    "quoted value",
		"FLOWHUB_HOOK_KEY": "single quoted",
		"FLOWHUB_SPACED":   "value with spaces",
		"FLOWHUB_EMPTY":    "",
		"FLOWHUB_INDENTED": "1",
		// A `#` that is not at the start of a line is part of the value: this is a file,
		// not a shell, and the token may contain one.
		"FLOWHUB_HASH": "value#notacomment",
	}
	for name, value := range want {
		actual, ok := got[name]
		if !ok {
			t.Errorf("%s was not parsed", name)
			continue
		}
		if actual != value {
			t.Errorf("%s = %q, want %q", name, actual, value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d variables, want %d: %v", len(got), len(want), got)
	}
}

// TestParseEnvFileRefusesWhatItCannotHonour: a line it does not understand, a name it
// cannot set, and a duplicate all refuse with the line number, because a value that is
// silently dropped or silently overwritten is the failure this project refuses.
func TestParseEnvFileRefusesWhatItCannotHonour(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"a line with no assignment":    {body: "FLOWHUB_ADDR\n", want: "line 1"},
		"a name that is not a name":    {body: "FLOWHUB-ADDR=x\n", want: "line 1"},
		"a name starting with a digit": {body: "1FLOWHUB=x\n", want: "line 1"},
		"a duplicate key":              {body: "A=1\nA=2\n", want: "line 2"},
		"a duplicate after a comment":  {body: "# c\nA=1\n\nA=2\n", want: "line 4"},
		"a bare export":                {body: "export =1\n", want: "line 1"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseEnvFile([]byte(testCase.body))
			if err == nil {
				t.Fatalf("parseEnvFile accepted %q", testCase.body)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %q, want it to name %q", err, testCase.want)
			}
		})
	}
}

// TestApplyEnvFileLetsTheRealEnvironmentWin is the precedence decision: the file
// supplies defaults, and a variable the operator exported is never replaced. The
// direction matters because the opposite would let a forgotten file change a value the
// command line stated explicitly.
func TestApplyEnvFileLetsTheRealEnvironmentWin(t *testing.T) {
	// DSHENV_FROM_FILE is not set at all, which is the case the file has to fill.
	t.Setenv("DSHENV_ALREADY_SET", "from-the-environment")

	path := writeEnvFile(t, filepath.Join(t.TempDir(), ".env"), `
DSHENV_FROM_FILE=from-the-file
DSHENV_ALREADY_SET=from-the-file
DSHENV_EMPTY_IN_ENV=x
`, 0o600)
	t.Setenv("DSHENV_EMPTY_IN_ENV", "")

	result, err := ApplyEnvFile(EnvFileOptions{Path: path})
	if err != nil {
		t.Fatalf("ApplyEnvFile: %v", err)
	}
	if !result.Found || result.Path != path {
		t.Fatalf("result = %+v, want the file reported as found", result)
	}
	if got := os.Getenv("DSHENV_FROM_FILE"); got != "from-the-file" {
		t.Fatalf("a variable only in the file = %q, want it applied", got)
	}
	if got := os.Getenv("DSHENV_ALREADY_SET"); got != "from-the-environment" {
		t.Fatalf("a variable set in the environment = %q, want the environment's value", got)
	}
	// Exported-but-empty counts as set: the operator who wrote `FLOWHUB_X=` meant it,
	// and the program's own validation is where half a value is refused.
	if got := os.Getenv("DSHENV_EMPTY_IN_ENV"); got != "" {
		t.Fatalf("an exported-empty variable = %q, want it left alone", got)
	}
	if !equalStrings(result.Applied, []string{"DSHENV_FROM_FILE"}) {
		t.Errorf("Applied = %v, want only the file-only variable", result.Applied)
	}
	if !equalStrings(result.Skipped, []string{"DSHENV_ALREADY_SET", "DSHENV_EMPTY_IN_ENV"}) {
		t.Errorf("Skipped = %v, want the two the environment already had", result.Skipped)
	}
}

// TestApplyEnvFileMissingFileIsOnlyFatalWhenNamed: a host without a file is the normal
// case and must start; a path the operator named and that does not exist is a mistake.
func TestApplyEnvFileMissingFileIsOnlyFatalWhenNamed(t *testing.T) {
	t.Setenv(EnvFileVariable, "")
	absent := filepath.Join(t.TempDir(), "absent.env")

	// The default path is not configurable per test, so the "default is fine" case is
	// expressed through the variable: pointing it at an absent file is the named case,
	// and clearing it falls back to the real default, which this test cannot assert on.
	t.Setenv(EnvFileVariable, absent)
	if _, err := ApplyEnvFile(EnvFileOptions{}); err == nil {
		t.Fatal("an absent file named through the environment was accepted")
	} else if !strings.Contains(err.Error(), EnvFileVariable) {
		t.Fatalf("error = %q, want it to name %s", err, EnvFileVariable)
	}

	if _, err := ApplyEnvFile(EnvFileOptions{Path: absent}); err == nil {
		t.Fatal("an absent file named by the caller was accepted")
	}

	// The default path, absent: not an error. FLOWHUB_ENV_FILE is cleared, and the
	// default is asserted to be a path that does not exist on a test runner.
	t.Setenv(EnvFileVariable, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	result, err := ApplyEnvFile(EnvFileOptions{})
	if err != nil {
		t.Fatalf("an absent default file was an error: %v", err)
	}
	if result.Found {
		t.Fatalf("result = %+v, want nothing found", result)
	}
	if want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "flowhub", EnvFileName); result.Path != want {
		t.Fatalf("default path = %q, want %q", result.Path, want)
	}
}

// TestApplyEnvFileCanBeDisabled: a deployment that configures through the environment
// has to be able to say so, and the switch must not be confusable with a path.
func TestApplyEnvFileCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	// A file that would fail loudly if it were read: a permissive mode.
	writeEnvFile(t, filepath.Join(dir, ".env"), "DSHENV_SHOULD_NOT_BE_SET=1\n", 0o644)
	t.Setenv("XDG_CONFIG_HOME", dir)

	for _, value := range []string{"-", "none", "off", "OFF"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(EnvFileVariable, value)
			result, err := ApplyEnvFile(EnvFileOptions{})
			if err != nil {
				t.Fatalf("disabled by %q: %v", value, err)
			}
			if result.Path != "" || result.Found {
				t.Fatalf("result = %+v, want the feature reported off", result)
			}
			if _, set := os.LookupEnv("DSHENV_SHOULD_NOT_BE_SET"); set {
				t.Fatal("a disabled environment file was still applied")
			}
		})
	}
}

// TestAWorldReadableEnvFileIsRefused is the permission floor, shared with the
// credentials file (ADR 0004): this file holds the URL key and the token. It is
// mutation-checked in the PR — making the predicate constant-true turns the 0644 case
// green locally and red here.
func TestAWorldReadableEnvFileIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()

	if _, err := ApplyEnvFile(EnvFileOptions{Path: writeEnvFile(t, filepath.Join(dir, "good.env"), "DSHENV_OK=1\n", 0o600)}); err != nil {
		t.Fatalf("a 0600 environment file was refused: %v", err)
	}
	_, err := ApplyEnvFile(EnvFileOptions{Path: writeEnvFile(t, filepath.Join(dir, "bad.env"), "DSHENV_NOPE=1\n", 0o644)})
	if err == nil {
		t.Fatal("a 0644 environment file was accepted")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("error = %q, want the chmod fix in it", err)
	}

	// A directory is refused as a directory, not reported as unreadable.
	if _, err := ApplyEnvFile(EnvFileOptions{Path: dir}); err == nil {
		t.Fatal("a directory was accepted as an environment file")
	}
}

// TestEnvFileSuppliesTheReceiverConfiguration is the integration the feature exists
// for: a value that is only in the file reaches config.Load, and a value the operator
// exported beats it there too.
func TestEnvFileSuppliesTheReceiverConfiguration(t *testing.T) {
	// The variables this test drives are cleared so the assertions are about the file.
	for _, name := range []string{"FLOWHUB_ADDR", "FLOWHUB_TRIGGER", "FLOWHUB_REPLAY_WINDOW"} {
		original, had := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if had {
				os.Setenv(name, original)
			} else {
				os.Unsetenv(name)
			}
		})
	}
	t.Setenv("FLOWHUB_TRIGGER", "/exported wins")

	path := writeEnvFile(t, filepath.Join(t.TempDir(), ".env"), `
FLOWHUB_ADDR=10.9.0.1:8080
FLOWHUB_TRIGGER=/from the file
FLOWHUB_REPLAY_WINDOW=0
`, 0o600)
	if _, err := ApplyEnvFile(EnvFileOptions{Path: path}); err != nil {
		t.Fatalf("ApplyEnvFile: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "10.9.0.1:8080" {
		t.Errorf("addr = %q, want the file's value", cfg.Addr)
	}
	if cfg.ReplayWindow != 0 {
		t.Errorf("replay window = %v, want the file's 0", cfg.ReplayWindow)
	}
	if cfg.Trigger != "/exported wins" {
		t.Errorf("trigger = %q, want the environment's value", cfg.Trigger)
	}
}

// equalStrings compares two slices, order included.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
