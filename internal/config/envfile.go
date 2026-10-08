package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The environment file is the answer to "I do not want to export six variables in
// every new shell". It is a file of `KEY=VALUE` pairs, read once at start, whose
// values become *defaults*: a variable already present in the real process
// environment is never replaced.
//
// The direction is the load-bearing decision. Letting a file override the
// environment would mean `FLOWHUB_ADDR=… ./bin/flowhub` silently used a different
// address because of a file the operator forgot, which is the same class of silent
// override the credentials surface refuses (ADR 0004). Deployments that configure
// through the environment — systemd `Environment=`, containers' `-e`, a shell that
// exported the value on purpose — keep doing exactly what they say.
//
// The file is also a secret store in practice: it holds the URL key and the header
// token. It therefore carries the same permission floor as the credentials file, and
// its contents never reach a log or a report — only the path and the variable names.

// EnvFileName is the file's default name, inside the configuration directory.
const EnvFileName = ".env"

// EnvFileVariable names an alternative environment file. The empty value means the
// default path; `-` or `none` disables the file entirely, which is how a deployment
// that wants "environment only" states that on purpose.
const EnvFileVariable = "FLOWHUB_ENV_FILE"

// EnvFileDisabled is the value of EnvFileVariable that turns the file off.
const EnvFileDisabled = "-"

// envKeyPattern is what a variable name in the file may look like. It is the POSIX
// name rule, which is also every name this program reads.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvFileResult reports what the environment file did, without carrying a value:
// result is a report for a human, and a value only makes it into the process.
type EnvFileResult struct {
	// Path is the file that was read, or would have been read.
	Path string
	// Applied lists the variable names the file supplied. Names only — the values are
	// in the process environment and nowhere else.
	Applied []string
	// Skipped lists names the file carried but the real environment had already set.
	// They are the reason a file entry can appear to do nothing, so they are reported.
	Skipped []string
	// Found reports whether the file existed.
	Found bool
}

// ApplyEnvFile applies the environment file to this process, before the receiver
// configuration is loaded.
//
// The path comes from `overrides.Path`, or `FLOWHUB_ENV_FILE`, or the default beside
// the configuration file (see envFilePath). An absent default path is fine — most
// hosts have no file — while an absent path the operator named is an error, because a
// named file that does not exist is a mistake, not a configuration style.
func ApplyEnvFile(overrides EnvFileOptions) (EnvFileResult, error) {
	path, disabled, named := envFilePath(overrides)
	if disabled {
		return EnvFileResult{}, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if overrides.Require || named {
				return EnvFileResult{Path: path}, fmt.Errorf(
					"environment file %s was named but does not exist: remove %s from the environment (or set it to %q) to configure through the environment only",
					path, EnvFileVariable, EnvFileDisabled)
			}
			return EnvFileResult{Path: path}, nil
		}
		return EnvFileResult{Path: path}, fmt.Errorf("environment file %s: %w", path, err)
	}

	if err := checkEnvFilePermissions(path); err != nil {
		return EnvFileResult{Path: path, Found: true}, err
	}

	pairs, err := parseEnvFile(raw)
	if err != nil {
		return EnvFileResult{Path: path, Found: true}, fmt.Errorf("environment file %s: %w", path, err)
	}

	result := EnvFileResult{Path: path, Found: true}
	for _, pair := range pairs {
		if _, present := os.LookupEnv(pair.Name); present {
			// The real environment wins. An empty-but-set variable counts as set: the
			// operator who wrote `FLOWHUB_WHATEVER=` meant it, and the program's own
			// validation is where a half a value is refused.
			result.Skipped = append(result.Skipped, pair.Name)
			continue
		}
		if err := os.Setenv(pair.Name, pair.Value); err != nil {
			return result, fmt.Errorf("environment file %s: set %s: %w", path, pair.Name, err)
		}
		result.Applied = append(result.Applied, pair.Name)
	}
	return result, nil
}

