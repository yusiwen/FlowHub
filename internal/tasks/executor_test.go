package tasks

import (
	"errors"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
)

// TestTheFollowUpIsBoundedToACleanFinish pins the two guards around the follow-up.
//
// It only fires for a turn that ended cleanly and posted nothing: an error, a
// FirstResponse failure and a timeout each have their own state, and re-prompting a
// session whose prompt the server rejected would fail again for the same reason. And
// its deadline is a fraction of the ordinary one, so a single delivery cannot hold a
// worker for two full turn budgets.
func TestTheFollowUpIsBoundedToACleanFinish(t *testing.T) {
	clean := agent.Result{Finished: true, SessionID: "ses_1"}
	cases := map[string]struct {
		result  agent.Result
		err     error
		replied bool
		want    bool
	}{
		"finished without a comment": {clean, nil, false, true},
		"finished with a comment":    {clean, nil, true, false},
		"errored":                    {clean, errors.New("boom"), false, false},
		"never finished":             {agent.Result{SessionID: "ses_1"}, nil, false, false},
		"timed out":                  {agent.Result{Finished: true, TimedOut: true, SessionID: "ses_1"}, nil, false, false},
		"no session to continue":     {agent.Result{Finished: true}, nil, false, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := silentFinish(tc.result, tc.err, tc.replied); got != tc.want {
				t.Fatalf("silentFinish = %t, want %t", got, tc.want)
			}
		})
	}

	if got := nudgeDeadline(15 * time.Minute); got != 5*time.Minute {
		t.Fatalf("nudgeDeadline(15m) = %s, want 5m", got)
	}
	if got := nudgeDeadline(3 * time.Minute); got != MinNudgeDeadline {
		t.Fatalf("nudgeDeadline(3m) = %s, want the %s floor", got, MinNudgeDeadline)
	}
	if got := nudgeDeadline(0); got != MinNudgeDeadline {
		t.Fatalf("nudgeDeadline(0) = %s, want the %s floor", got, MinNudgeDeadline)
	}
	// The floor never raises the bound past the deployment's own budget: a host whose
	// turns get one second does not hand its follow-up two minutes.
	if got := nudgeDeadline(time.Second); got != time.Second {
		t.Fatalf("nudgeDeadline(1s) = %s, want 1s", got)
	}
}

func TestReplyToolNamesFromV2MCPAreNormalized(t *testing.T) {
	calls := []agent.ToolCall{{Name: "youtrack.add_issue_comment", Status: "completed"}}
	if !anyToolCompleted(calls, []string{"youtrack_add_issue_comment"}) {
		t.Fatal("a dotted v2 MCP name must count as the source's underscore reply tool")
	}
	if anyToolCompleted([]agent.ToolCall{{Name: "youtrack.add_issue_comment", Status: "error"}}, []string{"youtrack_add_issue_comment"}) {
		t.Fatal("a failed call must not count as a reply")
	}
}

// TestAPlanCountsOnlyWhenTheReadPrecedesTheReply pins the order the gate depends
// on. Reading the discussion after posting the plan is too late: the comment the
// maintainer sees was written without it. A corrected reply after a read does
// count, which is what makes the bounded follow-up useful.
func TestAPlanCountsOnlyWhenTheReadPrecedesTheReply(t *testing.T) {
	const read = "youtrack_get_issue_comments"
	const reply = "youtrack_add_issue_comment"
	completed := func(name string) agent.ToolCall {
		return agent.ToolCall{Name: name, Status: "completed"}
	}
	cases := map[string]struct {
		calls []agent.ToolCall
		reads []string
		want  bool
	}{
		"read then reply":            {[]agent.ToolCall{completed(read), completed(reply)}, []string{read}, true},
		"reply then read":            {[]agent.ToolCall{completed(reply), completed(read)}, []string{read}, false},
		"read only":                  {[]agent.ToolCall{completed(read)}, []string{read}, false},
		"reply only":                 {[]agent.ToolCall{completed(reply)}, []string{read}, false},
		"failed read then reply":     {[]agent.ToolCall{{Name: read, Status: "error"}, completed(reply)}, []string{read}, false},
		"read then failed reply":     {[]agent.ToolCall{completed(read), {Name: reply, Status: "error"}}, []string{read}, false},
		"corrected reply after read": {[]agent.ToolCall{completed(reply), completed(read), completed(reply)}, []string{read}, true},
		"dotted v2 names":            {[]agent.ToolCall{completed("youtrack.get_issue_comments"), completed("youtrack.add_issue_comment")}, []string{read}, true},
		"no read requirement":        {[]agent.ToolCall{completed(reply)}, nil, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := planReadSatisfied(tc.calls, tc.reads, []string{reply}); got != tc.want {
				t.Fatalf("planReadSatisfied = %t, want %t", got, tc.want)
			}
		})
	}
}
