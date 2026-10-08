package projectmap

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// credentialFixture is a configuration directory with a repository-shaped entry,
// so a credentials test only has to describe what it is about.
type credentialFixture struct {
	dir string
}

func newCredentialFixture(t *testing.T) credentialFixture {
	t.Helper()
	return credentialFixture{dir: t.TempDir()}
}

// write puts a file in the fixture directory, with an explicit mode: the permission
// cases need a mode that is deliberately wrong.
func (f credentialFixture) write(t *testing.T, name, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile applies the umask, so the mode is set again explicitly: a test that
	// meant 0644 must not pass because the developer's umask made it 0600.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// config writes the routing table and loads it.
func (f credentialFixture) config(t *testing.T, body string) *Map {
	t.Helper()
	path := f.write(t, "config.json", body, 0o600)
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m
}

// env returns a getenv that answers only the named variables.
func env(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

// hasProblem reports whether any problem contains the given substring. Several
// cases produce more than one problem (the credentials file and the runtime it leaves
// unpaired), so the assertions name what they require rather than counting.
func hasProblem(problems []string, want string) bool {
	for _, problem := range problems {
		if strings.Contains(problem, want) {
			return true
		}
	}
	return false
}

// projectSuffix is the part of the body every fixture needs.
const projectSuffix = `,
	  "projects": [{"source": "youtrack", "project": "TEST", "repo": {"path": "/tmp/test"}, "runtime": "local"}]}`

// TestCredentialSourcesResolveInPriorityOrder is the acceptance test for the four
// sources: the runtime's own block wins over the shared file, the shared file wins
// over the environment pair, and the environment pair still answers for a runtime the
// file never declares. A runtime-specific declaration that lost to a process-wide
// default would be the silent override this project refuses everywhere else.
func TestCredentialSourcesResolveInPriorityOrder(t *testing.T) {
	f := newCredentialFixture(t)
	f.write(t, "credentials.json", `{"runtimes": {
	  "enrolled": {"user": "file-user", "password": "from-credentials-file"}
	}}`, 0o600)

	m := f.config(t, `{"version": 2,
	  "credentials_file": "credentials.json",
	  "runtimes": {
	    "local":    {"url": "http://h:1", "auth": {"user": "block-user", "password": "inline"}},
	    "envp":     {"url": "http://h:2", "auth": {"user": "block-user", "password_env": "ENVP_PASSWORD"}},
	    "envonly":  {"url": "http://h:3"},
	    "enrolled": {"url": "http://h:4", "auth": {"user": "block-user"}},
	    "fileonly": {"url": "http://h:5", "auth": {"user": "block-user", "password_file": "envp.pass"}}
	  }`+projectSuffix)

	f.write(t, "envp.pass", "from-password-file\n", 0o600)
	getenv := env(map[string]string{
		"ENVP_PASSWORD":             "from-env-per-runtime",
		"FLOWHUB_OPENCODE_USER":     "global-user",
		"FLOWHUB_OPENCODE_PASSWORD": "from-global-env",
	})

	cases := []struct {
		name       string
		runtime    string
		wantUser   string
		wantPass   string
		wantSource string
	}{
		{"an inline block wins over the shared file", "local", "block-user", "inline", "password"},
		{"a per-runtime variable wins over the shared file", "envp", "block-user", "from-env-per-runtime", "password_env:ENVP_PASSWORD"},
		{"a per-runtime file wins over the shared file", "fileonly", "block-user", "from-password-file", "password_file:" + filepath.Join(f.dir, "envp.pass")},
		{"the shared file answers a bare auth block", "enrolled", "block-user", "from-credentials-file", "credentials_file:" + filepath.Join(f.dir, "credentials.json") + "#enrolled"},
		{"the environment pair answers when nothing else does", "envonly", "global-user", "from-global-env", "FLOWHUB_OPENCODE_PASSWORD"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			credential, ok := m.ResolveCredential(testCase.runtime, getenv)
			if !ok {
				t.Fatalf("ResolveCredential(%q) = no credential, want one", testCase.runtime)
			}
			if credential.User != testCase.wantUser || credential.Password != testCase.wantPass {
				t.Fatalf("ResolveCredential(%q) = %q:%q, want %q:%q",
					testCase.runtime, credential.User, credential.Password, testCase.wantUser, testCase.wantPass)
			}
			if credential.Source != testCase.wantSource {
				t.Fatalf("source = %q, want %q", credential.Source, testCase.wantSource)
			}
		})
	}

	// The shared file may name a runtime the configuration does not declare only if
	// the operator meant it — that is a typo detector, and it must fire.
	if problems := m.RuntimeCredentialProblems(getenv); len(problems) != 0 {
		t.Fatalf("RuntimeCredentialProblems = %v, want none", problems)
	}
}

// TestCredentialsFileCanNameAnUndeclaredRuntime is the typo detector: an entry for a
// runtime the configuration never declares silently disables a credential, so it is
// reported instead.
func TestCredentialsFileCanNameAnUndeclaredRuntime(t *testing.T) {
	f := newCredentialFixture(t)
	f.write(t, "credentials.json", `{"runtimes": {"locl": {"user": "u", "password": "p"}}}`, 0o600)
	m := f.config(t, `{"version": 2, "credentials_file": "credentials.json"`+projectSuffix)

	problems := m.RuntimeCredentialProblems(env(nil))
	if len(problems) != 1 || !strings.Contains(problems[0], "locl") {
		t.Fatalf("RuntimeCredentialProblems = %v, want one problem naming locl", problems)
	}
}

// TestCredentialRefusalsAreFailClosed covers every way the credentials surface can be
// configured and still leave a runtime unable to authenticate. Each one refuses the
// start rather than resolving to an empty password, because an empty password looks
// exactly like a wrong one, at the first turn instead of at startup.
func TestCredentialRefusalsAreFailClosed(t *testing.T) {
	newCase := func(t *testing.T, setup func(f credentialFixture), body string) *Map {
		t.Helper()
		f := newCredentialFixture(t)
		setup(f)
		return f.config(t, body)
	}

	t.Run("a missing credentials file", func(t *testing.T) {
		m := newCase(t, func(credentialFixture) {},
			`{"version": 2, "credentials_file": "absent.json"`+projectSuffix)
		problems := m.RuntimeCredentialProblems(env(nil))
		if len(problems) != 1 || !strings.Contains(problems[0], "absent.json") {
			t.Fatalf("RuntimeCredentialProblems = %v, want one problem naming the missing file", problems)
		}
	})

	t.Run("an empty credentials file", func(t *testing.T) {
		m := newCase(t, func(f credentialFixture) { f.write(t, "credentials.json", `{"runtimes": {}}`, 0o600) },
			`{"version": 2, "credentials_file": "credentials.json"`+projectSuffix)
		problems := m.RuntimeCredentialProblems(env(nil))
		if len(problems) != 1 || !strings.Contains(problems[0], "declares no runtimes") {
			t.Fatalf("RuntimeCredentialProblems = %v, want the empty-file refusal", problems)
		}
	})

	t.Run("an unparseable credentials file", func(t *testing.T) {
		m := newCase(t, func(f credentialFixture) { f.write(t, "credentials.json", `{"runtimes":`, 0o600) },
			`{"version": 2, "credentials_file": "credentials.json"`+projectSuffix)
		problems := m.RuntimeCredentialProblems(env(nil))
		if len(problems) != 1 || !strings.Contains(problems[0], "credentials_file") {
			t.Fatalf("RuntimeCredentialProblems = %v, want the parse refusal", problems)
		}
	})

	t.Run("an unknown field inside the credentials file", func(t *testing.T) {
		m := newCase(t, func(f credentialFixture) {
			f.write(t, "credentials.json", `{"runtimes": {"local": {"user": "u", "password": "p", "tokn": "x"}}}`, 0o600)
		}, `{"version": 2, "credentials_file": "credentials.json"`+projectSuffix)
		problems := m.RuntimeCredentialProblems(env(nil))
		if len(problems) != 1 || !strings.Contains(problems[0], "tokn") {
			t.Fatalf("RuntimeCredentialProblems = %v, want an unknown-field refusal naming tokn", problems)
		}
	})

	t.Run("an entry with no password", func(t *testing.T) {
		m := newCase(t, func(f credentialFixture) {
			f.write(t, "credentials.json", `{"runtimes": {"local": {"user": "u"}}}`, 0o600)
		}, `{"version": 2, "credentials_file": "credentials.json"`+projectSuffix)
		problems := m.RuntimeCredentialProblems(env(nil))
		if len(problems) != 1 || !strings.Contains(problems[0], "empty password") {
			t.Fatalf("RuntimeCredentialProblems = %v, want an empty-password refusal", problems)
		}
	})

	t.Run("an entry with no user", func(t *testing.T) {
		m := newCase(t, func(f credentialFixture) {
			f.write(t, "credentials.json", `{"runtimes": {"local": {"password": "p"}}}`, 0o600)
		}, `{"version": 2, "credentials_file": "credentials.json"`+projectSuffix)
		problems := m.RuntimeCredentialProblems(env(nil))
		if !hasProblem(problems, "no user") {
			t.Fatalf("RuntimeCredentialProblems = %v, want a no-user refusal", problems)
		}
	})

	t.Run("an env placeholder with no variable", func(t *testing.T) {
		m := newCase(t, func(f credentialFixture) {
			f.write(t, "credentials.json", `{"runtimes": {"local": {"user": "u", "password": "{env:MISSING}"}}}`, 0o600)
		}, `{"version": 2, "credentials_file": "credentials.json"`+projectSuffix)
		problems := m.RuntimeCredentialProblems(env(nil))
		if !hasProblem(problems, "MISSING") {
			t.Fatalf("RuntimeCredentialProblems = %v, want a refusal naming MISSING", problems)
		}
	})

	t.Run("an empty environment variable", func(t *testing.T) {
		m := newCase(t, func(credentialFixture) {},
			`{"version": 2, "runtimes": {"local": {"url": "http://h:1", "auth": {"user": "u", "password_env": "EMPTY_VAR"}}}`+projectSuffix)
		// The variable is exported but empty, so there is no password to use. The
		// refusal describes the half pair rather than the variable name: the name is
		// already in the configuration the operator is looking at.
		problems := m.RuntimeCredentialProblems(env(map[string]string{"EMPTY_VAR": "   "}))
		if !hasProblem(problems, "no password could be resolved") {
			t.Fatalf("RuntimeCredentialProblems = %v, want the half-pair refusal", problems)
		}
		if credential, ok := m.ResolveCredential("local", env(map[string]string{"EMPTY_VAR": "   "})); ok {
			t.Fatalf("an empty variable resolved to %+v", credential)
		}
	})

	t.Run("a missing password file", func(t *testing.T) {
		m := newCase(t, func(credentialFixture) {},
			`{"version": 2, "runtimes": {"local": {"url": "http://h:1", "auth": {"user": "u", "password_file": "absent.pass"}}}`+projectSuffix)
		// The contract is checked through the resolved credential, so a runtime whose
		// file cannot be read has no credential to resolve.
		if credential, ok := m.ResolveCredential("local", env(nil)); ok {
			t.Fatalf("a missing password file resolved to %+v", credential)
		}
	})

	t.Run("an empty password file", func(t *testing.T) {
		m := newCase(t, func(f credentialFixture) { f.write(t, "empty.pass", "\n \n", 0o600) },
			`{"version": 2, "runtimes": {"local": {"url": "http://h:1", "auth": {"user": "u", "password_file": "empty.pass"}}}`+projectSuffix)
		if credential, ok := m.ResolveCredential("local", env(nil)); ok {
			t.Fatalf("an empty password file resolved to %+v", credential)
		}
	})

	t.Run("a user with no source at all", func(t *testing.T) {
		m := newCase(t, func(credentialFixture) {},
			`{"version": 2, "runtimes": {"local": {"url": "http://h:1", "auth": {"user": "u"}}}`+projectSuffix)
		problems := m.RuntimeCredentialProblems(env(nil))
		if len(problems) != 1 || !strings.Contains(problems[0], "no password could be resolved") {
			t.Fatalf("RuntimeCredentialProblems = %v, want the half-pair refusal", problems)
		}
	})
}

// TestTwoPasswordSourcesAreRefusedAtLoad is the mutual-exclusion rule: naming two
// sources is the mistake that survives every later reading, because the operator
// changes the one that is not in force.
func TestTwoPasswordSourcesAreRefusedAtLoad(t *testing.T) {
	for name, auth := range map[string]string{
		"inline and a variable": `{"user": "u", "password": "p", "password_env": "X"}`,
		"inline and a file":     `{"user": "u", "password": "p", "password_file": "p.pass"}`,
		"a variable and a file": `{"user": "u", "password_env": "X", "password_file": "p.pass"}`,
		"all three":             `{"user": "u", "password": "p", "password_env": "X", "password_file": "p.pass"}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := loadErr(t, `{"version": 2, "runtimes": {"builder-a": {"url": "http://h:1", "auth": `+auth+`}},
			  "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "/tmp/x"}, "runtime": "builder-a"}]}`)
			if !strings.Contains(err, "password sources") {
				t.Fatalf("refusal = %s, want it to name the password sources", err)
			}
		})
	}
}

