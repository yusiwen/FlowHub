package opencode

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
)

// wrappedArbiter is the read-only policy of a host whose plugin rewrites `git …`
// into `rtk git …` — the shape measured on 2026-10-09, when every read-only git
// inspection of one turn was refused and the turn ended with no comment.
func wrappedArbiter() *Arbiter {
	arbiter := DefaultArbiter()
	arbiter.Wrappers = []string{"rtk"}
	return arbiter
}

func TestStripWrappers(t *testing.T) {
	arbiter := wrappedArbiter()
	cases := []struct{ segment, target, wrapper string }{
		{"rtk git status", "git status", "rtk"},
		{"rtk git log --oneline -5", "git log --oneline -5", "rtk"},
		// A wrapper in front of a command the policy never allowed is still
		// stripped: the remainder is what gets judged, not the wrapper.
		{"rtk rm -rf /", "rm -rf /", "rtk"},
		// Repeated wrapping terminates instead of looping.
		{"rtk rtk git status", "git status", "rtk"},
		{"git status", "git status", ""},
		{"rtk", "rtk", ""},                             // a bare wrapper is never stripped to nothing
		{"rtk", "rtk", ""},                             // and an empty target must never be judged
		{"ls", "ls", ""},                               // an allowlisted command needs no wrapper
		{"rtkctl git status", "rtkctl git status", ""}, // a longer name is not the wrapper
	}
	for _, tc := range cases {
		target, wrapper := arbiter.stripWrappers(tc.segment)
		if target != tc.target || wrapper != tc.wrapper {
			t.Errorf("stripWrappers(%q) = (%q, %q), want (%q, %q)",
				tc.segment, target, wrapper, tc.target, tc.wrapper)
		}
	}
}

// TestTheDefaultArbiterStripsNothing is the control for every other test here: the
// feature is opt-in, and a host that declares no wrapper behaves exactly as before.
func TestTheDefaultArbiterStripsNothing(t *testing.T) {
	if got := DefaultArbiter().Wrappers; len(got) != 0 {
		t.Fatalf("DefaultArbiter().Wrappers = %v, want none", got)
	}
	decision := DefaultArbiter().Decide(bashRequest("rtk git status", "rtk git status"))
	if decision.Allowed() {
		t.Fatalf("an undeclared wrapper was trusted: %s", decision.Reason)
	}
}

func TestAConfiguredWrapperIsJudgedWithoutIt(t *testing.T) {
	arbiter := wrappedArbiter()
	for _, command := range []string{
		"rtk git status",
		"rtk git status --porcelain=v1 -uall",
		"rtk git log --oneline -5",
		"rtk git diff --stat",
		"rtk git show HEAD",
		"rtk git branch --show-current",
		"rtk ls -la",
	} {
		decision := arbiter.Decide(bashRequest(command))
		if !decision.Allowed() {
			t.Errorf("Decide(%q) = %s (%s), want allowed", command, decision.Reply, decision.Reason)
		}
	}

	// A compound line is stripped segment by segment, and the request's own patterns
	// are the rewritten segments, so they have to be stripped the same way.
	command := `rtk git status && echo "---BRANCH---" && rtk git log --oneline -5`
	decision := arbiter.Decide(bashRequest(command,
		"rtk git status", `echo "---BRANCH---"`, "rtk git log --oneline -5"))
	if !decision.Allowed() {
		t.Fatalf("a compound wrapped command was rejected: %s", decision.Reason)
	}
}

// The wrapper is trusted; what it wraps is not. Each of these is refused by the same
// lists that refuse the unwrapped spelling, which is what keeps the wrapper list from
// widening the policy rather than merely relabelling it.
func TestAWrapperCannotSmuggleADeniedCommand(t *testing.T) {
	arbiter := wrappedArbiter()
	cases := map[string]string{
		"rtk rm -rf /":                  "deny list",
		"rtk sudo cat /etc/shadow":      "deny list",
		"rtk git push origin master":    "deny list",
		"rtk git commit -m x":           "deny list",
		"rtk npm install left-pad":      "deny list",
		"rtk sh -c 'rm -rf /'":          "deny list",
		"rtk ls > /tmp/out":             "redirection",
		"rtk echo $(whoami)":            "command substitution",
		"rtk curl https://example.com":  "deny list",
		"rtk cat /etc/passwd":           "outside the task worktree",
		"rtk git status && rm -rf /":    "deny list",
		"rtk git status | rtk rm -rf /": "deny list",
		"rtk python3 -c 'print(1)'":     "deny list",
	}
	for command, wantReason := range cases {
		decision := arbiter.Decide(bashRequest(command))
		if decision.Allowed() {
			t.Errorf("Decide(%q) allowed, want rejected", command)
			continue
		}
		if wantReason != "" && !contains(decision.Reason, wantReason) {
			t.Errorf("Decide(%q) reason = %q, want it to mention %q", command, decision.Reason, wantReason)
		}
	}
}

