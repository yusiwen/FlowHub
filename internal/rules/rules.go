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
}

// SelfMarker is the literal the prompt requires on the last line of every reply.
const SelfMarker = "<!-- flowhub-auto -->"

// DefaultStateFields are the changedFields names that carry the workflow state.
// The measured payload repeats a custom field under its internal and its
// localised name, so both spellings have to be listed.
var DefaultStateFields = []string{"State", "状态"}

// Delivery is the part of an audited delivery the rules need.
type Delivery struct {
	Event       string
	IssueID     string
	ProjectKey  string
	Summary     string
	Description string
	CommentText string
	Actor       string
	// States maps a changed field name to the new value's internal name, for
	// example {"State": "In Progress"}.
	States map[string]string
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
	// StateFields are the changedFields names that carry the workflow state.
	StateFields []string
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
	if len(p.StateFields) == 0 {
		p.StateFields = append([]string(nil), DefaultStateFields...)
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

// Decision is the outcome of applying the policy.
type Decision struct {
	Action Action
	Reason string
}

// Decide applies the policy to one delivery.
//
// Order matters: our own comments are filtered first (the agent posts as the same
// YouTrack user as the human, so identity cannot be used), then the turn budget,
// then the explicit human triggers, and only then the automatic analysis.
func (p Policy) Decide(d Delivery, task TaskView) Decision {
	p = p.Defaults()

	if task.Plan == "" {
		task.Plan = registry.PlanNone
	}
	if d.Event == "commentAdded" || d.Event == "commentUpdated" {
		if self, why := p.isSelfComment(d.CommentText, task.LastReply); self {
			return Decision{Action: ActionIgnore, Reason: "our own comment (" + why + ")"}
		}
	}
	if task.Turns >= p.MaxTurns {
		return Decision{
			Action: ActionIgnore,
			Reason: fmt.Sprintf("task has already run %d turns (limit %d); a human must take over", task.Turns, p.MaxTurns),
		}
	}

	if d.Event == "commentAdded" || d.Event == "commentUpdated" {
		switch {
		case startPattern(p.Trigger).MatchString(d.CommentText):
			return p.startDecision(task, "comment matched "+p.Trigger)
		case analyzePattern().MatchString(d.CommentText):
			return Decision{Action: ActionAnalyze, Reason: "comment asked for analysis"}
		}
	}

	if value, field, ok := p.stateTransition(d); ok {
		return p.startDecision(task, fmt.Sprintf("%s changed to %q", field, value))
	}

	if d.Event == "issueCreated" && !p.SkipAnalyzeOnCreate {
		return Decision{Action: ActionAnalyze, Reason: "issue was created"}
	}

	return Decision{Action: ActionIgnore, Reason: "no rule matched this event"}
}

// startDecision picks between planning and executing, which is the "check whether
// a plan exists first" step of the agreed workflow: with no plan yet, the turn
// produces one and asks the questions that block implementation.
func (p Policy) startDecision(task TaskView, reason string) Decision {
	if task.Plan == registry.PlanNone {
		return Decision{Action: ActionPlan, Reason: reason + "; no plan exists yet, so plan first"}
	}
	return Decision{Action: ActionExecute, Reason: reason + "; a plan exists"}
}

// stateTransition reports the first configured state field that moved into one of
// the start states.
func (p Policy) stateTransition(d Delivery) (value, field string, ok bool) {
	for _, name := range p.StateFields {
		moved, present := d.States[name]
		if !present {
			continue
		}
		for _, wanted := range p.StartStates {
			if strings.EqualFold(strings.TrimSpace(moved), strings.TrimSpace(wanted)) {
				return moved, name, true
			}
		}
	}
	return "", "", false
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
