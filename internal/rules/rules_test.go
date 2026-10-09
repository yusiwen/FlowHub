package rules

import (
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/registry"
)

// made builds an event the way an adapter would: the neutral shape, nothing else.
func made(kind event.Kind, key string, mutate ...func(*event.Event)) event.Event {
	e := event.Event{Source: "youtrack", Kind: kind, Subject: event.Subject{Key: key}}
	for _, apply := range mutate {
		apply(&e)
	}
	return e
}

func withComment(text string) func(*event.Event) {
	return func(e *event.Event) { e.Comment = text }
}

func withState(field, value string) func(*event.Event) {
	return func(e *event.Event) {
		e.Kind, e.StateField, e.State = event.KindStateSet, field, value
	}
}

func TestAnalyzeOnCreate(t *testing.T) {
	policy := Policy{}.Defaults()
	decision := policy.Decide(made(event.KindCreated, "TEST-11"), TaskView{})
	if decision.Action != ActionAnalyze {
		t.Fatalf("action = %s (%s), want analyze", decision.Action, decision.Reason)
	}

	// The switch exists so a noisy tracker can be quietened without a code change.
	quiet := Policy{SkipAnalyzeOnCreate: true}.Defaults()
	if got := quiet.Decide(made(event.KindCreated, ""), TaskView{}).Action; got != ActionIgnore {
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
		decision := policy.Decide(made(event.KindCommented, "TEST-1", withComment(text)), TaskView{Known: true})
		if decision.Action != want {
			t.Errorf("Decide(%q) = %s (%s), want %s", text, decision.Action, decision.Reason, want)
		}
	}
}

func TestStartPlansFirstWhenNoPlanExists(t *testing.T) {
	policy := Policy{}.Defaults()
	start := made(event.KindCommented, "TEST-1", withComment("/opencode start"))

	if got := policy.Decide(start, TaskView{Known: true, Plan: registry.PlanNone}); got.Action != ActionPlan {
		t.Fatalf("action = %s (%s), want plan first", got.Action, got.Reason)
	}
	if got := policy.Decide(start, TaskView{Known: true, Plan: registry.PlanDraft}); got.Action != ActionExecute {
		t.Fatalf("action = %s (%s), want execute when a plan exists", got.Action, got.Reason)
	}
}

func TestStateTransitionToInProgressStarts(t *testing.T) {
	policy := Policy{}.Defaults()
	stateChange := made(event.KindStateSet, "TEST-1", withState("State", "In Progress"))
	decision := policy.Decide(stateChange, TaskView{Known: true, Plan: registry.PlanDraft})
	if decision.Action != ActionExecute {
		t.Fatalf("action = %s (%s), want execute", decision.Action, decision.Reason)
	}

	// The payload also repeats the field under its localised name, and the
	// comparison is case-insensitive because YouTrack renders names either way.
	localised := made(event.KindStateSet, "", withState("状态", "in progress"))
	if got := policy.Decide(localised, TaskView{Known: true, Plan: registry.PlanDraft}); got.Action != ActionExecute {
		t.Fatalf("localised state name did not trigger: %s", got.Reason)
	}

	// Other field changes must not start work.
	unrelated := made(event.KindUpdated, "TEST-1")
	if got := policy.Decide(unrelated, TaskView{Known: true}); got.Action != ActionIgnore {
		t.Fatalf("an unrelated field change triggered %s", got.Action)
	}
	// Nor must a move to a different state.
	other := made(event.KindStateSet, "TEST-1", withState("State", "Open"))
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
		decision := policy.Decide(made(event.KindCommented, "", withComment(text)), TaskView{Known: true})
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
		made(event.KindCommented, "", withComment(quoted)),
		TaskView{Known: true, LastReply: reply},
	)
	if decision.Action != ActionIgnore {
		t.Fatalf("action = %s (%s), want ignore because the comment repeats our reply", decision.Action, decision.Reason)
	}

	// A genuinely different comment must still get through. A plan exists here,
	// so the instruction executes rather than planning again.
	other := policy.Decide(
		made(event.KindCommented, "", withComment("/opencode start")),
		TaskView{Known: true, Plan: registry.PlanDraft, LastReply: reply},
	)
	if other.Action != ActionExecute {
		t.Fatalf("a real instruction was ignored: %s", other.Reason)
	}
}

func TestTurnBudgetStopsARunaway(t *testing.T) {
	policy := Policy{MaxTurns: 3}.Defaults()
	decision := policy.Decide(
		made(event.KindCommented, "", withComment("/opencode start")),
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
	// Everything an adapter cannot classify arrives as KindOther, which is how
	// YouTrack's work-item and attachment events keep being ignored.
	policy := Policy{}.Defaults()
	for _, kind := range []event.Kind{event.KindOther, event.KindDeleted, event.KindUpdated, ""} {
		if got := policy.Decide(made(kind, "TEST-1"), TaskView{}); got.Action != ActionIgnore {
			t.Errorf("kind %q produced %s", kind, got.Action)
		}
	}
}

func TestTheNewEnglishSignOffIsRecognisedAsOurOwn(t *testing.T) {
	policy := Policy{}.Defaults()
	reply := "## Analysis\n\nNothing blocking.\n\n> This comment was generated automatically by opencode, from the creation of this issue.\n> \n"
	if self, why := policy.isSelfComment(reply, ""); !self {
		t.Fatalf("an English sign-off was not recognised as ours: %q", why)
	}
}

func TestBasisNamesTheRealTrigger(t *testing.T) {
	policy := Policy{}.Defaults()

	comment := made(event.KindCommented, "TEST-20", withComment("/opencode start\n\n请开始实施"))
	state := made(event.KindStateSet, "TEST-20", withState("State", "In Progress"))
	cases := map[string]struct {
		decision Decision
		delivery event.Event
		want     string
	}{
		"created": {Decision{Trigger: TriggerCreated}, made(event.KindCreated, "TEST-20"), "the creation of this issue"},
		"comment": {Decision{Trigger: TriggerComment}, comment, `the comment "/opencode start 请开始实施"`},
		"state":   {Decision{Trigger: TriggerState}, state, `the state change of State to "In Progress"`},
		"unknown": {Decision{}, event.Event{}, "a FlowHub automation trigger"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := policy.Basis(tc.decision, tc.delivery); got != tc.want {
				t.Fatalf("Basis = %q, want %q", got, tc.want)
			}
		})
	}

	// A long comment must not be copied into the sign-off whole.
	long := made(event.KindCommented, "TEST-20", withComment(strings.Repeat("很长的一段话", 40)))
	if got := policy.Basis(Decision{Trigger: TriggerComment}, long); len([]rune(got)) > 120 {
		t.Fatalf("Basis copied too much of the comment: %d runes", len([]rune(got)))
	}
}

