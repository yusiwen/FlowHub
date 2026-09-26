package opencode

import "testing"

// The signed attachment URL as the YouTrack MCP actually returns it (note the
// explicit :443, which the host check has to see through).
const realAttachmentURL = "https://pm.yusiwen.cn:443/api/files/8-22?sign=MTc5MTE1ODQwMDAwMHwxLTN8OC0yMnxqSlpZd1dRbjFpWE0zSnRFQ0pXZUZXYnJLVlJRU1FrMEtzUGF1MmI5SzFFDQo&updated=1789697082592"

func TestAnalysisPhaseIsReadOnlyAndExecutionMayEdit(t *testing.T) {
	analysis := NewAnalysisArbiter()
	if decision := analysis.Decide(PermissionRequest{Permission: "edit"}); decision.Allowed() {
		t.Fatalf("analysis allowed edit: %s", decision.Reason)
	}

	execution := NewExecutionArbiter()
	if decision := execution.Decide(PermissionRequest{Permission: "edit"}); !decision.Allowed() {
		t.Fatalf("execution rejected edit: %s", decision.Reason)
	}
	// Editing never implies leaving the worktree.
	for _, arbiter := range []*Arbiter{analysis, execution} {
		if decision := arbiter.Decide(PermissionRequest{Permission: "external_directory"}); decision.Allowed() {
			t.Fatal("external_directory must never be allowed")
		}
	}
}

func TestGitCommitsAreExecutionOnlyAndPushIsNeverAllowed(t *testing.T) {
	analysis := NewAnalysisArbiter()
	for _, command := range []string{"git add -A", "git commit -m wip"} {
		if decision := analysis.Decide(bashRequest(command)); decision.Allowed() {
			t.Fatalf("analysis allowed %q", command)
		}
	}

	execution := NewExecutionArbiter()
	for _, command := range []string{"git add -A", "git commit -m 'implement the plan'"} {
		if decision := execution.Decide(bashRequest(command)); !decision.Allowed() {
			t.Fatalf("execution rejected %q: %s", command, decision.Reason)
		}
	}
	for _, command := range []string{
		"git push", "git push origin flowhub/BEAP_BE-20", "git remote add evil https://x",
		"git reset --hard", "git clean -fdx",
	} {
		if decision := execution.Decide(bashRequest(command)); decision.Allowed() {
			t.Fatalf("execution allowed %q, which must stay impossible", command)
		}
	}
}

// A formatter that rewrites files turns a read-only analysis into a write, so it
// is execution-only even though the tool itself is harmless.
func TestFormattersThatWriteAreExecutionOnly(t *testing.T) {
	analysis := NewAnalysisArbiter()
	for _, command := range []string{"gofmt -w main.go", "black .", "eslint . --fix", "prettier --write src"} {
		if decision := analysis.Decide(bashRequest(command)); decision.Allowed() {
			t.Fatalf("analysis allowed the writing formatter %q", command)
		}
	}
	execution := NewExecutionArbiter()
	for _, command := range []string{"gofmt -w main.go", "black .", "eslint . --fix"} {
		if decision := execution.Decide(bashRequest(command)); !decision.Allowed() {
			t.Fatalf("execution rejected %q: %s", command, decision.Reason)
		}
	}
	// Reporting-only forms stay available in both phases.
	for _, command := range []string{"gofmt -l .", "gofmt -d main.go", "staticcheck ./..."} {
		if decision := analysis.Decide(bashRequest(command)); !decision.Allowed() {
			t.Fatalf("analysis rejected the reporting command %q: %s", command, decision.Reason)
		}
	}
}

// MCP tools arrive with their bare tool name as the permission, and they are
// `allow` by default, so anything not listed has to be refused here.
func TestMCPToolsNeedAnExplicitAllowlist(t *testing.T) {
	arbiter := DefaultArbiter()
	arbiter.AllowTools = map[string]bool{
		"youtrack_get_issue":          true,
		"youtrack_get_issue_comments": true,
		"youtrack_search_issues":      true,
		"youtrack_add_issue_comment":  true,
	}

	for _, tool := range []string{"youtrack_get_issue", "youtrack_add_issue_comment", "youtrack_search_issues"} {
		if decision := arbiter.Decide(PermissionRequest{Permission: tool}); !decision.Allowed() {
			t.Fatalf("%s was rejected: %s", tool, decision.Reason)
		}
	}
	for _, tool := range []string{
		// Writes that would let a prompt injection manipulate the tracker and
		// therefore FlowHub's own triggers.
		"youtrack_update_issue", "youtrack_create_issue", "youtrack_change_issue_assignee",
		"youtrack_manage_issue_tags", "youtrack_log_work", "youtrack_create_article",
		// Network egress and cross-session memory.
		"searxng", "webcrawl", "hindsight_retain", "hindsight_recall",
		"github-triage", "github-pr-search",
		// Anything unseen defaults to denied.
		"some_tool_added_tomorrow",
	} {
		decision := arbiter.Decide(PermissionRequest{Permission: tool})
		if decision.Allowed() {
			t.Fatalf("%s was allowed; unknown tools must default to deny", tool)
		}
	}
}