// A wrapper changes which commands are permitted, never which phase they run in:
// `git add` stays an execution-phase command with or without `rtk` in front of it.
func TestAWrapperKeepsThePhaseBoundary(t *testing.T) {
	analysis := wrappedArbiter()
	if decision := analysis.Decide(bashRequest("rtk git add -A")); decision.Allowed() {
		t.Fatal("a read-only turn allowed a wrapped commit-staging command")
	}

	execution := NewExecutionArbiter()
	execution.Wrappers = []string{"rtk"}
	if decision := execution.Decide(bashRequest("rtk git add -A")); !decision.Allowed() {
		t.Fatalf("the execution phase refused a wrapped `git add`: %s", decision.Reason)
	}
	if decision := execution.Decide(bashRequest("rtk git commit -m x")); !decision.Allowed() {
		t.Fatalf("the execution phase refused a wrapped `git commit`: %s", decision.Reason)
	}
	if decision := execution.Decide(bashRequest("rtk git push origin master")); decision.Allowed() {
		t.Fatal("a wrapped push was allowed")
	}
}

func TestAWrappedPatternStillHasToAgreeWithTheCommand(t *testing.T) {
	arbiter := wrappedArbiter()
	// The command is fine, but one of the request's own patterns is not.
	decision := arbiter.Decide(bashRequest("rtk git status", "rtk git status", "rtk rm -rf /"))
	if decision.Allowed() {
		t.Fatalf("a rewritten pattern that is not allowlisted was accepted: %s", decision.Reason)
	}
}

// The diagnostic half. Nothing is configured here, so the command is refused — but
// the reason has to name the wrapper and the command it hides, because the model
// never sees the rewrite and "rephrase using inspection commands" sends it into a
// retry loop that is rewritten identically every time.
func TestAnUndeclaredWrapperIsNamedInTheRefusal(t *testing.T) {
	decision := DefaultArbiter().Decide(bashRequest("rtk git status && rtk git log --oneline -5"))
	if decision.Allowed() {
		t.Fatal("an undeclared wrapper must not be trusted")
	}
	for _, want := range []string{"rtk", "git status", "rewritten", "shell_wrappers"} {
		if !contains(decision.Reason, want) {
			t.Fatalf("reason = %q, want it to mention %q", decision.Reason, want)
		}
	}

	// The control: an ordinary unknown command is refused with the plain reason and
	// is not dressed up as a wrapper. A refusal that cries "wrapper" everywhere would
	// be as unactionable as the original.
	plain := DefaultArbiter().Decide(bashRequest("gotcha nonsense"))
	if plain.Allowed() {
		t.Fatal("an unknown command must be refused")
	}
	if contains(plain.Reason, "wrapper") {
		t.Fatalf("an ordinary refusal mentions a wrapper: %q", plain.Reason)
	}

	// And a wrapped command whose payload is denied is refused on the payload, not
	// presented as something the operator could simply trust.
	denied := DefaultArbiter().Decide(bashRequest("rtk git push origin master"))
	if denied.Allowed() || contains(denied.Reason, "but") {
		t.Fatalf("a wrapped denied command was reported as a wrapper candidate: %q", denied.Reason)
	}
}

func TestAnUndeclaredWrapperIsAlsoNamedInTheExecutionPhase(t *testing.T) {
	arbiter := NewExecutionArbiter()
	if decision := arbiter.Decide(bashRequest("rtk git add -A")); decision.Allowed() {
		t.Fatal("an undeclared wrapper must not be trusted in the execution phase either")
	}
	// In the execution phase the payload *is* permitted, so the refusal has to say
	// that the wrapper is the whole problem.
	decision := arbiter.Decide(bashRequest("rtk git add -A"))
	for _, want := range []string{"rtk", "git add -A", "wrapper"} {
		if !contains(decision.Reason, want) {
			t.Fatalf("reason = %q, want it to mention %q", decision.Reason, want)
		}
	}
}

