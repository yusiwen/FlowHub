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
			"youtrack_get_issue",
			"UNTRUSTED",
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

	// A read-only turn has to hand the maintainer the exact next step, and that
	// step has to be the configured trigger rather than a hardcoded phrase.
	custom := Policy{Trigger: "/flowhub go", StartStates: []string{"Doing"}}.Defaults()
	analysis := custom.Prompt(ActionAnalyze, delivery, ctx)
	if !strings.Contains(analysis, "/flowhub go") || !strings.Contains(analysis, "Doing") {
		t.Errorf("the analysis prompt does not name the configured trigger:\n%s", analysis)
	}
	if strings.Contains(custom.Prompt(ActionExecute, delivery, ctx), "/flowhub go") {
		t.Error("the execution prompt must not tell the maintainer how to start")
	}
}

func TestPromptForAnUnknownActionIsEmpty(t *testing.T) {
	if got := (Policy{}).Prompt(ActionIgnore, Delivery{}, PromptContext{}); got != "" {
		t.Fatalf("prompt = %q, want empty", got)
	}
}

func TestBasisNamesTheRealTrigger(t *testing.T) {
	policy := Policy{}.Defaults()

	comment := Delivery{
		Event: "commentAdded", IssueID: "TEST-20", ProjectKey: "TEST",
		CommentText: "/opencode start\n\n请开始实施",
	}
	state := Delivery{
		Event: "issueUpdated", IssueID: "TEST-20", ProjectKey: "TEST",
		States: map[string]string{"State": "In Progress"},
	}
	cases := map[string]struct {
		decision Decision
		delivery Delivery
		want     string
	}{
		"created": {Decision{Trigger: TriggerCreated}, Delivery{Event: "issueCreated"}, "the creation of this issue"},
		"comment": {Decision{Trigger: TriggerComment}, comment, `the comment "/opencode start 请开始实施"`},
		"state":   {Decision{Trigger: TriggerState}, state, `the state change of State to "In Progress"`},
		"unknown": {Decision{}, Delivery{}, "a FlowHub automation trigger"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := policy.Basis(tc.decision, tc.delivery); got != tc.want {
				t.Fatalf("Basis = %q, want %q", got, tc.want)
			}
		})
	}

	// A long comment must not be copied into the sign-off whole.
	long := Delivery{CommentText: strings.Repeat("很长的一段话", 40)}
	if got := policy.Basis(Decision{Trigger: TriggerComment}, long); len([]rune(got)) > 120 {
		t.Fatalf("Basis copied too much of the comment: %d runes", len([]rune(got)))
	}
}

func TestPromptRequiresABlockquoteSignOff(t *testing.T) {
	policy := Policy{}.Defaults()
	delivery := Delivery{Event: "issueCreated", IssueID: "TEST-20", ProjectKey: "TEST", Summary: "s"}
	decision := Decision{Action: ActionAnalyze, Trigger: TriggerCreated}
	ctx := PromptContext{
		Worktree: "/wt/TEST-20", Repository: "mine/test", Author: "yusiwen",
		Basis: policy.Basis(decision, delivery),
	}
	prompt := policy.Prompt(ActionAnalyze, delivery, ctx)

	for _, want := range []string{
		"> This comment was generated automatically by opencode, from the creation of this issue.",
		"> " + SelfMarker,
		"blockquote",
		"in the language of the issue",
		"The final line of the comment must be `> " + SelfMarker + "`",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}

	// The sign-off is the last element: the "how to proceed" line has to come
	// before it, or the marker stops being the final line of the reply.
	proceed := strings.Index(prompt, "how to proceed")
	signoff := strings.Index(prompt, "Sign-off")
	if proceed < 0 || signoff < 0 || proceed > signoff {
		t.Fatalf("ordering is wrong: proceed=%d signoff=%d", proceed, signoff)
	}

	// The execution turn has no "how to proceed" line but still signs off.
	exec := policy.Prompt(ActionExecute, delivery, PromptContext{Basis: ctx.Basis})
	if strings.Contains(exec, "how to proceed") {
		t.Error("the execution prompt tells the maintainer how to start")
	}
	if !strings.Contains(exec, "> "+SelfMarker) {
		t.Error("the execution prompt has no sign-off")
	}

	// A caller that forgot the basis still gets a usable sentence.
	bare := policy.Prompt(ActionAnalyze, delivery, PromptContext{})
	if !strings.Contains(bare, "from a FlowHub automation trigger.") {
		t.Error("the prompt has no fallback basis")
	}
}

func TestTheNewEnglishSignOffIsRecognisedAsOurOwn(t *testing.T) {
	policy := Policy{}.Defaults()
	reply := "## Analysis\n\nNothing blocking.\n\n> This comment was generated automatically by opencode, from the creation of this issue.\n> \n"
	if self, why := policy.isSelfComment(reply, ""); !self {
		t.Fatalf("an English sign-off was not recognised as ours: %q", why)
	}
}
