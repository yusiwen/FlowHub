package engine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/flow/builtin"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/registry"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/source/youtrack"
	"github.com/yusiwen/flowhub/internal/upstream"
)

type fakeRouter struct {
	projects map[string]bool
}

func (f fakeRouter) Match(source, project string) (projectmap.Match, bool) {
	if !f.projects[project] {
		return projectmap.Match{}, false
	}
	return projectmap.Match{
		Source: source,
		Key:    project,
		Entry:  &projectmap.Entry{Source: source, Project: project},
	}, true
}

type fakeTasks struct {
	tasks map[string]registry.Task
}

func (f fakeTasks) Get(_ string, key string) (registry.Task, bool) {
	task, ok := f.tasks[key]
	return task, ok
}

type parityCase struct {
	Name    string `json:"name"`
	Event   string `json:"event"`
	Compare string `json:"compare"`
	Task    *struct {
		Plan      string             `json:"plan"`
		Turns     int                `json:"turns"`
		LastReply string             `json:"last_reply"`
		Refusals  []registry.Refusal `json:"refusals"`
	} `json:"task"`
	Expected struct {
		Action         string `json:"action"`
		ReasonContains string `json:"reason_contains"`
	} `json:"expected"`
}

func readParityCases(t *testing.T) []parityCase {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/flows/parity-cases.json")
	if err != nil {
		t.Fatalf("read parity cases: %v", err)
	}
	var file struct {
		Cases []parityCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("decode parity cases: %v", err)
	}
	return file.Cases
}

func readEvent(t *testing.T, name string) upstream.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../../../testdata/flows", name))
	if err != nil {
		t.Fatalf("read event %s: %v", name, err)
	}
	var event upstream.Event
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("decode event %s: %v", name, err)
	}
	return event
}

func newShadowEngine(t *testing.T, tasks map[string]registry.Task) *Engine {
	t.Helper()
	compiled, raw, err := builtin.Load()
	if err != nil {
		t.Fatalf("load builtin: %v", err)
	}
	engine, err := New(Options{
		Flow:       compiled,
		Definition: raw,
		Projects:   fakeRouter{projects: map[string]bool{"TEST": true, "SP": true}},
		Tasks:      fakeTasks{tasks: tasks},
		Policy:     youtrack.New(rules.Policy{}, "").Policy(),
		DataDir:    t.TempDir(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        func() time.Time { return time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return engine
}

func TestParityCases(t *testing.T) {
	policy := youtrack.New(rules.Policy{}, "").Policy()
	cases := readParityCases(t)
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			event := readEvent(t, test.Event)
			tasks := map[string]registry.Task{}
			view := rules.TaskView{}
			if test.Task != nil {
				plan := registry.PlanState(test.Task.Plan)
				if plan == "" {
					plan = registry.PlanNone
				}
				task := registry.Task{
					Source: "youtrack", Key: event.Subject.Key,
					Plan: plan, Turns: test.Task.Turns,
					LastReply: test.Task.LastReply, Refusals: test.Task.Refusals,
				}
				tasks[event.Subject.Key] = task
				view = rules.TaskView{
					Known: true, Plan: plan, Turns: task.Turns,
					LastReply: task.LastReply, Refusals: task.Refusals,
				}
			}
			engine := newShadowEngine(t, tasks)
			run, err := engine.RunEvent(event)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if run == nil {
				t.Fatal("no run was created")
			}
			step := run.Steps["evaluate"]
			if step == nil {
				t.Fatalf("no evaluate step: %+v", run.Steps)
			}
			action, _ := step.Output["action"].(string)
			reason, _ := step.Output["reason"].(string)
			if action != test.Expected.Action {
				t.Fatalf("action = %q, want %q (reason %q)", action, test.Expected.Action, reason)
			}
			if !strings.Contains(reason, test.Expected.ReasonContains) {
				t.Fatalf("reason %q does not contain %q", reason, test.Expected.ReasonContains)
			}
			if test.Compare == "routing" {
				return
			}
			decision := policy.Decide(neutralEvent(event), view)
			if string(decision.Action) != test.Expected.Action {
				t.Fatalf("policy action = %q, flow action = %q", decision.Action, action)
			}
			if !strings.Contains(decision.Reason, test.Expected.ReasonContains) {
				t.Fatalf("policy reason %q does not contain %q", decision.Reason, test.Expected.ReasonContains)
			}
		})
	}
}

func TestShadowSkipsTurnsAndTouchesNothing(t *testing.T) {
	event := readEvent(t, "events/comment-start.json")
	tasks := map[string]registry.Task{
		event.Subject.Key: {
			Source: "youtrack", Key: event.Subject.Key,
			Plan: registry.PlanDraft, Turns: 1,
		},
	}
	engine := newShadowEngine(t, tasks)
	run, err := engine.RunEvent(event)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Mode != "shadow" || run.State != StateSucceeded {
		t.Fatalf("run = %s/%s", run.Mode, run.State)
	}
	step := run.Steps["execute"]
	if step == nil || step.State != StepSkipped {
		t.Fatalf("execute step = %+v", step)
	}
	if step.Output != nil {
		t.Fatalf("shadow turn produced output: %+v", step.Output)
	}
}

func TestStoreRecoversAnInterruptedRun(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	run := &Run{
		ID: "run_interrupted", Flow: "default-youtrack", State: StateRunning,
		StartedAt: time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC),
		Steps: map[string]*Step{
			"evaluate": {ID: "evaluate", Type: "task.evaluate", State: StepRunning},
		},
	}
	if err := store.Create(run); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Recover(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	loaded, err := store.Load(run.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.State != StateNeedsAttention || loaded.Error == "" {
		t.Fatalf("recovered run = %+v", loaded)
	}
	if step := loaded.Steps["evaluate"]; step == nil || step.State != StepInterrupted {
		t.Fatalf("recovered step = %+v", loaded.Steps["evaluate"])
	}
	journal, err := os.ReadFile(filepath.Join(store.Dir(), run.ID, "journal.jsonl"))
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if !strings.Contains(string(journal), "run.interrupted") {
		t.Fatalf("journal = %s", journal)
	}
}

func TestEngineSubmitProcessesAndCloses(t *testing.T) {
	event := readEvent(t, "events/issue-created.json")
	engine := newShadowEngine(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)
	engine.Submit(event)

	deadline := time.Now().Add(2 * time.Second)
	for {
		runs, err := engine.store.List()
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		if len(runs) == 1 && runs[0].State == StateSucceeded {
			if runs[0].Mode != "shadow" {
				t.Fatalf("run = %+v", runs[0])
			}
			break
		}
		if len(runs) == 1 && runs[0].State == StateFailed {
			t.Fatalf("run failed: %+v", runs[0])
		}
		if time.Now().After(deadline) {
			t.Fatalf("no run was written; snapshot = %+v", engine.Snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
