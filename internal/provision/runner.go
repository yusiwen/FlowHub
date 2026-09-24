// Package provision prepares a host to serve as a FlowHub runtime, and reports
// what that host can and cannot do.
//
// It runs on the machine that will execute turns, not on the control plane, and
// that is the whole point: the checks have to run as the user whose HOME the
// agent will work in. A capability check that runs from somewhere else — or as
// somebody else — passes and then the first real push fails with a 401.
//
// The only implemented command is `flowhub runtime init --check`, which writes
// nothing and registers nothing. Installation (artifacts and the manifest) and
// enrollment (the admin API) are ADR 0002 steps 2 and 3.
package provision

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"time"
)

// Runner is the boundary between the checks and the host. Everything the checks
// need to learn about the machine goes through it, so the whole report can be
// tested without executing a single command.
type Runner interface {
	// Lookup resolves a binary the way exec.LookPath does.
	Lookup(name string) (string, bool)
	// Run executes a command and returns its combined output. A non-nil error
	// means the command failed; the output still holds whatever it printed.
	Run(ctx context.Context, dir, name string, args ...string) (string, error)
	// Getenv reports whether a variable is set. The value is returned only so the
	// caller can decide on emptiness; it is never written to the report.
	Getenv(key string) (string, bool)
	// Stat reports whether a path exists, and whether it is a directory. A
	// non-nil error means the check could not look, which is not the same as
	// "absent": reporting a permission failure as "does not exist" sends an
	// operator looking for a file that is right there.
	Stat(path string) (exists bool, isDir bool, err error)
	// TempDir is a directory that exists and is safe to run a command in when no
	// repository is involved.
	TempDir() string
	// Identity describes who this process is, which is what a later turn has to
	// match.
	Identity() Identity
	Hostname() string
	GOOS() string
	GOARCH() string
}

// Identity is the user the check ran as. The report carries it so the control
// plane can compare it with the HOME its turns actually run in.
type Identity struct {
	User string `json:"user"`
	UID  string `json:"uid"`
	Home string `json:"home"`
}

// ExecRunner is the real Runner: it shell outs to the host's tools.
type ExecRunner struct {
	// Timeout bounds one command. A credential prompt or an unreachable host must
	// fail the check rather than hang it.
	Timeout time.Duration
}

// DefaultCommandTimeout bounds a single check command.
const DefaultCommandTimeout = 20 * time.Second

// Run executes one command with a bounded context and an environment that cannot
// stop to ask a human for anything.
//
// The overrides matter as much as the timeout: without them, `git ls-remote` on a
// repository whose credentials are missing waits for a password, `ssh` asks for a
// host key, and a credential helper opens a window. A check that can block is
// worse than a check that fails.
func (r ExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = mergeEnv(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // never prompt for an HTTP credential
		"GIT_ASKPASS=",          // and not through a helper either
		"SSH_ASKPASS=",
		"GCM_INTERACTIVE=never", // git-credential-manager
		// A key passphrase or an unknown host key would otherwise block on a
		// prompt; accept-new keeps a first connection working without one.
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new",
		// The check must not take optional locks in somebody's clone: it is a
		// visitor there, and a background run holding a lock is a real annoyance.
		"GIT_OPTIONAL_LOCKS=0",
	)

	out, err := command.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return text, fmt.Errorf("timed out after %s", timeout)
		}
		return text, err
	}
	return text, nil
}

// Lookup resolves a binary on PATH.
func (r ExecRunner) Lookup(name string) (string, bool) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", false
	}
	return path, true
}

// Getenv reads the environment without interpreting it.
func (r ExecRunner) Getenv(key string) (string, bool) { return osLookupEnv(key) }

// Stat reports whether a path exists and whether it is a directory. Only
// fs.ErrNotExist is reported as absence; anything else (a denied parent
// directory, an I/O error) comes back as an error.
func (r ExecRunner) Stat(path string) (bool, bool, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		return true, info.IsDir(), nil
	case errors.Is(err, fs.ErrNotExist):
		return false, false, nil
	default:
		return false, false, err
	}
}

// TempDir is the operating system's temporary directory.
func (r ExecRunner) TempDir() string { return os.TempDir() }

// Identity describes the running process, falling back to the environment when
// the user database is unavailable.
func (r ExecRunner) Identity() Identity { return currentIdentity() }

// Hostname is the machine's name, best effort.
func (r ExecRunner) Hostname() string { return osHostname() }

// GOOS is the operating system this check is running on.
func (r ExecRunner) GOOS() string { return osGOOS() }

// GOARCH is the architecture this check is running on.
func (r ExecRunner) GOARCH() string { return osGOARCH() }

// boundedError is a helper for tests and callers that want a short reason.
func boundedError(output string, err error) string {
	if line := firstLine(output); line != "" {
		return line
	}
	if err == nil {
		return "failed"
	}
	return firstLine(err.Error())
}

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncate(line, 200)
		}
	}
	return ""
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// mergeEnv applies overrides on top of a base environment.
//
// It removes the base entries for the keys being overridden instead of appending
// a duplicate: getenv returns the first match, so an appended override would be
// silently ignored exactly when the parent environment already sets it (a user
// with GIT_TERMINAL_PROMPT=1 in their profile is the case that bit).
func mergeEnv(base []string, overrides ...string) []string {
	drop := make(map[string]bool, len(overrides))
	for _, override := range overrides {
		if key, _, found := strings.Cut(override, "="); found {
			drop[key] = true
		}
	}
	merged := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		if key, _, found := strings.Cut(entry, "="); found && drop[key] {
			continue
		}
		merged = append(merged, entry)
	}
	return append(merged, overrides...)
}

// The os wrappers keep Runner implementations free of direct os calls, which is
// what makes the checks testable with a fake.
func osLookupEnv(key string) (string, bool) { return os.LookupEnv(key) }

func osHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

func osGOOS() string   { return runtime.GOOS }
func osGOARCH() string { return runtime.GOARCH }

func currentIdentity() Identity {
	identity := Identity{User: os.Getenv("USER"), Home: os.Getenv("HOME")}
	if current, err := user.Current(); err == nil {
		if current.Username != "" {
			identity.User = current.Username
		}
		if current.Uid != "" {
			identity.UID = current.Uid
		}
		if current.HomeDir != "" {
			identity.Home = current.HomeDir
		}
	}
	return identity
}