// TestAWorldReadableSecretIsRefused is the permission gate, and it is mutation-checked
// in the PR: making secretPermissionProblem return nil unconditionally turns the
// 0644 cases below green and the suite red.
func TestAWorldReadableSecretIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}

	t.Run("a credentials file", func(t *testing.T) {
		f := newCredentialFixture(t)
		f.write(t, "credentials.json", `{"runtimes": {"local": {"user": "u", "password": "p"}}}`, 0o600)
		// The runtime is declared so the only problem this case can produce is the
		// permission one.
		body := `{"version": 2, "credentials_file": "credentials.json",
		  "runtimes": {"local": {"url": "http://h:1", "auth": {"user": "u"}}}` + projectSuffix

		if problems := f.config(t, body).RuntimeCredentialProblems(env(nil)); len(problems) != 0 {
			t.Fatalf("a 0600 credentials file was refused: %v", problems)
		}

		f.write(t, "credentials.json", `{"runtimes": {"local": {"user": "u", "password": "p"}}}`, 0o644)
		problems := f.config(t, body).RuntimeCredentialProblems(env(nil))
		if !hasProblem(problems, "chmod 600") {
			t.Fatalf("a 0644 credentials file was accepted: %v", problems)
		}
	})

	t.Run("a password file", func(t *testing.T) {
		f := newCredentialFixture(t)
		f.write(t, "local.pass", "secret\n", 0o640)
		m := f.config(t, `{"version": 2, "runtimes": {"local": {"url": "http://h:1", "auth": {"user": "u", "password_file": "local.pass"}}}`+projectSuffix)
		if credential, ok := m.ResolveCredential("local", env(nil)); ok {
			t.Fatalf("a group-readable password file resolved to %+v", credential)
		}

		f.write(t, "local.pass", "secret\n", 0o600)
		credential, ok := f.config(t, `{"version": 2, "runtimes": {"local": {"url": "http://h:1", "auth": {"user": "u", "password_file": "local.pass"}}}`+projectSuffix).ResolveCredential("local", env(nil))
		if !ok || credential.Password != "secret" {
			t.Fatalf("a 0600 password file did not resolve: %+v (ok=%v)", credential, ok)
		}
	})

	t.Run("a directory instead of a file", func(t *testing.T) {
		f := newCredentialFixture(t)
		if err := os.Mkdir(filepath.Join(f.dir, "adir"), 0o700); err != nil {
			t.Fatal(err)
		}
		m := f.config(t, `{"version": 2, "runtimes": {"local": {"url": "http://h:1", "auth": {"user": "u", "password_file": "adir"}}}`+projectSuffix)
		if credential, ok := m.ResolveCredential("local", env(nil)); ok {
			t.Fatalf("a directory resolved as a password file: %+v", credential)
		}
	})
}

