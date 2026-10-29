package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/compare"
	"github.com/yusiwen/flowhub/internal/flow/builtin"
	"github.com/yusiwen/flowhub/internal/flow/mode"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/source/youtrack"
	"github.com/yusiwen/flowhub/internal/store"
	tasksvc "github.com/yusiwen/flowhub/internal/tasks"
	"github.com/yusiwen/flowhub/internal/upstream"
)

type liveSelector struct {
	host tasksvc.Host
	err  error
}

func (s liveSelector) Select(context.Context, *store.Record, *projectmap.Entry) (tasksvc.Host, error) {
	return s.host, s.err
}

type liveTasks struct {
	task   registry.Task
	ensure int
	turn   int
	grant  int
	revoke int
	last   tasksvc.TurnRequest
}

func (s *liveTasks) Ensure(_ context.Context, request tasksvc.EnsureRequest) (registry.Task, error) {
	s.ensure++
	s.task.Key = request.Record.IssueID
	if s.task.Worktree == "" {
		s.task.Worktree = "/tmp/worktree"
	}
	return s.task, nil
}

func (s *liveTasks) Turn(_ context.Context, request tasksvc.TurnRequest) (tasksvc.TurnResult, error) {
	s.turn++
	s.last = request
	s.task = request.Task
	s.task.Turns++
	s.task.State = registry.StateDone
	s.task.LastAction = string(request.Action)
	return tasksvc.TurnResult{
		Task: s.task, SessionID: "ses_flow", Text: "done", Finished: true,
		Replied: true, Cost: 0.1, Action: request.Action,
	}, nil
}

func (s *liveTasks) Grant(_ context.Context, request tasksvc.GrantRequest) (tasksvc.GrantResult, error) {
	s.grant++
	commands := request.Commands
	if len(commands) == 0 {
		for _, refusal := range s.task.Refusals {
			commands = append(commands, refusal.Command)
		}
	}
	s.task.Grant = &registry.Grant{Commands: commands, By: "tester"}
	action := rules.Action("execute")
	return tasksvc.GrantResult{Task: s.task, Action: action}, nil
}

func (s *liveTasks) Revoke(_ context.Context, request tasksvc.RevokeRequest) error {
	s.revoke++
	s.task.Grant = nil
	return nil
}

