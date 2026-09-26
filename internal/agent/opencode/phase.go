package opencode

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yusiwen/flowhub/internal/agent"
)

// Phase is the lifecycle position of a turn. The same session runs the analysis
// and the execution turn, so a session ruleset cannot express this: the ruleset is
// fixed at session creation and would either deny edits forever or allow them from
// the start. The arbiter therefore carries the phase.
//
// This only works because the gated permission is set to "ask" in the ruleset.
// "allow" bypasses the arbiter entirely and "deny" removes the tool, so both would
// make a phase decision impossible.
//
// It is an alias for the neutral agent.Phase: which phase a turn is in is a fact
// about the work, not about this product. The policy that acts on it stays here.
type Phase = agent.Phase

const (
	// PhaseAnalysis is read-only: the agent may inspect the issue and the
	// repository and must post its findings.
	PhaseAnalysis = agent.PhaseAnalysis
	// PhaseExecution may modify the task worktree.
	PhaseExecution = agent.PhaseExecution
)

// DefaultCurlHosts is the YouTrack host whose signed attachment URLs the agent is
// allowed to download from.
var DefaultCurlHosts = []string{"pm.yusiwen.cn"}

// DefaultAttachmentPathPrefix is the only path a download may target.
const DefaultAttachmentPathPrefix = ".flowhub/attachments"

// NewAnalysisArbiter returns the policy for a read-only turn.
func NewAnalysisArbiter() *Arbiter {
	arbiter := DefaultArbiter()
	arbiter.Phase = PhaseAnalysis
	return arbiter
}

// NewExecutionArbiter returns the policy for a turn that may change the worktree.
//
// It still refuses to publish: `git push` stays on the deny list, and the agent
// never holds a remote credential. Committing locally is allowed so that a task
// leaves reviewable history behind.
func NewExecutionArbiter() *Arbiter {
	arbiter := DefaultArbiter()
	arbiter.Phase = PhaseExecution
	return arbiter
}

// Decide answers one permission request.
//
// The order of checks matters and is deliberate:
//
//  1. shell constructs that make a command unjudgeable;
//  2. execution-phase exceptions (git add/commit, gofmt -w) — these must run
//     before the deny list, which contains `git add` and `git commit` because a
//     read-only analysis turn must not create commits;
//  3. the deny list, so a known-dangerous command gets its own clear reason;
//  4. paths outside the worktree;
//  5. the attachment-download exception;
//  6. the read-only allowlist.
func (a *Arbiter) Decide(req PermissionRequest) Decision {
	if a == nil {
		return Decision{Reply: ReplyReject, Reason: "no permission policy is configured"}
	}
	if req.Permission == "bash" {
		return a.decideBash(req)
	}
	return a.decideTool(req)
}

// decideTool answers a request for a non-bash tool. MCP and plugin tools arrive
// here with their bare tool name as the permission (measured:
// `youtrack_get_current_user`), so anything not explicitly listed is refused.
// That default matters: MCP tools are `allow` unless the ruleset asks, and the
// available set includes web crawlers and cross-system writers.
func (a *Arbiter) decideTool(req PermissionRequest) Decision {
	name := req.Permission
	switch name {
	case "edit":
		if a.Phase == PhaseExecution {
			return a.allow("editing the task worktree is allowed in the execution phase")
		}
		return Decision{Reply: ReplyReject, Reason: "the analysis phase is read-only; propose the change instead of making it"}
	case "external_directory":
		return Decision{Reply: ReplyReject, Reason: "paths outside the task worktree are never allowed"}
	case "webfetch", "websearch":
		return Decision{Reply: ReplyReject, Reason: "network access through the built-in tools is not allowed"}
	case "bash":
		// Unreachable: handled by decideBash.
		return Decision{Reply: ReplyReject, Reason: "unexpected bash request"}
	}
	if a.AllowKinds[name] {
		return a.allow("tool " + name + " is read-only")
	}
	if a.AllowTools[name] {
		return a.allow("tool " + name + " is on the allowlist")
	}
	return Decision{
		Reply:  ReplyReject,
		Reason: fmt.Sprintf("tool %q is not granted to unattended runs", name),
	}
}