// TestAPermitAuthorisesTheRefusedCommandsAndNothingElse pins the decision the human's
// keyword produces. Two things matter: it only fires when there is something to
// authorise (the refused set is the whole scope of the authorisation, so a permit with
// nothing to point at must be ignored rather than guessed at), and the action it
// produces is the one that continues the work.
func TestAPermitAuthorisesTheRefusedCommands(t *testing.T) {
	p := Policy{}.Defaults()
	refused := []registry.Refusal{{Command: "mvn -q verify", Reason: "not on the read-only allowlist"}}

	decision := p.Decide(made(event.KindCommented, "TEST-1", withComment(PermitTrigger)), TaskView{Refusals: refused})
	if decision.Action != ActionPermit {
		t.Fatalf("action = %s (%s), want permit", decision.Action, decision.Reason)
	}
	if decision.Trigger != TriggerComment {
		t.Fatalf("trigger = %q, want the comment that caused it", decision.Trigger)
	}
	// The sign-off quotes the phrase the human typed, so a reader can see what it reacted to.
	if basis := p.Basis(decision, made(event.KindCommented, "TEST-1", withComment(PermitTrigger))); !strings.Contains(basis, PermitTrigger) {
		t.Fatalf("basis = %q, want it to name the permit comment", basis)
	}

	empty := p.Decide(made(event.KindCommented, "TEST-1", withComment(PermitTrigger)), TaskView{})
	if empty.Action != ActionIgnore {
		t.Fatalf("a permit with nothing refused produced %s, want it ignored", empty.Action)
	}
	if !strings.Contains(empty.Reason, "nothing to authorise") {
		t.Fatalf("reason = %q, want it to say why", empty.Reason)
	}
}

func TestARevokeIsRecognised(t *testing.T) {
	p := Policy{}.Defaults()
	decision := p.Decide(made(event.KindCommented, "TEST-1", withComment(RevokeTrigger)), TaskView{})
	if decision.Action != ActionRevoke {
		t.Fatalf("action = %s (%s), want revoke", decision.Action, decision.Reason)
	}
	// Revoking is not gated on anything, not even the refused set: a human retracting an
	// authorisation must always be able to.
	atLimit := p.Decide(made(event.KindCommented, "TEST-1", withComment(RevokeTrigger)), TaskView{Turns: 99})
	if atLimit.Action != ActionRevoke {
		t.Fatalf("a revoke at the turn limit produced %s, want it honoured", atLimit.Action)
	}
}

// TestThePermitPhraseIsAnchoredAndNeverOurOwnComment: the agent tells the maintainer
// which keyword to type, so the phrase appears in its replies constantly. Only a
// comment that *starts* with it may act, and never one that carries our marker — the
// agent posts as the same tracker user as the human, so a model that writes the keyword
// itself must authorise nothing.
func TestThePermitPhraseIsAnchoredAndNeverOurOwnComment(t *testing.T) {
	p := Policy{}.Defaults()
	view := TaskView{Refusals: []registry.Refusal{{Command: "mvn -q verify"}}}

	for _, text := range []string{
		"you can reply " + PermitTrigger + " to authorise it",
		"> " + PermitTrigger,
		"Permit me: " + PermitTrigger,
		"`" + PermitTrigger + "`",
	} {
		if decision := p.Decide(made(event.KindCommented, "TEST-1", withComment(text)), view); decision.Action == ActionPermit {
			t.Errorf("%q was read as a permit", text)
		}
	}
	// The anchored form acts, and trailing words are allowed exactly as they are after
	// the start trigger — a human may write "permit, go ahead". Trailing text cannot
	// widen anything: the authorised set always comes from what the turn was refused
	// for, never from what the human wrote after the keyword.
	for _, text := range []string{"  " + PermitTrigger, PermitTrigger + ", go ahead"} {
		if decision := p.Decide(made(event.KindCommented, "TEST-1", withComment(text)), view); decision.Action != ActionPermit {
			t.Errorf("%q was not recognised as a permit: %s", text, decision.Reason)
		}
	}

	// Our own reply, marker and all, grants nothing even when the phrase is first.
	self := PermitTrigger + "\n\n" + SelfMarker
	decision := p.Decide(made(event.KindCommented, "TEST-1", withComment(self)), view)
	if decision.Action != ActionIgnore {
		t.Fatalf("our own comment produced %s, want ignore", decision.Action)
	}
}