func newLiveEngine(t *testing.T, service *liveTasks, selector tasksvc.Selector) *Engine {
	t.Helper()
	compiled, raw, err := builtin.Load()
	if err != nil {
		t.Fatalf("builtin: %v", err)
	}
	engine, err := New(Options{
		Flow:         compiled,
		Definition:   raw,
		Projects:     fakeRouter{projects: map[string]bool{"TEST": true}},
		Tasks:        fakeTasks{tasks: map[string]registry.Task{"TEST-12": service.task}},
		TaskService:  service,
		TaskSelector: selector,
		Policy:       youtrack.New(rules.Policy{}, "").Policy(),
		DataDir:      t.TempDir(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:          func() time.Time { return time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC) },
		ModeFor:      func(string, string) string { return mode.On },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return engine
}

func TestOnExecutesEnsureAndTurn(t *testing.T) {
	event := readEvent(t, "events/comment-start.json")
	service := &liveTasks{task: registry.Task{Key: event.Subject.Key, Plan: registry.PlanDraft, Turns: 1}}
	engine := newLiveEngine(t, service, liveSelector{host: tasksvc.Host{Name: "builder-a"}})
	run, err := engine.RunEvent(event)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run == nil || run.Mode != mode.On || run.State != StateSucceeded {
		t.Fatalf("run = %+v", run)
	}
	if service.ensure != 1 || service.turn != 1 {
		t.Fatalf("ensure=%d turn=%d", service.ensure, service.turn)
	}
	execute := run.Steps["execute"]
	if execute == nil || execute.State != StepSucceeded {
		t.Fatalf("execute = %+v", execute)
	}
	if execute.Output["replied"] != true || execute.Output["session_id"] != "ses_flow" {
		t.Fatalf("execute output = %+v", execute.Output)
	}
}

func TestOnIsOffOutsideTheProjectScope(t *testing.T) {
	event := readEvent(t, "events/comment-start.json")
	service := &liveTasks{task: registry.Task{Key: event.Subject.Key, Plan: registry.PlanDraft, Turns: 1}}
	compiled, raw, err := builtin.Load()
	if err != nil {
		t.Fatalf("builtin: %v", err)
	}
	engine, err := New(Options{
		Flow: compiled, Definition: raw, Projects: fakeRouter{projects: map[string]bool{"TEST": true}},
		Tasks:       fakeTasks{tasks: map[string]registry.Task{"TEST-12": service.task}},
		TaskService: service, TaskSelector: liveSelector{host: tasksvc.Host{Name: "builder-a"}},
		Policy: youtrack.New(rules.Policy{}, "").Policy(), DataDir: t.TempDir(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		ModeFor: func(string, string) string { return mode.Off },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	run, err := engine.RunEvent(event)
	if err != nil || run != nil {
		t.Fatalf("run = %+v, err = %v; want skipped", run, err)
	}
	if service.ensure != 0 || service.turn != 0 {
		t.Fatalf("service was called: %+v", service)
	}
}

func TestOnPermitGrantsAndContinues(t *testing.T) {
	event := readEvent(t, "events/comment-permit.json")
	service := &liveTasks{task: registry.Task{
		Key: event.Subject.Key, Plan: registry.PlanDraft, Turns: 1, LastAction: "execute",
		Refusals: []registry.Refusal{{Command: "mvn -q verify", Reason: "not allowed"}},
	}}
	engine := newLiveEngine(t, service, liveSelector{host: tasksvc.Host{Name: "builder-a"}})
	run, err := engine.RunEvent(event)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run == nil || run.State != StateSucceeded {
		t.Fatalf("run = %+v", run)
	}
	if service.grant != 1 || service.turn != 1 {
		t.Fatalf("grant=%d turn=%d", service.grant, service.turn)
	}
	if grant := run.Steps["permit"]; grant == nil || grant.State != StepSucceeded {
		t.Fatalf("permit = %+v", grant)
	}
}

func TestOnRevokeClearsTheGrant(t *testing.T) {
	event := readEvent(t, "events/comment-revoke.json")
	service := &liveTasks{task: registry.Task{
		Key: event.Subject.Key, Plan: registry.PlanDraft, Turns: 1, LastAction: "execute",
		Grant: &registry.Grant{Commands: []string{"mvn -q verify"}, By: "yusiwen"},
	}}
	engine := newLiveEngine(t, service, liveSelector{host: tasksvc.Host{Name: "builder-a"}})
	run, err := engine.RunEvent(event)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run == nil || run.State != StateSucceeded {
		t.Fatalf("run = %+v", run)
	}
	if service.revoke != 1 {
		t.Fatalf("revoke = %d", service.revoke)
	}
}

type authorRouter struct {
	authors []string
}

func (r authorRouter) Match(source, project string) (projectmap.Match, bool) {
	return projectmap.Match{
		Source: source, Key: project,
		Entry: &projectmap.Entry{Source: source, Project: project, Authors: r.authors},
	}, true
}

func TestOnHonoursTheAuthorAllowlist(t *testing.T) {
	event := readEvent(t, "events/comment-start.json")
	service := &liveTasks{task: registry.Task{Key: event.Subject.Key, Plan: registry.PlanDraft, Turns: 1}}
	compiled, raw, err := builtin.Load()
	if err != nil {
		t.Fatalf("builtin: %v", err)
	}
	engine, err := New(Options{
		Flow: compiled, Definition: raw, Projects: authorRouter{authors: []string{"someone-else"}},
		Tasks:       fakeTasks{tasks: map[string]registry.Task{"TEST-12": service.task}},
		TaskService: service, TaskSelector: liveSelector{host: tasksvc.Host{Name: "builder-a"}},
		Policy: youtrack.New(rules.Policy{}, "").Policy(), DataDir: t.TempDir(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		ModeFor: func(string, string) string { return mode.On },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	run, err := engine.RunEvent(event)
	if err != nil || run != nil {
		t.Fatalf("run = %+v, err = %v; want ignored", run, err)
	}
	if service.ensure != 0 || service.turn != 0 {
		t.Fatalf("service was called: %+v", service)
	}
}

func TestOnHonoursThePauseFile(t *testing.T) {
	event := readEvent(t, "events/comment-start.json")
	service := &liveTasks{task: registry.Task{Key: event.Subject.Key, Plan: registry.PlanDraft, Turns: 1}}
	pause := filepath.Join(t.TempDir(), "DISPATCH_OFF")
	if err := os.WriteFile(pause, []byte("off"), 0o600); err != nil {
		t.Fatalf("pause file: %v", err)
	}
	compiled, raw, err := builtin.Load()
	if err != nil {
		t.Fatalf("builtin: %v", err)
	}
	engine, err := New(Options{
		Flow: compiled, Definition: raw, Projects: fakeRouter{projects: map[string]bool{"TEST": true}},
		Tasks:       fakeTasks{tasks: map[string]registry.Task{"TEST-12": service.task}},
		TaskService: service, TaskSelector: liveSelector{host: tasksvc.Host{Name: "builder-a"}},
		Policy: youtrack.New(rules.Policy{}, "").Policy(), DataDir: t.TempDir(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PauseFile: pause,
		ModeFor: func(string, string) string { return mode.On },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	run, err := engine.RunEvent(event)
	if err != nil || run != nil {
		t.Fatalf("run = %+v, err = %v; want ignored", run, err)
	}
	if service.ensure != 0 || service.turn != 0 {
		t.Fatalf("service was called: %+v", service)
	}
}

func TestShadowRunPublishesItsDecisionForComparison(t *testing.T) {
	event := readEvent(t, "events/issue-created.json")
	compiled, raw, err := builtin.Load()
	if err != nil {
		t.Fatalf("builtin: %v", err)
	}
	comparisons, err := compare.Open(t.TempDir())
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	defer func() { _ = comparisons.Close() }()
	engine, err := New(Options{
		Flow: compiled, Definition: raw, Projects: fakeRouter{projects: map[string]bool{"TEST": true}},
		Tasks: fakeTasks{}, Policy: youtrack.New(rules.Policy{}, "").Policy(),
		DataDir: t.TempDir(), Compare: comparisons,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	run, err := engine.RunEvent(event)
	if err != nil || run == nil {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	if comparisons.Pending() != 1 {
		t.Fatalf("pending = %d, want the shadow side", comparisons.Pending())
	}
	comparisons.ObserveDispatch(compare.Decision{
		EventID: event.ID, Action: "analyze", Reason: "issue was created", Routable: true,
	})
	comparison, ok := comparisons.Get(event.ID)
	if !ok || !comparison.Match {
		t.Fatalf("comparison = %+v, ok = %v", comparison, ok)
	}
}

// TestTheFlowTurnCarriesTheDecisionTrigger: the executor derives the reply's
// sign-off basis from Decision.Trigger. Passing the literal "flow" made every
// flow-driven reply say "a FlowHub automation trigger", so a plan triggered by a
// state change could not name what moved it. The decision's own trigger has to
// survive into the turn.
func TestTheFlowTurnCarriesTheDecisionTrigger(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	event := upstream.Event{
		ID: "evt_trigger", Schema: 1, Upstream: "youtrack", Kind: upstream.KindIssueUpdated,
		OccurredAt: now, ReceivedAt: now,
		Subject: upstream.Subject{Type: "issue", Project: "TEST", Key: "TEST-12", Title: "issue"},
		Actor:   upstream.Actor{Login: "yusiwen"},
		Changes: []upstream.Change{{Field: "State", To: "In Progress"}},
	}
	service := &liveTasks{task: registry.Task{Key: "TEST-12", Plan: registry.PlanNone}}
	engine := newLiveEngine(t, service, liveSelector{host: tasksvc.Host{Name: "builder-a"}})
	if _, err := engine.RunEvent(event); err != nil {
		t.Fatalf("run: %v", err)
	}
	if service.turn != 1 {
		t.Fatalf("turns = %d, want one plan turn", service.turn)
	}
	if service.last.Trigger != rules.TriggerState {
		t.Fatalf("turn trigger = %q, want %q", service.last.Trigger, rules.TriggerState)
	}
	if !strings.Contains(service.last.Basis, "State changed") {
		t.Fatalf("turn basis = %q, want the state-change reason", service.last.Basis)
	}
}

// TestOnAReplanPlansEvenWithADraft: the flow's evaluate must choose the plan node
// for the replan phrase, not the execute node a draft would select for a start.
func TestOnAReplanPlansEvenWithADraft(t *testing.T) {
	event := readEvent(t, "events/comment-start.json")
	event.Comment.Text = "/opencode replan"
	service := &liveTasks{task: registry.Task{
		Key: event.Subject.Key, Plan: registry.PlanDraft, State: registry.StateAwaitingInput, Turns: 1,
	}}
	engine := newLiveEngine(t, service, liveSelector{host: tasksvc.Host{Name: "builder-a"}})
	run, err := engine.RunEvent(event)
	if err != nil || run == nil || run.State != StateSucceeded {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	if service.turn != 1 {
		t.Fatalf("turn = %d, want one plan turn", service.turn)
	}
	if service.last.Action != rules.ActionPlan {
		t.Fatalf("action = %s, want plan", service.last.Action)
	}
}

// TestOnAStartWithOpenQuestionsIsIgnored: the hard questions guard lives in rules,
// so the flow's evaluate must produce ignore and the task service must not run.
func TestOnAStartWithOpenQuestionsIsIgnored(t *testing.T) {
	event := readEvent(t, "events/comment-start.json")
	service := &liveTasks{task: registry.Task{
		Key: event.Subject.Key, Plan: registry.PlanQuestions, State: registry.StateAwaitingInput, Turns: 2,
	}}
	engine := newLiveEngine(t, service, liveSelector{host: tasksvc.Host{Name: "builder-a"}})
	run, err := engine.RunEvent(event)
	if err != nil || run == nil || run.State != StateSucceeded {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	if service.turn != 0 {
		t.Fatalf("turn = %d, want no turn while questions remain", service.turn)
	}
}

// TestOnAReplanWhileATurnRunsIsIgnored: the flow's task projection must carry the
// running state into rules; a queued revision would arrive too late to help.
func TestOnAReplanWhileATurnRunsIsIgnored(t *testing.T) {
	event := readEvent(t, "events/comment-start.json")
	event.Comment.Text = "/opencode replan"
	service := &liveTasks{task: registry.Task{
		Key: event.Subject.Key, Plan: registry.PlanDraft, State: registry.StateExecuting, Turns: 2,
	}}
	engine := newLiveEngine(t, service, liveSelector{host: tasksvc.Host{Name: "builder-a"}})
	run, err := engine.RunEvent(event)
	if err != nil || run == nil || run.State != StateSucceeded {
		t.Fatalf("run = %+v, err = %v", run, err)
	}
	if service.turn != 0 {
		t.Fatalf("turn = %d, want no replan while executing", service.turn)
	}
}