func (a *Arbiter) decideBash(req PermissionRequest) Decision {
	command := strings.TrimSpace(req.Metadata.Command)
	if command == "" {
		// Fall back to the request's own patterns: the server sends them
		// alongside the command, and an empty command must not mean "allow".
		command = strings.Join(req.Patterns, " && ")
	}
	if command == "" {
		return Decision{Reply: ReplyReject, Reason: "permission request carries no command to judge"}
	}

	if unsafe, why := unsafeShell(command); unsafe {
		return Decision{Reply: ReplyReject, Reason: why}
	}

	segments := splitSegments(command)
	if len(segments) == 0 {
		return Decision{Reply: ReplyReject, Reason: "permission request carries no command to judge"}
	}

	for _, segment := range segments {
		if a.Phase == PhaseExecution && matchesAny(a.ExecutionAllow, segment) {
			continue
		}
		// A segment that *starts* with curl is judged by the attachment exception
		// below; the deny list still catches curl smuggled into another command
		// (for example `find . -exec curl …`), which is why the pattern stays.
		if isCurl(segment) {
			continue
		}
		if matched, deny := firstMatch(a.Deny, segment); matched {
			return Decision{
				Reply:  ReplyReject,
				Reason: fmt.Sprintf("command segment %q is on the deny list (%s); run it yourself if it is really needed", segment, deny),
			}
		}
	}

	if escapes, why := escapesWorktree(command); escapes {
		return Decision{Reply: ReplyReject, Reason: why}
	}

	for _, segment := range segments {
		if matchesAny(a.ExecutionAllow, segment) {
			if a.Phase == PhaseExecution {
				continue
			}
			return Decision{
				Reply:  ReplyReject,
				Reason: fmt.Sprintf("command segment %q changes files and is only allowed in the execution phase", segment),
			}
		}
		if isCurl(segment) {
			if ok, why := a.allowCurl(segment); !ok {
				return Decision{Reply: ReplyReject, Reason: why}
			}
			continue
		}
		if !a.matchesAny(segment) {
			return Decision{
				Reply:  ReplyReject,
				Reason: fmt.Sprintf("command segment %q is not on the read-only allowlist; rephrase using inspection commands, or ask a human", segment),
			}
		}
	}

	// The patterns array must agree with the command: a pattern the policy has
	// not seen means the metadata and the request disagree, which is not a state
	// to guess in.
	for _, pattern := range req.Patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if matchesAny(a.ExecutionAllow, pattern) {
			if a.Phase == PhaseExecution {
				continue
			}
			return Decision{
				Reply:  ReplyReject,
				Reason: fmt.Sprintf("request pattern %q only applies to the execution phase", pattern),
			}
		}
		if isCurl(pattern) {
			if ok, why := a.allowCurl(pattern); !ok {
				return Decision{Reply: ReplyReject, Reason: fmt.Sprintf("request pattern %q: %s", pattern, why)}
			}
			continue
		}
		if !a.matchesAny(pattern) {
			return Decision{
				Reply:  ReplyReject,
				Reason: fmt.Sprintf("request pattern %q is not on the read-only allowlist", pattern),
			}
		}
	}

	return a.allow(fmt.Sprintf("all %s are permitted", plural(len(segments), "segment")))
}

// curlUploadFlags are the flags that turn a download into something else: sending
// data, choosing a method, or reading further instructions from a file.
var curlUploadFlags = map[string]bool{
	"-d": true, "--data": true, "--data-raw": true, "--data-binary": true,
	"--data-urlencode": true, "--data-ascii": true,
	"-F": true, "--form": true, "--form-string": true,
	"-T": true, "--upload-file": true,
	"-X": true, "--request": true,
	"-K": true, "--config": true,
	"-G": true, "--get": true,
	"-x": true, "--proxy": true, "--proxy-user": true,
	"-u": true, "--user": true,
	"-b": true, "--cookie": true, "--cookie-jar": true,
	"--trace": true, "--trace-ascii": true, "--trace-config": true,
}