// TestThePasswordNeverReachesTheReport is the containment assertion: the report names
// the source of a password and never the password itself, for every source there is.
func TestThePasswordNeverReachesTheReport(t *testing.T) {
	f := newCredentialFixture(t)
	f.write(t, "credentials.json", `{"runtimes": {"shared": {"user": "u", "password": "PASSWORD-FROM-CREDENTIALS-FILE"}}}`, 0o600)
	f.write(t, "local.pass", "PASSWORD-FROM-PASSWORD-FILE\n", 0o600)
	m := f.config(t, `{"version": 2,
	  "credentials_file": "credentials.json",
	  "runtimes": {
	    "inline":  {"url": "http://h:1", "auth": {"user": "u", "password": "PASSWORD-INLINE"}},
	    "fromenv": {"url": "http://h:2", "auth": {"user": "u", "password_env": "REPORT_VAR"}},
	    "fromfile":{"url": "http://h:3", "auth": {"user": "u", "password_file": "local.pass"}},
	    "shared":  {"url": "http://h:4", "auth": {"user": "u"}}
	  }`+projectSuffix)

	// t.Setenv makes the report's own environment lookup deterministic.
	t.Setenv("REPORT_VAR", "PASSWORD-FROM-ENV")

	var rendered strings.Builder
	for _, credential := range m.CredentialReport() {
		rendered.WriteString(credential.Scope + " " + credential.Field + "=" + credential.Value + "\n")
	}
	for _, secret := range []string{
		"PASSWORD-INLINE",
		"PASSWORD-FROM-ENV",
		"PASSWORD-FROM-PASSWORD-FILE",
		"PASSWORD-FROM-CREDENTIALS-FILE",
	} {
		if strings.Contains(rendered.String(), secret) {
			t.Fatalf("the report contains %q:\n%s", secret, rendered.String())
		}
	}
	// The control: the report is not empty and does name the sources.
	for _, want := range []string{"password_env:REPORT_VAR", "password_file:", "credentials_file:"} {
		if !strings.Contains(rendered.String(), want) {
			t.Fatalf("the report does not name %q:\n%s", want, rendered.String())
		}
	}
}

