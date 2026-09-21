package opencode

import (
	"fmt"
	"regexp"
	"strings"
)

// Decision is the arbiter's verdict on one permission request.
type Decision struct {
	Reply  Reply
	Reason string
}

// Allowed reports whether the decision lets the tool call through.
func (d Decision) Allowed() bool { return d.Reply == ReplyOnce || d.Reply == ReplyAlways }

// Arbiter answers permission requests without a human.
//
// It is the second line of defence, not the first: the session ruleset already
// denies edit-adjacent permissions it never wants to reason about, and the phase
// only matters because the gated permission is set to "ask" — "allow" would bypass
// the arbiter and "deny" would remove the tool, making a per-phase decision
// impossible.
//
// The bash policy is an allowlist over shell *segments*, not a shell parser:
//
//   - the command is split on &&, ||, ;, | and newlines, and every segment must
//     match an allow entry (or the narrow curl exception);
//   - redirection, command substitution and background execution are rejected
//     outright, because they turn a read into a write or hide a second command;
//   - the deny list runs before the allowlist, so a forbidden command yields a
//     clear reason instead of a generic one;
//   - paths outside the worktree are rejected: the ruleset's external_directory
//     does not police a command run inside a shell.
//
// Build and test commands are on the default allowlist because they are the
// agent's job, and they are also arbitrary code execution: the containment that
// matters is the per-task worktree plus the session ruleset, not this list.
type Arbiter struct {
	// Allow matches a whole command segment.
	Allow []*regexp.Regexp
	// Deny matches a whole command segment and wins over Allow.
	Deny []*regexp.Regexp
	// ExecutionAllow are whole-segment patterns permitted only in the execution
	// phase. They are checked before Deny, which is how `git add` and
	// `git commit` stay impossible during a read-only analysis turn.
	ExecutionAllow []*regexp.Regexp
	// AllowKinds are built-in read-only tool permissions granted without
	// inspecting a command.
	AllowKinds map[string]bool
	// AllowTools are MCP and plugin tools granted by exact name. Anything not
	// listed is refused, because those tools are `allow` by default and the
	// available set includes web crawlers and cross-system writers.
	AllowTools map[string]bool
	// Remember replies "always" instead of "once" for allowed commands. It
	// removes repeat prompts at the cost of visibility, so it defaults to false.
	Remember bool
	// Phase selects the read-only or the execution policy.
	Phase Phase
	// CurlHosts whitelists hosts an attachment download may reach. Empty
	// disables the download exception entirely.
	CurlHosts []string
	// CurlOutputPrefix is the only directory a download may write into, relative
	// to the worktree.
	CurlOutputPrefix string
}

// DefaultArbiter returns the read-only policy used for unattended runs.
func DefaultArbiter() *Arbiter {
	return &Arbiter{
		Phase: PhaseAnalysis,
		Allow: []*regexp.Regexp{
			// Inspection.
			mustCompile(`(pwd|ls|cat|head|tail|wc|grep|rg|find|file|stat|uname|whoami|id|date|diff|sort|uniq|cut|tr|jq|echo|printf|sed -n|awk)(\s.*)?`),
			// Git, read-only. git remote/config are excluded on purpose: remote
			// URLs can embed credentials.
			mustCompile(`git\s+(status|log|diff|show|branch|rev-parse|describe|ls-files|blame|tag|shortlog|whatchanged|cat-file|grep|stash\s+list)(\s.*)?`),
			// Build and test: the agent's actual job.
			mustCompile(`go\s+(build|vet|test|list|fmt|generate)(\s.*)?`),
			mustCompile(`go\s+mod\s+(verify)(\s.*)?`),
			mustCompile(`(make|npm|pnpm|yarn|bun)\s+(test|tests|lint|build|check|typecheck|verify)(\s.*)?`),
			mustCompile(`(npm|pnpm|yarn|bun)\s+run\s+(test|lint|build|check|typecheck)(\s.*)?`),
			// Checkers that only report. Formatters that rewrite files are in
			// ExecutionAllow instead, so a read-only turn cannot change the tree.
			mustCompile(`(ruff\s+check|mypy|eslint|tsc|staticcheck|golangci-lint|gofmt\s+-[ld])(\s.*)?`),
		},
		ExecutionAllow: []*regexp.Regexp{
			// The user approved local commits in the task worktree, and never a
			// push: `git push` stays on the deny list.
			mustCompile(`git\s+add(\s.*)?`),
			mustCompile(`git\s+commit(\s.*)?`),
			mustCompile(`git\s+restore\s+--staged(\s.*)?`),
			// Formatters and dependency updates that write.
			mustCompile(`gofmt\s+-w(\s.*)?`),
			mustCompile(`black(\s.*)?`),
			mustCompile(`(prettier|ruff|eslint)(\s.*)?(--write|--fix|-w)(\s.*)?`),
			mustCompile(`go\s+mod\s+(tidy|download)(\s.*)?`),
		},
		Deny: []*regexp.Regexp{
			mustCompile(`(sudo|su|doas)\b.*`),
			mustCompile(`rm\b.*`),
			mustCompile(`(mv|cp|mkdir|rmdir|touch|chmod|chown|ln|install|truncate|dd|tee)\b.*`),
			mustCompile(`(curl|wget|nc|ncat|telnet|scp|sftp|rsync|ssh|ftp)\b.*`),
			mustCompile(`git\s+(push|fetch|pull|clone|remote|config|reset|checkout|switch|clean|apply|rebase|merge|commit|add|rm|restore)\b.*`),
			mustCompile(`(docker|podman|kubectl|helm|systemctl|launchctl|service|kill|pkill|killall|shutdown|reboot)\b.*`),
			mustCompile(`(npm|pnpm|yarn|bun|pip|pip3|gem|cargo|go)\s+(install|publish|add|remove|uninstall|get)\b.*`),
			mustCompile(`(sh|bash|zsh|fish|python|python3|perl|ruby|node|osascript|env|printenv)\b.*`),
			mustCompile(`.*[;&|].*`), // any separator that survived splitting is suspicious
		},
		AllowKinds: map[string]bool{
			// Read-only built-ins. `read` cannot leave the worktree because the
			// ruleset denies external_directory outright.
			"read": true, "glob": true, "grep": true, "list": true, "todowrite": true,
		},
		CurlHosts:        append([]string(nil), DefaultCurlHosts...),
		CurlOutputPrefix: DefaultAttachmentPathPrefix,
	}
}