func TestValidateShellWrappers(t *testing.T) {
	for _, names := range [][]string{
		nil,
		{"rtk"},
		{"rtk", "my-tool"},
		{"_hidden"},
		{"tool.local"},
		{"c++"},
		{"x1"},
	} {
		if err := ValidateShellWrappers(names); err != nil {
			t.Errorf("ValidateShellWrappers(%v) = %v, want accepted", names, err)
		}
	}

	cases := map[string][]string{
		"an empty entry":        {""},
		"a space":               {"rtk git"},
		"an absolute path":      {"/usr/bin/rtk"},
		"a relative path":       {"bin/rtk"},
		"a command separator":   {"rtk;rm"},
		"a pipeline":            {"rtk|x"},
		"a leading dash":        {"-x"},
		"a leading dot":         {".x"},
		"a redirection":         {"rtk>out"},
		"a variable reference":  {"$RTK"},
		"a quoted name":         {`"rtk"`},
		"a duplicate":           {"rtk", "rtk"},
		"more than the maximum": make([]string, MaxShellWrappers+1),
	}
	for name, names := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateShellWrappers(names); err == nil {
				t.Fatalf("ValidateShellWrappers(%v) was accepted", names)
			}
		})
	}

	// The refusal has to name the entry, or the operator has to guess which line of a
	// list is wrong.
	err := ValidateShellWrappers([]string{"rtk", "rtk git"})
	if err == nil || !strings.Contains(err.Error(), `"rtk git"`) {
		t.Fatalf("the refusal does not name the bad entry: %v", err)
	}
}

// TestTheRuntimeGivesItsShellWrappersToTheArbiter is the wiring half, end to end
// through Runtime.Run against a fake server: the operator declares the wrappers on the
// runtime, so a turn on that runtime must answer the rewritten permission request with
// "once". A list that never reaches the arbiter would be a silently inert setting —
// the same class of failure as the read-only `prompt_file` that parsed and did nothing.
func TestTheRuntimeGivesItsShellWrappersToTheArbiter(t *testing.T) {
	fake := newFakeServer(t)
	fake.directory = "/private/tmp/work"
	server := fake.start()
	defer server.Close()

	fake.addPending(PermissionRequest{
		ID: "per_1", SessionID: fake.sessionID, Permission: "bash",
		Patterns: []string{"rtk git status"},
		Metadata: PermissionMetadata{Command: "rtk git status"},
	})
	fake.afterReply = func(f *fakeServer) { f.finishWith("the tree is clean") }

	runtime := NewRuntime(
		New(Options{BaseURL: server.URL, Timeout: 10 * time.Second}),
		RuntimeOptions{
			Name:          "local",
			Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
			ShellWrappers: []string{"rtk"},
		},
	)
	result, err := runtime.Run(context.Background(), agent.Turn{
		Directory: "/private/tmp/work",
		Prompt:    "check the git status",
		Agent:     "devops",
		Title:     "BEAP_BE-46",
		Phase:     agent.PhaseAnalysis,
		Deadline:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Permissions) != 1 {
		t.Fatalf("permissions = %+v, want one answered", result.Permissions)
	}
	if got := result.Permissions[0].Reply; got != string(ReplyOnce) {
		t.Fatalf("the rewritten command was answered %s (%s), want once", got, result.Permissions[0].Reason)
	}
	if got := fake.replies["per_1"]; got != ReplyOnce {
		t.Fatalf("server saw %s, want once", got)
	}

	// The control: the same turn without the declaration refuses it. Without this the
	// test would pass for a runtime that trusts every prefix.
	undeclared := newFakeServer(t)
	undeclared.directory = "/private/tmp/work"
	undeclaredServer := undeclared.start()
	defer undeclaredServer.Close()
	undeclared.addPending(PermissionRequest{
		ID: "per_1", SessionID: undeclared.sessionID, Permission: "bash",
		Patterns: []string{"rtk git status"},
		Metadata: PermissionMetadata{Command: "rtk git status"},
	})
	undeclared.afterReply = func(f *fakeServer) { f.finishWith("blocked") }

	plain := NewRuntime(
		New(Options{BaseURL: undeclaredServer.URL, Timeout: 10 * time.Second}),
		RuntimeOptions{Name: "local", Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
	)
	plainResult, err := plain.Run(context.Background(), agent.Turn{
		Directory: "/private/tmp/work",
		Prompt:    "check the git status",
		Agent:     "devops",
		Title:     "BEAP_BE-46",
		Phase:     agent.PhaseAnalysis,
		Deadline:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(plainResult.Permissions) != 1 || plainResult.Permissions[0].Reply != string(ReplyReject) {
		t.Fatalf("permissions = %+v, want one rejection", plainResult.Permissions)
	}
}
