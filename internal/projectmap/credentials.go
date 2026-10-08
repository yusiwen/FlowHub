package projectmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The credentials surface answers one question for a runtime: which Basic Auth
// pair drives the calls to that agent server.
//
// Four sources are available, and a runtime may name at most one of them per
// level, because "which password is in force" must never be ambiguous:
//
//   - `auth.password`         inline in the configuration file;
//   - `auth.password_env`     the name of an environment variable (the spelling that
//     keeps the file free of secrets, and the only one that existed before);
//   - `auth.password_file`    a file holding nothing but the secret;
//   - a top-level `credentials_file` keyed by runtime name.
//
// The credentials file exists because a deployment with several hosts otherwise
// has no file-based way to hold the credentials, and a service unit ends up
// duplicating secrets in its environment. Giving that file a home does not relax
// the rule it protects: the *secret* is still kept out of the configuration file
// unless the operator explicitly chooses otherwise.
//
// Secrets are read once, when the table is loaded, and cached: rotating a password
// takes a restart, exactly as it does with an environment variable.
type credentialsFile struct {
	Runtimes map[string]credentialsEntry `json:"runtimes"`
}

// credentialsEntry is one runtime's pair inside the credentials file. There is no
// nested `auth` object: inside a file that holds nothing else, the nesting would be
// noise.
type credentialsEntry struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// storedCredential is a resolved pair together with where it was written, so a
// report can name the source without naming the secret.
type storedCredential struct {
	User     string
	Password string
	// Origin is the file the pair was read from, for messages only.
	Origin string
}

// Credential is the effective Basic Auth pair for one runtime, together with a
// description of where the password came from. The description never contains the
// secret: it is what `-print-config` prints and what a log line may carry.
type Credential struct {
	User     string
	Password string
	// Source names the level that supplied the password, e.g. `credentials_file:local`,
	// `password_env:BUILDER_PASSWORD`, `password_file:/etc/flowhub/a.pass`,
	// `password` or `FLOWHUB_OPENCODE_PASSWORD`. Empty means no source supplied one.
	Source string
}

// ErrNoCredential reports that no source supplied a password for a runtime that
// names a user. It is a sentinel so a caller can tell "half a pair" apart from "no
// auth configured at all".
var ErrNoCredential = errors.New("no password source supplied a credential")

// maxSecretFileBytes bounds a password file. A secret is one line; anything larger
// is a mistake (a keypair, a kubeconfig, a log) and reading it would hide that.
const maxSecretFileBytes = 8 << 10

// loadCredentials reads the credentials file named by `credentials_file`, if any.
//
// It returns the resolved pairs plus the problems a report should show. The two are
// returned separately rather than as one error so `-print-config` can still render
// the table while naming the problem, the same way the rest of the loader works.
// A problem here is always fatal at startup: a runtime whose password cannot be
// resolved fails every turn, late, instead of refusing the start.
func (m *Map) loadCredentials(path string) (map[string]storedCredential, string, []string) {
	resolved := strings.TrimSpace(path)
	if resolved == "" {
		return nil, "", nil
	}
	location := m.resolveRelativePath(resolved)
	if err := checkSecretPermissions("credentials_file", location); err != nil {
		return nil, location, []string{err.Error()}
	}
	raw, err := os.ReadFile(location)
	if err != nil {
		return nil, location, []string{fmt.Sprintf("credentials_file %s: %v", location, err)}
	}

	var parsed credentialsFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		return nil, location, []string{fmt.Sprintf("credentials_file %s: %v", location, err)}
	}
	if len(parsed.Runtimes) == 0 {
		return nil, location, []string{fmt.Sprintf(
			"credentials_file %s declares no runtimes; remove the field rather than pointing it at an empty file", location)}
	}

	problems := []string{}
	out := make(map[string]storedCredential, len(parsed.Runtimes))
	for _, name := range sortedKeys(parsed.Runtimes) {
		entry := parsed.Runtimes[name]
		trimmed := strings.TrimSpace(name)
		if trimmed != name || !runtimeNamePattern.MatchString(name) {
			problems = append(problems, fmt.Sprintf(
				"credentials_file %s: %q is not a runtime name (want %s)", location, name, runtimeNamePattern))
			continue
		}
		user := strings.TrimSpace(entry.User)
		if user == "" {
			problems = append(problems, fmt.Sprintf(
				"credentials_file %s: runtimes.%s has no user, so the pair cannot be used", location, name))
		}
		password, err := expandSecretValue(entry.Password)
		if err != nil {
			problems = append(problems, fmt.Sprintf("credentials_file %s: runtimes.%s: %v", location, name, err))
			continue
		}
		if strings.TrimSpace(password) == "" {
			problems = append(problems, fmt.Sprintf(
				"credentials_file %s: runtimes.%s has an empty password", location, name))
			continue
		}
		out[name] = storedCredential{User: user, Password: password, Origin: location}
	}
	return out, location, problems
}

