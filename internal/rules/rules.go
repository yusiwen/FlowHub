// Package rules decides what a delivery should make FlowHub do, and writes the
// prompt for it.
//
// The policy is deliberately small and explicit. Everything here is derived from
// measurements recorded in youtrack-webhook-and-flowhub-security.md §5.6 and from
// the workflow the human asked for: analyse on creation, then implement only after
// a human says so.
package rules

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/registry"
)

// Action is what FlowHub should do about a delivery.
type Action string

const (
	// ActionIgnore means record it and stop.
	ActionIgnore Action = "ignore"
	// ActionAnalyze is a read-only turn that analyses the issue and replies.
	ActionAnalyze Action = "analyze"
	// ActionPlan produces or refreshes an implementation plan and lists the
	// questions a human must answer first.
	ActionPlan Action = "plan"
	// ActionExecute implements the plan inside the task worktree.
	ActionExecute Action = "execute"
)

// DefaultSelfMarkers identify FlowHub's own comments. The HTML comment is the
// primary marker because it is machine stable and invisible in the rendered UI;
// the Chinese sentences cover comments the agent already posts today.
var DefaultSelfMarkers = []string{
	"<!-- flowhub-auto -->",
	"opencode 分析后自动生成",
	"opencode 自动生成",
	"请确认后再继续",
	// The prompt requires this phrase in the sign-off, so an English reply that
	// lost the HTML marker is still recognisable. The trade-off is deliberate and
	// symmetric with the Chinese sentences above: a human who quotes our own
	// sign-off loses that one comment, while a missed marker would start a turn
	// that the human never asked for.
	"generated automatically by opencode",
}

// SelfMarker is the literal the prompt requires on the last line of every reply.
const SelfMarker = "<!-- flowhub-auto -->"

// PromptContext is the routing information a source's prompt needs.
type PromptContext struct {
	// Worktree is the directory the agent works in.
	Worktree string
	// AttachmentsDir is where downloaded attachments must be written, relative to
	// the worktree.
	AttachmentsDir string
	// Repository is the human readable repository identity, for context only.
	Repository string
	// Author is the login whose action triggered this turn.
	Author string
	// Basis is what caused the turn, as a short English clause the sign-off can
	// translate ("the creation of this issue", `the comment "/opencode start"`).
	// It comes from the decision, so the reply cannot claim a reason of its own.
	Basis string
	// Instructions is what the deployment wants this turn to know, from a
	// `prompt_file` in the configuration file: project-specific guidance such as
	// "this repository is Java, run `mvn -q verify`". It is *appended* to the
	// adapter's own prompt, never a replacement, because the marker and the sign-off
	// that keep FlowHub from answering its own replies belong to the adapter.
	Instructions string
}

// TaskView is what the registry already knows about the issue.
type TaskView struct {
	Known     bool
	Plan      registry.PlanState
	Turns     int
	LastReply string
}

// Policy is the trigger configuration.
type Policy struct {
	// Trigger is the comment phrase that means "start implementing".
	Trigger string
	// StartStates are the state values that mean "start implementing".
	StartStates []string
	// SkipAnalyzeOnCreate turns off the automatic read-only analysis of a newly
	// created issue. Analysis on creation is the default, so the field names the
	// exception rather than the rule (a plain bool could not tell "unset" from
	// "explicitly off").
	SkipAnalyzeOnCreate bool
	// SelfMarkers identify our own comments.
	SelfMarkers []string
	// MaxTurns stops a task that keeps triggering; the cheap runaway guard.
	MaxTurns int
	// WriteAccess tells the prompt that the execution turn may change files.
	WriteAccess bool
}

// Defaults fills the zero fields with the measured, working values.
func (p Policy) Defaults() Policy {
	if p.Trigger == "" {
		p.Trigger = "/opencode start"
	}
	if len(p.StartStates) == 0 {
		p.StartStates = []string{"In Progress"}
	}
	if len(p.SelfMarkers) == 0 {
		p.SelfMarkers = append([]string(nil), DefaultSelfMarkers...)
	}
	if p.MaxTurns <= 0 {
		p.MaxTurns = 8
	}
	return p
}

// Trigger names what caused a turn. It exists so the reply can state, truthfully,
// what the agent was reacting to: the model is told the basis and only renders it
// in the issue's language, instead of inventing a reason for its own reply.
type Trigger string

const (
	// TriggerCreated is the automatic read-only analysis of a new issue.
	TriggerCreated Trigger = "issue_created"
	// TriggerComment is a comment that matched the trigger or the analyze form.
	TriggerComment Trigger = "comment"
	// TriggerState is a workflow state change into one of the start states.
	TriggerState Trigger = "state"
)

// Decision is the outcome of applying the policy.
type Decision struct {
	Action  Action
	Trigger Trigger
	Reason  string
}