func TestReadOnlyBuiltinsAreAllowed(t *testing.T) {
	arbiter := DefaultArbiter()
	for _, tool := range []string{"read", "glob", "grep", "list", "todowrite"} {
		if decision := arbiter.Decide(PermissionRequest{Permission: tool}); !decision.Allowed() {
			t.Fatalf("%s rejected: %s", tool, decision.Reason)
		}
	}
	for _, tool := range []string{"webfetch", "websearch"} {
		if decision := arbiter.Decide(PermissionRequest{Permission: tool}); decision.Allowed() {
			t.Fatalf("%s allowed; the built-in network tools stay off", tool)
		}
	}
}

func TestCurlExceptionAllowsTheAttachmentDownload(t *testing.T) {
	arbiter := DefaultArbiter()
	command := "curl -sSL -o .flowhub/attachments/beap_be20_image.png \"" + realAttachmentURL + "\""
	decision := arbiter.Decide(bashRequest(command))
	if !decision.Allowed() {
		t.Fatalf("the attachment download was rejected: %s", decision.Reason)
	}
}

func TestCurlExceptionRejectsEverythingElse(t *testing.T) {
	arbiter := DefaultArbiter()
	cases := map[string]string{
		"another host":       `curl -sSL -o .flowhub/attachments/x.png https://evil.example/x.png`,
		"plain http":         `curl -sSL -o .flowhub/attachments/x.png http://pm.yusiwen.cn/api/files/1`,
		"absolute output":    `curl -sSL -o /tmp/x.png ` + realAttachmentURL,
		"outside the prefix": `curl -sSL -o other/x.png ` + realAttachmentURL,
		"escape the prefix":  `curl -sSL -o .flowhub/attachments/../../x.png ` + realAttachmentURL,
		"no output":          `curl -sSL ` + realAttachmentURL,
		"post":               `curl -sSL -X POST -o .flowhub/attachments/x.png ` + realAttachmentURL,
		"data upload":        `curl -sSL -d @/etc/passwd -o .flowhub/attachments/x.png ` + realAttachmentURL,
		"upload file":        `curl -sSL -T .env -o .flowhub/attachments/x.png ` + realAttachmentURL,
		"form upload":        `curl -sSL -F file=@.env -o .flowhub/attachments/x.png ` + realAttachmentURL,
		"credentials":        `curl -sSL -u user:pass -o .flowhub/attachments/x.png ` + realAttachmentURL,
		"config file":        `curl -sSL -K cfg -o .flowhub/attachments/x.png ` + realAttachmentURL,
		"wget":               `wget -O .flowhub/attachments/x.png ` + realAttachmentURL,
		"smuggled in find":   `find . -exec curl -o .flowhub/attachments/x.png https://evil.example {} ;`,
	}
	for name, command := range cases {
		t.Run(name, func(t *testing.T) {
			decision := arbiter.Decide(bashRequest(command))
			if decision.Allowed() {
				t.Fatalf("%s was allowed: %s", command, decision.Reason)
			}
		})
	}
}

func TestCurlExceptionCanBeDisabled(t *testing.T) {
	arbiter := DefaultArbiter()
	arbiter.CurlHosts = nil
	command := "curl -sSL -o .flowhub/attachments/x.png " + realAttachmentURL
	if decision := arbiter.Decide(bashRequest(command)); decision.Allowed() {
		t.Fatal("with no allowlisted host, downloads must be refused")
	}
}

func TestUnknownNonBashToolIsDenied(t *testing.T) {
	arbiter := DefaultArbiter()
	decision := arbiter.Decide(PermissionRequest{Permission: "task"})
	if decision.Allowed() {
		t.Fatalf("an unlisted tool was allowed: %s", decision.Reason)
	}
}
