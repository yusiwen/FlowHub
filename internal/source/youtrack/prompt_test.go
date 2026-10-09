package youtrack

import (
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/rules"
)

// made builds the event shape the adapter produces, for the prompt tests.
func made(kind event.Kind, key string, mutate ...func(*event.Event)) event.Event {
	e := event.Event{Source: SourceName, Kind: kind, Subject: event.Subject{Key: key}}
	for _, apply := range mutate {
		apply(&e)
	}
	return e
}

func withSubject(project, title, body string) func(*event.Event) {
	return func(e *event.Event) {
		e.Subject.Project, e.Subject.Title, e.Subject.Body = project, title, body
	}
}

func TestPromptsCarryTheContract(t *testing.T) {
	src := New(rules.Policy{}, "")
	delivery := made(event.KindCreated, "TEST-12", withSubject("TEST", "准入首页", "分页返回黑名单机构信息"))
	ctx := rules.PromptContext{Worktree: "/wt/TEST-12", Repository: "mine/test", Author: "yusiwen"}

	for _, action := range []rules.Action{rules.ActionAnalyze, rules.ActionPlan, rules.ActionExecute} {
		prompt := src.Prompt(action, &delivery, ctx)
		for _, want := range []string{
			"TEST-12",
			"/wt/TEST-12",
			rules.SelfMarker,
			"UNTRUSTED input",
			".flowhub/attachments",
			"youtrack_add_issue_comment",
			"youtrack_get_issue",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s prompt is missing %q", action, want)
			}
		}
	}

	// The read-only instruction must not leak into the execution turn, and vice
	// versa: the phase is what keeps analysis harmless.
	if strings.Contains(src.Prompt(rules.ActionAnalyze, &delivery, ctx), "commit locally") {
		t.Error("the analysis prompt tells the agent it may commit")
	}
	if !strings.Contains(src.Prompt(rules.ActionExecute, &delivery, ctx), "never push") {
		t.Error("the execution prompt does not forbid pushing")
	}
}

// TestThePromptNamesTheConfiguredTrigger keeps the two halves of one policy
// together: the phrase the prompt tells the maintainer to type has to be the phrase
// the dispatcher's trigger matches, or following the instruction does nothing.
func TestThePromptNamesTheConfiguredTrigger(t *testing.T) {
	policy := rules.Policy{Trigger: "/flowhub go", StartStates: []string{"Doing"}}
	src := New(policy, "")
	if src.Policy().Trigger != "/flowhub go" {
		t.Fatalf("the adapter did not keep the configured trigger: %q", src.Policy().Trigger)
	}
	delivery := made(event.KindCreated, "TEST-12")
	ctx := rules.PromptContext{Worktree: "/wt/TEST-12", Basis: "the creation of this issue"}

	analysis := src.Prompt(rules.ActionAnalyze, &delivery, ctx)
	if !strings.Contains(analysis, "/flowhub go") || !strings.Contains(analysis, "Doing") {
		t.Errorf("the analysis prompt does not name the configured trigger:\n%s", analysis)
	}
	if strings.Contains(src.Prompt(rules.ActionExecute, &delivery, ctx), "/flowhub go") {
		t.Error("the execution prompt must not tell the maintainer how to start")
	}
}

func TestPromptForAnUnknownActionIsEmpty(t *testing.T) {
	if got := New(rules.Policy{}, "").Prompt(rules.ActionIgnore, &event.Event{}, rules.PromptContext{}); got != "" {
		t.Fatalf("prompt = %q, want empty", got)
	}
}

