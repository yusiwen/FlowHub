package opencode

import "testing"

func bashRequest(command string, patterns ...string) PermissionRequest {
	if len(patterns) == 0 {
		patterns = []string{command}
	}
	return PermissionRequest{
		ID:         "per_1",
		SessionID:  "ses_1",
		Permission: "bash",
		Patterns:   patterns,
		Metadata:   PermissionMetadata{Command: command},
	}
}

func TestArbiterAllowsReadOnlyCommands(t *testing.T) {
	arbiter := DefaultArbiter()
	for _, command := range []string{
		"pwd",
		"ls -la",
		"cat README.md",
		"git status --short",
		"git log --oneline -5",
		"git diff HEAD",
		"go test ./...",
		"go vet ./...",
		"make test",
		"grep -rn foo internal/",
		"wc -l main.go",
		"find . -name '*.go'",
	} {
		decision := arbiter.Decide(bashRequest(command))
		if !decision.Allowed() {
			t.Errorf("Decide(%q) = %s (%s), want allowed", command, decision.Reply, decision.Reason)
		}
	}
}

// The live trial showed that opencode sends a compound shell line as ONE request:
// patterns=["pwd","ls -la"] with metadata.command="pwd && ls -la".
func TestArbiterAllowsCompoundReadOnlyCommands(t *testing.T) {
	arbiter := DefaultArbiter()
	decision := arbiter.Decide(bashRequest("pwd && ls -la", "pwd", "ls -la"))
	if !decision.Allowed() {
		t.Fatalf("compound read-only command rejected: %s", decision.Reason)
	}
	if decision.Reply != ReplyOnce {
		t.Fatalf("reply = %s, want once by default", decision.Reply)
	}
}

func TestArbiterRejectsDangerousAndWideningCommands(t *testing.T) {
	arbiter := DefaultArbiter()
	cases := map[string]string{
		"rm -rf /":                    "deny list",
		"sudo cat /etc/shadow":        "deny list",
		"curl https://example.com":    "curl may only reach",
		"git push origin master":      "deny list",
		"git commit -m x":             "deny list",
		"npm install left-pad":        "deny list",
		"sh -c 'rm -rf /'":            "deny list",
		"pwd && rm -rf /":             "deny list",
		"ls > /tmp/out":               "redirection",
		"ls >> /tmp/out":              "redirection",
		"cat /etc/passwd < /dev/null": "redirection",
		"echo `whoami`":               "backticks",
		"echo $(whoami)":              "command substitution",
		"sleep 5 &":                   "background",
		"python3 -c 'print(1)'":       "deny list",
		"cat ~/.ssh/id_rsa":           "outside the task worktree",
		"tail -f /var/log/system.log": "outside the task worktree",
		"cat /etc/passwd":             "outside the task worktree",
		"ls /Users/yusiwen/.config":   "outside the task worktree",
	}
	for command, wantReason := range cases {
		decision := arbiter.Decide(bashRequest(command))
		if decision.Allowed() {
			t.Errorf("Decide(%q) allowed, want rejected", command)
			continue
		}
		if decision.Reply != ReplyReject {
			t.Errorf("Decide(%q) = %s, want reject", command, decision.Reply)
		}
		if wantReason != "" && !contains(decision.Reason, wantReason) {
			t.Errorf("Decide(%q) reason = %q, want it to mention %q", command, decision.Reason, wantReason)
		}
	}
}

func TestArbiterRejectsWhenPatternsDisagreeWithTheCommand(t *testing.T) {
	arbiter := DefaultArbiter()
	// The command looks fine but one of the request's own patterns does not.
	decision := arbiter.Decide(bashRequest("ls -la", "ls -la", "rm -rf /"))
	if decision.Allowed() {
		t.Fatal("a pattern that is not allowlisted must block the request")
	}
}

func TestArbiterRejectsEmptyCommand(t *testing.T) {
	arbiter := DefaultArbiter()
	decision := arbiter.Decide(PermissionRequest{ID: "per_1", SessionID: "ses_1", Permission: "bash"})
	if decision.Allowed() {
		t.Fatal("an empty command must never be allowed")
	}
}

func TestArbiterHandlesNonBashPermissions(t *testing.T) {
	arbiter := DefaultArbiter()
	if decision := arbiter.Decide(PermissionRequest{Permission: "read"}); !decision.Allowed() {
		t.Fatalf("read rejected: %s", decision.Reason)
	}
	for _, kind := range []string{"edit", "external_directory", "webfetch", "websearch", "question"} {
		decision := arbiter.Decide(PermissionRequest{Permission: kind})
		if decision.Allowed() {
			t.Errorf("permission kind %q allowed, want rejected", kind)
		}
	}
}

func TestArbiterRemembersOnlyWhenAsked(t *testing.T) {
	remembering := DefaultArbiter()
	remembering.Remember = true
	if got := remembering.Decide(bashRequest("pwd")).Reply; got != ReplyAlways {
		t.Fatalf("reply = %s, want always when Remember is set", got)
	}

	var nilArbiter *Arbiter
	if decision := nilArbiter.Decide(bashRequest("pwd")); decision.Allowed() {
		t.Fatal("a nil arbiter must deny everything")
	}
}

func TestSplitSegments(t *testing.T) {
	got := splitSegments("pwd && ls -la || echo hi ; git status | wc -l")
	want := []string{"pwd", "ls -la", "echo hi", "git status", "wc -l"}
	if len(got) != len(want) {
		t.Fatalf("splitSegments = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("splitSegments = %v, want %v", got, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