func isCurl(segment string) bool {
	fields := strings.Fields(segment)
	return len(fields) > 0 && (fields[0] == "curl" || fields[0] == "wget")
}

// allowCurl is the single narrow exception to the curl/wget ban.
//
// Attachments are the reason it exists: the YouTrack MCP returns attachment
// metadata and a signed URL but not the bytes, so the only way for the agent to
// read an attached image is to download it. Left unguarded that is an
// exfiltration channel, so the exception requires all of:
//
//   - the curl binary (wget is not accepted, its argument grammar differs);
//   - every URL on https and on the configured YouTrack host;
//   - a path under the configured attachment directory, relative to the worktree;
//   - no flag that uploads data, changes the method, or reads a config file.
func (a *Arbiter) allowCurl(segment string) (bool, string) {
	fields := strings.Fields(segment)
	if len(fields) == 0 || fields[0] != "curl" {
		return false, "only curl is accepted for attachment downloads"
	}
	if len(a.CurlHosts) == 0 {
		return false, "attachment downloads are disabled; ask a human to attach the file to the issue text"
	}
	hosts := make(map[string]bool, len(a.CurlHosts))
	for _, host := range a.CurlHosts {
		hosts[strings.ToLower(host)] = true
	}
	prefix := a.CurlOutputPrefix
	if prefix == "" {
		prefix = DefaultAttachmentPathPrefix
	}

	output := ""
	urls := 0
	for index := 1; index < len(fields); index++ {
		field := strings.Trim(fields[index], `"'`)
		switch {
		case field == "-o" || field == "--output":
			if index+1 >= len(fields) {
				return false, "curl -o needs an output path"
			}
			output = strings.Trim(fields[index+1], `"'`)
			index++
		case strings.HasPrefix(field, "--output="):
			output = strings.Trim(strings.TrimPrefix(field, "--output="), `"'`)
		case strings.HasPrefix(field, "-"):
			if curlUploadFlags[field] {
				return false, fmt.Sprintf("curl flag %s is not allowed (downloads only)", field)
			}
		default:
			parsed, err := url.Parse(field)
			if err != nil || parsed.Host == "" {
				return false, fmt.Sprintf("curl argument %q is not a URL", field)
			}
			if parsed.Scheme != "https" {
				return false, "attachments may only be downloaded over https"
			}
			if !hosts[strings.ToLower(parsed.Hostname())] {
				return false, fmt.Sprintf("curl may only reach %s, not %s", strings.Join(a.CurlHosts, ", "), parsed.Hostname())
			}
			urls++
		}
	}

	if urls == 0 {
		return false, "curl without a URL is not allowed"
	}
	if output == "" {
		return false, fmt.Sprintf("curl must write with -o inside %s/", prefix)
	}
	if filepath.IsAbs(output) {
		return false, fmt.Sprintf("curl must write inside %s/, not to an absolute path", prefix)
	}
	cleaned := filepath.Clean(output)
	if cleaned != filepath.Clean(prefix) && !strings.HasPrefix(cleaned, filepath.Clean(prefix)+string(filepath.Separator)) {
		return false, fmt.Sprintf("curl must write inside %s/, not %s", prefix, output)
	}
	return true, ""
}

func matchesAny(patterns []*regexp.Regexp, segment string) bool {
	for _, pattern := range patterns {
		if pattern != nil && pattern.MatchString(segment) {
			return true
		}
	}
	return false
}

func firstMatch(patterns []*regexp.Regexp, segment string) (bool, string) {
	for _, pattern := range patterns {
		if pattern != nil && pattern.MatchString(segment) {
			return true, pattern.String()
		}
	}
	return false, ""
}