// Decide applies the policy to one delivery.
//
// Order matters: our own comments are filtered first (the agent posts as the same
// YouTrack user as the human, so identity cannot be used), then the turn budget,
// then the explicit human triggers, and only then the automatic analysis.
func (p Policy) Decide(e event.Event, task TaskView) Decision {
	p = p.Defaults()

	if task.Plan == "" {
		task.Plan = registry.PlanNone
	}
	if e.IsComment() {
		if self, why := p.isSelfComment(e.Comment, task.LastReply); self {
			return Decision{Action: ActionIgnore, Reason: "our own comment (" + why + ")"}
		}
	}
	if task.Turns >= p.MaxTurns {
		return Decision{
			Action: ActionIgnore,
			Reason: fmt.Sprintf("task has already run %d turns (limit %d); a human must take over", task.Turns, p.MaxTurns),
		}
	}

	if e.IsComment() {
		switch {
		case startPattern(p.Trigger).MatchString(e.Comment):
			return p.startDecision(task, TriggerComment, "comment matched "+p.Trigger)
		case analyzePattern().MatchString(e.Comment):
			return Decision{Action: ActionAnalyze, Trigger: TriggerComment, Reason: "comment asked for analysis"}
		}
	}

	if value, ok := p.stateTransition(e); ok {
		return p.startDecision(task, TriggerState, fmt.Sprintf("%s changed to %q", e.StateField, value))
	}

	if e.Kind == event.KindCreated && !p.SkipAnalyzeOnCreate {
		return Decision{Action: ActionAnalyze, Trigger: TriggerCreated, Reason: "issue was created"}
	}

	return Decision{Action: ActionIgnore, Reason: "no rule matched this event"}
}

// startDecision picks between planning and executing, which is the "check whether
// a plan exists first" step of the agreed workflow: with no plan yet, the turn
// produces one and asks the questions that block implementation.
func (p Policy) startDecision(task TaskView, trigger Trigger, reason string) Decision {
	if task.Plan == registry.PlanNone {
		return Decision{Action: ActionPlan, Trigger: trigger, Reason: reason + "; no plan exists yet, so plan first"}
	}
	return Decision{Action: ActionExecute, Trigger: trigger, Reason: reason + "; a plan exists"}
}

// stateTransition reports whether the event set a workflow state that means "start
// implementing". The adapter has already normalised which field carried it and what
// the value is; the policy only knows the values it acts on.
func (p Policy) stateTransition(e event.Event) (value string, ok bool) {
	if strings.TrimSpace(e.State) == "" {
		return "", false
	}
	for _, wanted := range p.StartStates {
		if strings.EqualFold(strings.TrimSpace(e.State), strings.TrimSpace(wanted)) {
			return e.State, true
		}
	}
	return "", false
}

// isSelfComment recognises our own reply by content, because the agent posts as
// the same YouTrack user as the human.
func (p Policy) isSelfComment(text, lastReply string) (bool, string) {
	if strings.TrimSpace(text) == "" {
		return false, ""
	}
	for _, marker := range p.SelfMarkers {
		if marker != "" && strings.Contains(text, marker) {
			return true, "contains " + marker
		}
	}
	// Second layer, independent of the model remembering the marker: the agent
	// posted text we already produced, so a distinctive prefix of our last reply
	// appears in the comment.
	if probe := probeOf(lastReply); probe != "" && strings.Contains(normalizeSpace(text), probe) {
		return true, "repeats our previous reply"
	}
	return false, ""
}

// probeOf returns a distinctive, rune-safe prefix of a reply, or "" when the reply
// is too short or too generic to be evidence.
func probeOf(reply string) string {
	normalized := normalizeSpace(reply)
	runes := []rune(normalized)
	if len(runes) < 60 {
		return ""
	}
	if len(runes) > 160 {
		runes = runes[:160]
	}
	return string(runes)
}

// normalizeSpace collapses whitespace so formatting differences do not hide a
// match, and drops the marker itself.
func normalizeSpace(text string) string {
	text = strings.ReplaceAll(text, SelfMarker, " ")
	return strings.Join(strings.Fields(text), " ")
}

func startPattern(trigger string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)^\s*` + regexp.QuoteMeta(trigger) + `\b`)
}

// analyzePattern matches a bare /opencode comment, which means "look at this
// again". Natural language is deliberately not accepted: the trigger has to be
// unambiguous, and the reply of the agent itself must never look like one.
func analyzePattern() *regexp.Regexp {
	return regexp.MustCompile(`(?i)^\s*/opencode\s*(analyze)?\s*$`)
}

// Basis renders what triggered a turn as a short English clause, for the reply's
// sign-off. It is derived from the delivery and the policy rather than asked of
// the model: a model asked to explain its own reply writes something plausible
// instead of something true.
func (p Policy) Basis(decision Decision, e event.Event) string {
	p = p.Defaults()
	switch decision.Trigger {
	case TriggerCreated:
		return "the creation of this issue"
	case TriggerComment:
		if text := firstLine(e.Comment, 80); text != "" {
			return fmt.Sprintf("the comment %q", text)
		}
		return "a comment on this issue"
	case TriggerState:
		if value, ok := p.stateTransition(e); ok {
			field := e.StateField
			if strings.TrimSpace(field) == "" {
				field = "state"
			}
			return fmt.Sprintf("the state change of %s to %q", field, value)
		}
		return "a workflow state change"
	default:
		return "a FlowHub automation trigger"
	}
}

// firstLine collapses a comment to one short line, so a quoted comment cannot
// turn the sign-off into a copy of the issue thread.
func firstLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}