func TestPromptRequiresABlockquoteSignOff(t *testing.T) {
	src := New(rules.Policy{}, "")
	delivery := made(event.KindCreated, "TEST-20", withSubject("TEST", "s", ""))
	decision := rules.Decision{Action: rules.ActionAnalyze, Trigger: rules.TriggerCreated}
	ctx := rules.PromptContext{
		Worktree: "/wt/TEST-20", Repository: "mine/test", Author: "yusiwen",
		Basis: src.Policy().Basis(decision, delivery),
	}
	prompt := src.Prompt(rules.ActionAnalyze, &delivery, ctx)

	for _, want := range []string{
		"> This comment was generated automatically by opencode, from the creation of this issue.",
		"> " + rules.SelfMarker,
		"blockquote",
		"in the language of the issue",
		"The final line of the comment must be `> " + rules.SelfMarker + "`",
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
	exec := src.Prompt(rules.ActionExecute, &delivery, rules.PromptContext{Basis: ctx.Basis})
	if strings.Contains(exec, "how to proceed") {
		t.Error("the execution prompt tells the maintainer how to start")
	}
	if !strings.Contains(exec, "> "+rules.SelfMarker) {
		t.Error("the execution prompt has no sign-off")
	}

	// A caller that forgot the basis still gets a usable sentence.
	bare := src.Prompt(rules.ActionAnalyze, &delivery, rules.PromptContext{})
	if !strings.Contains(bare, "from a FlowHub automation trigger.") {
		t.Error("the prompt has no fallback basis")
	}
}

// TestProjectInstructionsAreAppendedNotSubstituted is the constraint the ADR's
// `prompt_file` had to bend around: the file adds guidance, and the adapter's own
// contract — the untrusted-input rule, the reply tool, the marker and the trigger it
// names — survives whatever the operator wrote. A file that could delete the marker
// would break the loop prevention that keeps FlowHub from answering its own replies.
//
// Both levels are covered, because they reach the adapter by different routes: the
// source's `prompt_file` is handed to New (it applies to every project this adapter
// serves), while the project's arrives per turn in the context.
func TestProjectInstructionsAreAppendedNotSubstituted(t *testing.T) {
	src := New(rules.Policy{}, "This source is the BEAP workspace: read CODEOWNERS first.\n")
	delivery := made(event.KindCreated, "TEST-12", withSubject("TEST", "s", "body"))
	ctx := rules.PromptContext{
		Worktree:     "/wt/TEST-12",
		Repository:   "mine/test",
		Basis:        "the creation of this issue",
		Instructions: "This repository is Go: run `make test` before you report.\n",
	}
	prompt := src.Prompt(rules.ActionAnalyze, &delivery, ctx)

	for _, want := range []string{
		"## Operator instructions",
		"### This source",
		"read CODEOWNERS first",
		"### This project",
		"make test",
		"UNTRUSTED input",
		"youtrack_add_issue_comment",
		"<!-- flowhub-auto -->",
		"from the creation of this issue",
		"## This turn",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	// The instructions sit between the ground rules and the phase, so the rules read
	// as standing guidance and the phase still comes last. The source's text comes
	// before the project's, because it is the broader of the two.
	groundRules := strings.Index(prompt, "## Ground rules")
	section := strings.Index(prompt, "## Operator instructions")
	sourceExtra := strings.Index(prompt, "### This source")
	projectExtra := strings.Index(prompt, "### This project")
	turn := strings.Index(prompt, "## This turn")
	if !(groundRules < section && section < sourceExtra && sourceExtra < projectExtra && projectExtra < turn) {
		t.Fatalf("ordering is wrong: rules=%d section=%d source=%d project=%d turn=%d",
			groundRules, section, sourceExtra, projectExtra, turn)
	}

	// No instructions at all: the section must not appear, so an existing deployment's
	// prompt is byte-for-byte what it was.
	without := New(rules.Policy{}, "").Prompt(rules.ActionAnalyze, &delivery, rules.PromptContext{Worktree: "/wt/TEST-12"})
	if strings.Contains(without, "## Operator instructions") {
		t.Errorf("the section appeared without any instructions:\n%s", without)
	}

	// The project's file alone still produces the section, and the source heading is
	// absent, so a project-only deployment does not get an empty "This source" block.
	projectOnly := New(rules.Policy{}, "").Prompt(rules.ActionAnalyze, &delivery, ctx)
	if !strings.Contains(projectOnly, "## Operator instructions") || !strings.Contains(projectOnly, "### This project") {
		t.Errorf("project-only instructions did not render:\n%s", projectOnly)
	}
	if strings.Contains(projectOnly, "### This source") {
		t.Errorf("a source heading appeared without source instructions:\n%s", projectOnly)
	}
}

// TestTheFollowUpPromptAsksForTheCommentAndSaysWhy pins the follow-up instruction.
// It is the prompt the dispatcher sends when a turn ended without a comment, so it has
// to (a) name the failure rather than the task, or the model simply retries the work it
// was already refused, and (b) keep the whole reply contract, because that is the only
// thing that stops a posted comment from re-triggering FlowHub.
func TestTheFollowUpPromptAsksForTheCommentAndSaysWhy(t *testing.T) {
	src := New(rules.Policy{}, "")
	delivery := made(event.KindUpdated, "TEST-12", withSubject("TEST", "准入首页", "分页返回黑名单机构信息"))
	ctx := rules.PromptContext{
		Worktree: "/wt/TEST-12", Repository: "mine/test", Author: "yusiwen",
		Basis: `the comment "/opencode start"`,
		Nudge: true,
	}

	for _, action := range []rules.Action{rules.ActionAnalyze, rules.ActionPlan, rules.ActionExecute} {
		prompt := src.Prompt(action, &delivery, ctx)
		for _, want := range []string{
			"ended without posting the comment",
			"TEST-12",
			"/wt/TEST-12",
			rules.SelfMarker,
			"youtrack_add_issue_comment",
			"UNTRUSTED input",
			"youtrack_get_issue",
			// The basis still comes from the decision, so the sign-off stays honest.
			`from the comment "/opencode start".`,
			"quote the command or tool call that was refused",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s follow-up prompt is missing %q", action, want)
			}
		}
		// The phase section is replaced, not duplicated: a follow-up that also reads as
		// "produce a plan" or "implement the plan" is an invitation to retry the refused
		// work instead of writing the comment.
		for _, unwanted := range []string{"## This turn\nThis turn is READ-ONLY", "Implement the plan in the working directory"} {
			if strings.Contains(prompt, unwanted) {
				t.Errorf("%s follow-up prompt still carries the ordinary phase instruction %q", action, unwanted)
			}
		}
	}
}