// resolveRelativePath resolves a path the configuration file names: an absolute path
// is kept, `~` is expanded, and a relative path is taken against the configuration
// file's own directory — never the process's working directory, which would make the
// same file resolve differently in a service unit and in a shell.
func (m *Map) resolveRelativePath(path string) string {
	resolved := expandHome(strings.TrimSpace(path))
	if resolved == "" || filepath.IsAbs(resolved) {
		return resolved
	}
	if m == nil || m.path == "" {
		return resolved
	}
	return filepath.Join(filepath.Dir(m.path), resolved)
}

// expandHome expands a leading `~` or `~/`. Go does not do it for a value that came
// from a file or an environment variable.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// expandSecretValue resolves the `{env:NAME}` placeholder a credentials entry may
// use, so that the file can exist without holding the secret itself.
//
// An empty or unset variable is an error rather than an empty password: "it is
// configured but has no effect" is refused everywhere else in this project, and a
// silently empty password looks exactly like a wrong one.
func expandSecretValue(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "{env:") || !strings.HasSuffix(trimmed, "}") {
		return value, nil
	}
	name := strings.TrimSpace(trimmed[len("{env:") : len(trimmed)-1])
	if name == "" {
		return "", errors.New("`{env:}` names no variable")
	}
	resolved, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(resolved) == "" {
		return "", fmt.Errorf("`{env:%s}` is empty or unset on this host", name)
	}
	return resolved, nil
}

// CheckSecretPermissions is checkSecretPermissions for callers outside this package:
// `main` enforces the same rule on the configuration file when that file holds an
// inline password. Exported rather than duplicated so the message and the threshold
// cannot drift apart.
func CheckSecretPermissions(label, path string) error {
	return checkSecretPermissions(label, path)
}

// checkSecretPermissions refuses a file that group or other can read or write.
//
// A world-readable password file is worse than no password file: the operator
// believes the secret is contained while anything on the machine can read it. The
// repository already writes its own secrets 0600 (the runtime identity, the
// manifest), so refusing is consistent rather than surprising.
//
// The check reads the permission bits only. On macOS an ACL that *grants* access
// cannot be seen in them, so this is a floor, not a proof — the message says so.
func checkSecretPermissions(label, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %s: %v", label, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s %s is a directory, not a file", label, path)
	}
	return secretPermissionProblem(label, path, info.Mode())
}

// secretPermissionProblem is the platform-independent half of the check. Windows has
// no meaningful POSIX bits — everything reports 0666 or 0444 — so the mode check is
// skipped there rather than refusing a start that no `chmod` could fix.
func secretPermissionProblem(label, path string, mode fs.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if mode.Perm()&0o077 != 0 {
		return fmt.Errorf("%s %s is mode %04o: group and other must have no access to a secret; fix it with `chmod 600 %s`",
			label, path, mode.Perm(), path)
	}
	return nil
}