func (a *Arbiter) matchesAny(segment string) bool {
	for _, deny := range a.Deny {
		if deny.MatchString(segment) {
			return false
		}
	}
	return matchesAny(a.Allow, segment)
}

func (a *Arbiter) allow(reason string) Decision {
	reply := ReplyOnce
	if a.Remember {
		reply = ReplyAlways
	}
	return Decision{Reply: reply, Reason: reason}
}

// escapesWorktree reports whether a command reaches outside the task's checkout.
//
// The session ruleset denies external_directory, but that only covers tools that
// open a path: a shell command runs in a shell, where nothing polices the paths
// it names. `cat ~/.ssh/id_rsa` and `tail -f /var/log/system.log` therefore have
// to be caught here. An agent that works in its own worktree never needs an
// absolute path, so refusing them outright is cheap.
func escapesWorktree(command string) (bool, string) {
	const reason = "paths outside the task worktree are not allowed; use a path relative to the working directory"
	for _, token := range strings.Fields(command) {
		token = strings.Trim(token, `"'`)
		if token == "" || token == "-" {
			continue
		}
		if strings.HasPrefix(token, "~") {
			return true, reason
		}
		if strings.HasPrefix(token, "/") {
			return true, reason
		}
		if _, value, found := strings.Cut(token, "="); found && strings.HasPrefix(value, "/") {
			return true, reason
		}
	}
	return false, ""
}

// unsafeShell reports shell constructs that make a command impossible to judge by
// looking at its segments alone.
//
// The scan is quote aware: a URL query string contains `&` and a grep pattern may
// contain `>`, and neither is shell syntax inside quotes. Only unquoted
// metacharacters change what a command does, so only those are rejected. Command
// substitution is checked inside double quotes too, because it executes there,
// while single quotes are literal.
func unsafeShell(command string) (bool, string) {
	var quote byte
	for index := 0; index < len(command); index++ {
		char := command[index]
		switch quote {
		case '\'':
			if char == '\'' {
				quote = 0
			}
			continue
		case '"':
			switch {
			case char == '\\':
				index++
			case char == '"':
				quote = 0
			case char == '`':
				return true, "command substitution with backticks is not allowed"
			case char == '$' && index+1 < len(command) && command[index+1] == '(':
				return true, "command substitution $(…) is not allowed"
			}
			continue
		}
		switch char {
		case 0:
			return true, "NUL byte in command"
		case '\'':
			quote = '\''
		case '"':
			quote = '"'
		case '`':
			return true, "command substitution with backticks is not allowed"
		case '$':
			if index+1 < len(command) && command[index+1] == '(' {
				return true, "command substitution $(…) is not allowed"
			}
		case '>':
			return true, "output redirection is not allowed (it turns a read into a write)"
		case '<':
			return true, "input redirection is not allowed"
		case '&':
			previous := index > 0 && command[index-1] == '&'
			next := index+1 < len(command) && command[index+1] == '&'
			if !previous && !next {
				return true, "background execution with & is not allowed"
			}
		}
	}
	if quote != 0 {
		return true, "unbalanced quotes make the command impossible to judge"
	}
	return false, ""
}

// splitSegments breaks a compound command into the parts that each get judged.
func splitSegments(command string) []string {
	replaced := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n", "\r", "\n").Replace(command)
	var segments []string
	for _, line := range strings.Split(replaced, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			segments = append(segments, line)
		}
	}
	return segments
}

func mustCompile(pattern string) *regexp.Regexp {
	return regexp.MustCompile(`^(?:` + pattern + `)$`)
}

func plural(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
