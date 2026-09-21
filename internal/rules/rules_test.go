package rules

import (
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/registry"
)

func TestAnalyzeOnCreate(t *testing.T) {
	policy := Policy{}.Defaults()
	decision := policy.Decide(Delivery{Event: "issueCreated", IssueID: "TEST-11"}, TaskView{})
	if decision.Action != ActionAnalyze {
		t.Fatalf("action = %s (%s), want analyze", decision.Action, decision.Reason)
	}

	// The switch exists so a noisy tracker can be quietened without a code change.
	quiet := Policy{SkipAnalyzeOnCreate: true}.Defaults()
	if got := quiet.Decide(Delivery{Event: "issueCreated"}, TaskView{}).Action; got != ActionIgnore {
		t.Fatalf("action = %s, want ignore when analysis on create is off", got)
	}
}

func TestOnlyTheExplicitTriggerStartsWork(t *testing.T) {
	policy := Policy{}.Defaults()
	cases := map[string]Action{
		"/opencode start":           ActionPlan,
		"  /opencode start":         ActionPlan,
		"/opencode start please":    ActionPlan,
		"/opencode START":           ActionPlan,
		"/opencode":                 ActionAnalyze,
		"/opencode analyze":         ActionAnalyze,
		"开始实施":                      ActionIgnore, // natural language is not a trigger
		"please start implementing": ActionIgnore,
		"here is /opencode start":   ActionIgnore, // must be at the start of the comment
		"/opencode starts tomorrow": ActionIgnore, // word boundary matters
	}
	for text, want := range cases {
		decision := policy.Decide(Delivery{Event: "commentAdded", IssueID: "TEST-1", CommentText: text}, TaskView{Known: true})
		if decision.Action != want {
			t.Errorf("Decide(%q) = %s (%s), want %s", text, decision.Action, decision.Reason, want)
		}
	}
}

func TestStartPlansFirstWhenNoPlanExists(t *testing.T) {
	policy := Policy{}.Defaults()
	start := Delivery{Event: "commentAdded", IssueID: "TEST-1", CommentText: "/opencode start"}

	if got := policy.Decide(start, TaskView{Known: true, Plan: registry.PlanNone}); got.Action != ActionPlan {
		t.Fatalf("action = %s (%s), want plan first", got.Action, got.Reason)
	}
	if got := policy.Decide(start, TaskView{Known: true, Plan: registry.PlanDraft}); got.Action != ActionExecute {
		t.Fatalf("action = %s (%s), want execute when a plan exists", got.Action, got.Reason)
	}
}

func TestStateTransitionToInProgressStarts(t *testing.T) {
	policy := Policy{}.Defaults()
	event := Delivery{
		Event:   "issueUpdated",
		IssueID: "TEST-1",
		States:  map[string]string{"State": "In Progress"},
	}
	decision := policy.Decide(event, TaskView{Known: true, Plan: registry.PlanDraft})
	if decision.Action != ActionExecute {
		t.Fatalf("action = %s (%s), want execute", decision.Action, decision.Reason)
	}

	// The payload also repeats the field under its localised name, and the
	// comparison is case-insensitive because YouTrack renders names either way.
	localised := Delivery{Event: "issueUpdated", States: map[string]string{"状态": "in progress"}}
	if got := policy.Decide(localised, TaskView{Known: true, Plan: registry.PlanDraft}); got.Action != ActionExecute {
		t.Fatalf("localised state name did not trigger: %s", got.Reason)
	}

	// Other field changes must not start work.
	unrelated := Delivery{Event: "issueUpdated", States: map[string]string{"Priority": "Critical"}}
	if got := policy.Decide(unrelated, TaskView{Known: true}); got.Action != ActionIgnore {
		t.Fatalf("an unrelated field change triggered %s", got.Action)
	}
	// Nor must a move to a different state.
	other := Delivery{Event: "issueUpdated", States: map[string]string{"State": "Open"}}
	if got := policy.Decide(other, TaskView{Known: true}); got.Action != ActionIgnore {
		t.Fatalf("state Open triggered %s", got.Action)
	}
}