// RuntimeCredentialProblems reports the credential problems the file's own shape can
// see: a runtime naming two password sources, a runtime whose password resolves from
// nowhere, and a credentials file entry for a runtime the file does not declare.
//
// The environment is passed in rather than read here, because the environment is the
// outermost level and `main` owns it; this keeps the whole check testable without
// touching the process environment. It is deliberately separate from ResolveCredentials
// so that `-print-config` can report a problem while the resolution path stays free of
// error plumbing.
func (m *Map) RuntimeCredentialProblems(getenv func(string) string) []string {
	if m == nil {
		return nil
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	problems := append([]string(nil), m.credentialProblems...)

	declared := map[string]bool{}
	for _, name := range m.runtimeOrder {
		declared[name] = true
	}
	for _, name := range sortedKeys(m.credentials) {
		if !declared[name] {
			problems = append(problems, fmt.Sprintf(
				"credentials_file %s: runtimes.%s names a runtime the configuration does not declare (a typo here silently disables the credential)",
				m.credentialsPath, name))
		}
	}

	for _, name := range m.runtimeOrder {
		block := m.runtimes[name]
		if block.Auth == nil {
			continue
		}
		if names := authPasswordSources(block.Auth); len(names) > 1 {
			problems = append(problems, fmt.Sprintf(
				"runtimes.%s.auth names %d password sources (%s); name exactly one",
				name, len(names), strings.Join(names, ", ")))
			continue
		}
		// A credentials file that could not be read already explains why the pair is
		// missing, and reporting both sends the operator to change a password that is
		// not the problem. Only the more specific message is kept then.
		if _, ok := m.resolveCredential(name, getenv); !ok && len(m.credentialProblems) == 0 {
			problems = append(problems, fmt.Sprintf(
				"runtimes.%s.auth names a user but no password could be resolved: set one of `password`, `password_env`, `password_file`, add `runtimes.%s` to %s, or export FLOWHUB_OPENCODE_PASSWORD",
				name, name, m.credentialsDescription()))
		}
	}
	return problems
}

// credentialsDescription names where a shared credential would be read from, for a
// message that has to tell the operator where to put it.
func (m *Map) credentialsDescription() string {
	if strings.TrimSpace(m.credentialsPath) != "" {
		return fmt.Sprintf("the credentials file (%s)", m.credentialsPath)
	}
	return "a credentials file named by `credentials_file`"
}

// authPasswordSources lists the password fields an `auth` block names, in a fixed
// order so a refusal reads the same on every run.
func authPasswordSources(auth *v2RuntimeAuth) []string {
	if auth == nil {
		return nil
	}
	var out []string
	if strings.TrimSpace(auth.Password) != "" {
		out = append(out, "password")
	}
	if strings.TrimSpace(auth.PasswordEnv) != "" {
		out = append(out, "password_env")
	}
	if strings.TrimSpace(auth.PasswordFile) != "" {
		out = append(out, "password_file")
	}
	return out
}

// ResolveCredential answers which Basic Auth pair drives this runtime, and where the
// password came from.
//
// The order is deliberate: the runtime's own block wins over the shared file, which
// wins over the environment pair. A runtime-specific declaration that lost to a
// process-wide default would be the kind of silent override this project refuses
// elsewhere.
func (m *Map) ResolveCredential(name string, getenv func(string) string) (Credential, bool) {
	if m == nil {
		return Credential{}, false
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	return m.resolveCredential(name, getenv)
}

func (m *Map) resolveCredential(name string, getenv func(string) string) (Credential, bool) {
	block, declared := m.runtimes[strings.TrimSpace(name)]

	if declared && block.Auth != nil {
		auth := block.Auth
		user := strings.TrimSpace(auth.User)
		// More than one source is a configuration problem the startup gate reports;
		// here the first named one wins so the process still has a deterministic
		// answer if it is asked before that gate runs.
		if inline := strings.TrimSpace(auth.Password); inline != "" {
			return Credential{User: user, Password: inline, Source: "password"}, true
		}
		if variable := strings.TrimSpace(auth.PasswordEnv); variable != "" {
			if value, ok := lookupSecret(getenv, variable); ok {
				return Credential{User: user, Password: value, Source: "password_env:" + variable}, true
			}
		}
		if file := strings.TrimSpace(auth.PasswordFile); file != "" {
			if value, err := m.readSecretFile(file); err == nil {
				return Credential{User: user, Password: value, Source: "password_file:" + m.resolveRelativePath(file)}, true
			}
		}
	}

	if stored, ok := m.credentials[strings.TrimSpace(name)]; ok {
		user := stored.User
		if declared && block.Auth != nil && strings.TrimSpace(block.Auth.User) != "" {
			user = strings.TrimSpace(block.Auth.User)
		}
		return Credential{User: user, Password: stored.Password, Source: "credentials_file:" + stored.Origin + "#" + strings.TrimSpace(name)}, true
	}

	// The process-wide pair is the outermost default, exactly as it is for a runtime
	// the file never declares.
	user, password := getenv("FLOWHUB_OPENCODE_USER"), getenv("FLOWHUB_OPENCODE_PASSWORD")
	if strings.TrimSpace(user) != "" && strings.TrimSpace(password) != "" {
		return Credential{User: strings.TrimSpace(user), Password: password, Source: "FLOWHUB_OPENCODE_PASSWORD"}, true
	}
	return Credential{}, false
}

// readSecretFile reads a password file: one secret, optionally with a trailing
// newline, and nothing else.
func (m *Map) readSecretFile(path string) (string, error) {
	location := m.resolveRelativePath(path)
	if err := checkSecretPermissions("password_file", location); err != nil {
		return "", err
	}
	info, err := os.Stat(location)
	if err != nil {
		return "", fmt.Errorf("password_file %s: %v", location, err)
	}
	if info.Size() > maxSecretFileBytes {
		return "", fmt.Errorf("password_file %s is %d bytes, over the %d byte limit: a secret is one line",
			location, info.Size(), maxSecretFileBytes)
	}
	raw, err := os.ReadFile(location)
	if err != nil {
		return "", fmt.Errorf("password_file %s: %v", location, err)
	}
	// Trim the whole value: a password with deliberate leading or trailing spaces
	// cannot be told apart from an editor's newline, and silently using the wrong
	// bytes is worse than refusing the start.
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("password_file %s is empty, so it would have no effect", location)
	}
	return value, nil
}

// lookupSecret reads a variable through the injected environment and reports an
// empty value as absent: an exported-but-empty variable is half a credential pair,
// which the startup gate refuses rather than letting every turn fail on a 401.
func lookupSecret(getenv func(string) string, name string) (string, bool) {
	value := getenv(name)
	if strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

// CredentialReport renders the effective credential for every declared runtime, with
// the password replaced by its source. It is what `-print-config` prints.
//
// It resolves against the live process environment, because that is the level the
// report describes; a unit test injects through ResolveCredential instead.
func (m *Map) CredentialReport() []Provenance {
	if m == nil {
		return nil
	}
	out := make([]Provenance, 0, len(m.runtimeOrder))
	for _, name := range m.runtimeOrder {
		if value, named := m.renderAuth(name); named {
			out = append(out, Provenance{
				Scope: "runtimes." + name,
				Field: "auth",
				Value: value,
				Level: "runtimes." + name,
			})
		}
	}
	return out
}

// renderAuth renders one runtime's `auth` for a report: the user, and the level the
// password came from. It never renders the password itself, whatever the source.
func (m *Map) renderAuth(name string) (string, bool) {
	block, declared := m.runtimes[name]
	if !declared || block.Auth == nil {
		return "", false
	}
	user := strings.TrimSpace(block.Auth.User)
	credential, ok := m.ResolveCredential(name, os.Getenv)
	if !ok {
		return fmt.Sprintf("user=%s password from <unresolved>", user), true
	}
	// The environment pair is a process-wide default, so it is only this runtime's
	// source when nothing more specific answered; ResolveCredential already applied
	// that order, so its answer is reported as-is.
	return fmt.Sprintf("user=%s password from %s", user, credential.Source), true
}

// HasInlinePassword reports whether the configuration file itself carries a
// password (`runtimes.<name>.auth.password`) rather than naming a variable or a
// file.
//
// `main` uses it to decide whether the configuration file's own permissions must be
// enforced: a file that holds a secret has to be 0600, while a file that only names
// variables and paths has no reason to be, and refusing it would break every
// existing deployment for no gain.
func (m *Map) HasInlinePassword() bool {
	if m == nil {
		return false
	}
	for _, name := range m.runtimeOrder {
		if block := m.runtimes[name]; block.Auth != nil && strings.TrimSpace(block.Auth.Password) != "" {
			return true
		}
	}
	return false
}