// EnvFileOptions is what the caller may say about the environment file.
type EnvFileOptions struct {
	// Path names the file directly, overriding FLOWHUB_ENV_FILE and the default.
	Path string
	// Require makes an absent file an error even when the path was defaulted.
	Require bool
}

// envFilePath resolves which file to read. It reports whether the feature is disabled,
// and whether the path was named rather than defaulted (a named path that does not
// exist is an error).
//
// Precedence: an explicit path, then FLOWHUB_ENV_FILE, then the configuration
// directory — the same directory as the routing table, because both describe this
// host. The disable values win over the path so that `FLOWHUB_ENV_FILE=-` cannot
// accidentally name a file called `-`.
func envFilePath(overrides EnvFileOptions) (path string, disabled, named bool) {
	if value := strings.TrimSpace(overrides.Path); value != "" {
		return value, false, true
	}
	value := strings.TrimSpace(os.Getenv(EnvFileVariable))
	switch strings.ToLower(value) {
	case "":
		return DefaultEnvFile(), false, false
	case EnvFileDisabled, "none", "off":
		return "", true, false
	default:
		return value, false, true
	}
}

// DefaultEnvFile is the environment file beside the configuration file:
// $XDG_CONFIG_HOME/flowhub/.env, or ~/.config/flowhub/.env.
func DefaultEnvFile() string {
	return filepath.Join(filepath.Dir(DefaultProjectsFile()), EnvFileName)
}

// envFileMode is the permission floor: group and other must have no access. The file
// holds the URL key and the header token, so the rule is the one the credentials file
// already follows (ADR 0004).
func checkEnvFilePermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("environment file %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("environment file %s is a directory, not a file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("environment file %s is mode %04o: group and other must have no access to a file that holds the URL key and the token; fix it with `chmod 600 %s`",
			path, info.Mode().Perm(), path)
	}
	return nil
}

// envPair is one parsed assignment.
type envPair struct {
	Name  string
	Value string
}

// parseEnvFile reads the dotenv subset this project supports: `KEY=VALUE`, `#`
// comments, blank lines, an optional `export ` prefix, and surrounding single or
// double quotes stripped.
//
// Deliberately not supported: `${VAR}` interpolation, includes, line continuations and
// shell evaluation. Each would be another way for a variable to become something the
// operator did not write, and a value that is silently different is worse than a
// refusal. A malformed line refuses rather than being skipped for the same reason.
func parseEnvFile(raw []byte) ([]envPair, error) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	pairs := make([]envPair, 0, len(lines))
	seen := make(map[string]bool, len(lines))

	for index, line := range lines {
		number := index + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// `export FOO=bar` is what a copied shell line looks like; accepting the prefix
		// is cheaper than explaining why it is refused.
		if rest, ok := strings.CutPrefix(trimmed, "export "); ok {
			trimmed = strings.TrimSpace(rest)
		}

		name, value, found := strings.Cut(trimmed, "=")
		if !found {
			return nil, fmt.Errorf("line %d: %q is not a KEY=VALUE assignment", number, line)
		}
		name = strings.TrimSpace(name)
		if !envKeyPattern.MatchString(name) {
			return nil, fmt.Errorf("line %d: %q is not a variable name", number, name)
		}
		if seen[name] {
			// Last-write-wins would be a guess about which line the operator meant; the
			// file is editable and a duplicate is almost always a half-finished edit.
			return nil, fmt.Errorf("line %d: %s is set twice in this file", number, name)
		}
		seen[name] = true

		pairs = append(pairs, envPair{Name: name, Value: unquoteEnvValue(strings.TrimSpace(value))})
	}
	return pairs, nil
}

// unquoteEnvValue strips one layer of matching surrounding quotes, and nothing else.
// An unquoted value keeps its inner spaces, so `A=b c` is `b c` — the shell would
// split that into a command, but this is a file, not a shell.
func unquoteEnvValue(value string) string {
	if len(value) < 2 {
		return value
	}
	quote := value[0]
	if quote != '"' && quote != '\'' {
		return value
	}
	if value[len(value)-1] != quote {
		return value
	}
	return value[1 : len(value)-1]
}