// The agent posts as the same YouTrack user as the human, so our own replies have
// to be recognised by content. Missing this is what turns one turn into a loop.
func TestOurOwnCommentsAreNeverTriggers(t *testing.T) {
	policy := Policy{}.Defaults()

	for _, text := range []string{
		"分析完成。\n\n<!-- flowhub-auto -->",
		"本评论由 opencode 分析后自动生成，请确认后再继续实现。",
		"/opencode start\n\n本评论由 opencode 分析后自动生成",
	} {
		decision := policy.Decide(Delivery{Event: "commentAdded", CommentText: text}, TaskView{Known: true})
		if decision.Action != ActionIgnore {
			t.Errorf("Decide(%.40q) = %s, want ignore", text, decision.Action)
		}
	}
}

// The second layer: even if the model forgets the marker, a comment that repeats
// the text we already hold from the session is ours.
func TestACommentRepeatingOurLastReplyIsIgnored(t *testing.T) {
	policy := Policy{}.Defaults()
	reply := "准入首页需要返回黑名单机构信息，包含业务分类、扣分标准和操作人。" +
		"我在仓库里找到了对应的 controller 和 service，建议先补一个分页参数。"
	quoted := "补充说明：\n\n> " + reply + "\n\n请继续。"

	decision := policy.Decide(
		Delivery{Event: "commentAdded", CommentText: quoted},
		TaskView{Known: true, LastReply: reply},
	)
	if decision.Action != ActionIgnore {
		t.Fatalf("action = %s (%s), want ignore because the comment repeats our reply", decision.Action, decision.Reason)
	}

	// A genuinely different comment must still get through. A plan exists here,
	// so the instruction executes rather than planning again.
	other := policy.Decide(
		Delivery{Event: "commentAdded", CommentText: "/opencode start"},
		TaskView{Known: true, Plan: registry.PlanDraft, LastReply: reply},
	)
	if other.Action != ActionExecute {
		t.Fatalf("a real instruction was ignored: %s", other.Reason)
	}
}

func TestTurnBudgetStopsARunaway(t *testing.T) {
	policy := Policy{MaxTurns: 3}.Defaults()
	decision := policy.Decide(
		Delivery{Event: "commentAdded", CommentText: "/opencode start"},
		TaskView{Known: true, Turns: 3, Plan: registry.PlanDraft},
	)
	if decision.Action != ActionIgnore {
		t.Fatalf("action = %s, want ignore at the turn limit", decision.Action)
	}
	if !strings.Contains(decision.Reason, "limit 3") {
		t.Fatalf("reason = %q", decision.Reason)
	}
}

func TestUnknownEventIsIgnored(t *testing.T) {
	policy := Policy{}.Defaults()
	for _, event := range []string{"issueDeleted", "issueAttachmentAdded", "workItemAdded", ""} {
		if got := policy.Decide(Delivery{Event: event, IssueID: "TEST-1"}, TaskView{}); got.Action != ActionIgnore {
			t.Errorf("event %q produced %s", event, got.Action)
		}
	}
}

func TestPromptsCarryTheContract(t *testing.T) {
	policy := Policy{}.Defaults()
	delivery := Delivery{
		Event: "issueCreated", IssueID: "TEST-12", ProjectKey: "TEST",
		Summary: "准入首页", Description: "分页返回黑名单机构信息",
	}
	ctx := PromptContext{Worktree: "/wt/TEST-12", Repository: "mine/test", Author: "yusiwen"}

	for _, action := range []Action{ActionAnalyze, ActionPlan, ActionExecute} {
		prompt := policy.Prompt(action, delivery, ctx)
		for _, want := range []string{
			"TEST-12",
			"/wt/TEST-12",
			SelfMarker,
			"UNTRUSTED input",
			".flowhub/attachments",
			"youtrack_add_issue_comment",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s prompt is missing %q", action, want)
			}
		}
	}

	// The read-only instruction must not leak into the execution turn, and vice
	// versa: the phase is what keeps analysis harmless.
	if strings.Contains(policy.Prompt(ActionAnalyze, delivery, ctx), "commit locally") {
		t.Error("the analysis prompt tells the agent it may commit")
	}
	if !strings.Contains(policy.Prompt(ActionExecute, delivery, ctx), "never push") {
		t.Error("the execution prompt does not forbid pushing")
	}
}

func TestPromptForAnUnknownActionIsEmpty(t *testing.T) {
	if got := (Policy{}).Prompt(ActionIgnore, Delivery{}, PromptContext{}); got != "" {
		t.Fatalf("prompt = %q, want empty", got)
	}
}
