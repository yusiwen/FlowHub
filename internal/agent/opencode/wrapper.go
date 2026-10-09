package opencode

import (
	"fmt"
	"strings"
)

// Shell wrappers are the command prefixes the operator lets this host put in front
// of a command before the permission request is raised.
//
// They exist because opencode's `tool.execute.before` plugin hook can rewrite a bash
// command *after* the model writes it and *before* anything is asked, and the model
// never learns that it happened: its own tool input still holds what it typed. The
// allowlist here is anchored at the command name, so a rewrite turns an allowed
// command into a refused one and the refusal names a command the model never wrote.
//
// Measured on 2026-10-09: the host's `rtk` plugin rewrites `git status` into
// `rtk git status` (and `log`, `diff`, `show`, `branch` the same way), so *every*
// read-only git inspection was refused, and the model spent its whole turn retrying
// commands that were rewritten again each time. The session ended with no comment on
// the issue.
//
// Two things follow, and both are implemented here:
//
//   - The refusal has to be legible even when nothing is configured (looksWrapped):
//     saying "rephrase using inspection commands" to a model whose every rephrase is
//     rewritten the same way is a dead turn, and the honest answer — "this looks
//     rewritten by a wrapper" — is one it can report to a human.
//   - A wrapper the *operator* named is stripped, and the remainder is judged by the
//     same deny-then-allow lists (stripWrappers, permitted). Trusting a wrapper can
//     therefore never admit a command the policy refuses: `rtk rm -rf /` strips to
//     `rm -rf /` and is refused by the deny list, exactly as if the wrapper were not
//     there. The list is empty by default and never guesses: an unnamed prefix is
//     not stripped.
//
// Which prefix is trusted is the operator's declaration, not this package's: it
// arrives through `FLOWHUB_SHELL_WRAPPERS` or `runtimes.<name>.shell_wrappers`, so no
// tool's name is baked into the core.

// MaxShellWrappers bounds how many prefixes one runtime may trust. The bound is not
// a security control — every candidate is judged by the same lists — it is a guard
// against a configuration line that obviously is not a list of command names.
const MaxShellWrappers = 16

// stripWrappers returns the text the shell policy should judge for one segment, plus
// the wrapper it was taken from.
//
// Stripping is bounded by the number of configured wrappers, so a segment wrapped
// repeatedly (`rtk rtk git status`) terminates. A segment that is *only* a wrapper is
// left alone: an empty target must never be the thing that gets allowed.
func (a *Arbiter) stripWrappers(segment string) (target, wrapper string) {
	target = segment
	if len(a.Wrappers) == 0 {
		return target, ""
	}
	for round := 0; round <= len(a.Wrappers); round++ {
		fields := strings.Fields(target)
		if len(fields) < 2 {
			break
		}
		if !a.trustsWrapper(fields[0]) {
			break
		}
		wrapper = fields[0]
		target = strings.TrimSpace(strings.TrimPrefix(target, fields[0]))
	}
	return target, wrapper
}

func (a *Arbiter) trustsWrapper(name string) bool {
	for _, candidate := range a.Wrappers {
		if candidate == name {
			return true
		}
	}
	return false
}

// permitted reports whether a segment is acceptable as a command target in the
// current phase: an execution-phase exception, or the read-only allowlist. The deny
// list is consulted by the caller first, because a denied command deserves its own
// reason.
func (a *Arbiter) permitted(segment string) bool {
	if a.Phase == PhaseExecution && matchesAny(a.ExecutionAllow, segment) {
		return true
	}
	return a.matchesAny(segment)
}

// looksWrapped reports the first token of a refused segment, when removing it leaves a
// segment the policy would have accepted.
//
// This is the diagnostic half of wrapper support and it needs no configuration: it
// fires exactly when the command in hand looks like an allowed command wearing an
// unknown prefix, which is what an in-place plugin rewrite produces.
func (a *Arbiter) looksWrapped(segment string) (wrapper, rest string, ok bool) {
	fields := strings.Fields(segment)
	if len(fields) < 2 {
		return "", "", false
	}
	rest = strings.TrimSpace(strings.TrimPrefix(segment, fields[0]))
	if rest == "" || !a.permitted(rest) {
		return "", "", false
	}
	return fields[0], rest, true
}

// rewriteReason explains a segment that is only refused because of its wrapper. It is
// deliberately addressed to the model: it names the command the model actually wrote,
// says that retrying changes nothing, and asks for the fact to be reported rather
// than retried.
func rewriteReason(segment, wrapper, rest string) string {
	return fmt.Sprintf(
		"command segment %q is not on the read-only allowlist, but %q is: a host-side wrapper (%q) appears to have rewritten the command after you wrote it, so every retry will be rewritten the same way. Say so in your reply instead of retrying; the operator can trust %q for this runtime with runtimes.<name>.shell_wrappers or FLOWHUB_SHELL_WRAPPERS",
		segment, rest, wrapper, wrapper)
}

// ValidateShellWrappers refuses a wrapper name that is not a bare command name.
//
// The match is against a segment's first field, so an entry that is not a command
// name can never match and would silently do nothing — the failure this project
// refuses everywhere else. An entry that is repeated is refused too, for the same
// reason a duplicate key in the environment file is: which one is in force must not
// be a guess.
func ValidateShellWrappers(names []string) error {
	if len(names) > MaxShellWrappers {
		return fmt.Errorf("%d wrappers exceed the supported maximum of %d; list only the prefixes this host actually inserts", len(names), MaxShellWrappers)
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !validShellWrapperName(name) {
			return fmt.Errorf("%q is not a command name: a wrapper is matched against a command's first word, so it may only contain letters, digits, dot, dash, underscore or plus and must not start with a dash or a path separator", name)
		}
		if seen[name] {
			return fmt.Errorf("%q is listed twice", name)
		}
		seen[name] = true
	}
	return nil
}

// validShellWrapperName reports whether a name can be a command name: a letter,
// digit or underscore first, then letters, digits, dot, dash, underscore or plus. A
// path separator, a space or any shell metacharacter is refused, so an entry can
// never smuggle a second command into the configuration.
func validShellWrapperName(name string) bool {
	if name == "" {
		return false
	}
	for index, char := range name {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case index > 0 && (char == '.' || char == '-' || char == '_' || char == '+'):
		case index == 0 && char == '_':
		default:
			return false
		}
	}
	return true
}