// TestCredentialsPathResolution pins how a path in the configuration file is read: a
// relative path belongs to the configuration file's directory (not the process
// working directory, which would resolve differently in a service unit), and `~` is
// expanded because no shell does it for a value read from a file.
func TestCredentialsPathResolution(t *testing.T) {
	f := newCredentialFixture(t)
	f.write(t, "credentials.json", `{"runtimes": {"local": {"user": "u", "password": "p"}}}`, 0o600)

	relative := f.config(t, `{"version": 2, "credentials_file": "credentials.json"`+projectSuffix)
	if got := relative.credentialsPath; got != filepath.Join(f.dir, "credentials.json") {
		t.Fatalf("relative credentials_file resolved to %q, want it beside the config file", got)
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory to expand")
	}
	absolute := f.config(t, `{"version": 2, "credentials_file": "~/flowhub-absent-credentials.json"`+projectSuffix)
	want := filepath.Join(home, "flowhub-absent-credentials.json")
	if got := absolute.credentialsPath; got != want {
		t.Fatalf("~ expanded to %q, want %q", got, want)
	}
}

// TestExpandSecretValue pins the placeholder rules directly, because they are the one
// place where an empty value must not be mistaken for "no password configured".
func TestExpandSecretValue(t *testing.T) {
	t.Setenv("PRESENT_VAR", "value")
	t.Setenv("BLANK_VAR", "   ")

	cases := map[string]struct {
		in      string
		want    string
		wantErr string
	}{
		"a literal is kept":         {in: "hunter2", want: "hunter2"},
		"a placeholder resolves":    {in: "{env:PRESENT_VAR}", want: "value"},
		"surrounding space is kept": {in: " hunter2 ", want: " hunter2 "},
		"a missing variable":        {in: "{env:ABSENT_VAR}", wantErr: "ABSENT_VAR"},
		"a blank variable":          {in: "{env:BLANK_VAR}", wantErr: "BLANK_VAR"},
		"a nameless placeholder":    {in: "{env:}", wantErr: "names no variable"},
		"something else is literal": {in: "{env:PRESENT_VAR", want: "{env:PRESENT_VAR"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := expandSecretValue(testCase.in)
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("expandSecretValue(%q) error = %v, want one containing %q", testCase.in, err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expandSecretValue(%q): %v", testCase.in, err)
			}
			if got != testCase.want {
				t.Fatalf("expandSecretValue(%q) = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}
}
