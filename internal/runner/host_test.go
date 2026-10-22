package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
	"github.com/yusiwen/flowhub/internal/runner"
)

type fakeRuntime struct {
	turn   agent.Turn
	result agent.Result
}

func (f *fakeRuntime) Name() string { return "fake" }

func (f *fakeRuntime) Health(context.Context) (string, error) { return "1.0", nil }

func (f *fakeRuntime) Run(_ context.Context, turn agent.Turn) (agent.Result, error) {
	f.turn = turn
	return f.result, nil
}

func TestHostRunCarriesTheTurn(t *testing.T) {
	runtime := &fakeRuntime{result: agent.Result{SessionID: "ses_new", Finished: true}}
	host := runner.Host{
		Name: "builder", Runtime: runtime,
		Agent: "devops", Model: "provider/model", Deadline: 10 * time.Minute,
	}
	result, err := host.Run(context.Background(), runner.Turn{
		Directory:    "/work/task",
		Prompt:       "do the thing",
		Title:        "TEST-12",
		SessionID:    "ses_old",
		Phase:        agent.PhaseExecution,
		AllowedTools: []string{"read", "edit"},
		Downloads:    agent.Downloads{Hosts: []string{"files.example"}, Prefix: ".flowhub/attachments"},
		Granted:      []string{"git status"},
		Metadata:     map[string]any{"task_key": "TEST-12"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.SessionID != "ses_new" || !result.Finished {
		t.Fatalf("result = %+v", result)
	}
	got := runtime.turn
	if got.Agent != "devops" || got.Model != "provider/model" {
		t.Fatalf("agent/model = %q/%q", got.Agent, got.Model)
	}
	if got.Deadline != 10*time.Minute {
		t.Fatalf("deadline = %s", got.Deadline)
	}
	if got.Directory != "/work/task" || got.Prompt != "do the thing" || got.Title != "TEST-12" {
		t.Fatalf("turn = %+v", got)
	}
	if got.SessionID != "ses_old" || got.Phase != agent.PhaseExecution {
		t.Fatalf("session/phase = %q/%q", got.SessionID, got.Phase)
	}
	if len(got.AllowedTools) != 2 || len(got.Granted) != 1 || got.Metadata["task_key"] != "TEST-12" {
		t.Fatalf("tools/grants/metadata = %+v", got)
	}
	if len(got.Downloads.Hosts) != 1 || got.Downloads.Prefix != ".flowhub/attachments" {
		t.Fatalf("downloads = %+v", got.Downloads)
	}
}

func TestTurnOverridesHostDefaults(t *testing.T) {
	runtime := &fakeRuntime{}
	host := runner.Host{
		Name: "builder", Runtime: runtime,
		Agent: "devops", Model: "provider/default", Deadline: 10 * time.Minute,
	}
	if _, err := host.Run(context.Background(), runner.Turn{
		Agent: "reviewer", Model: "provider/other", Deadline: 2 * time.Minute,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if runtime.turn.Agent != "reviewer" || runtime.turn.Model != "provider/other" {
		t.Fatalf("agent/model = %q/%q", runtime.turn.Agent, runtime.turn.Model)
	}
	if runtime.turn.Deadline != 2*time.Minute {
		t.Fatalf("deadline = %s", runtime.turn.Deadline)
	}
}

func TestHostWithoutRuntimeFails(t *testing.T) {
	_, err := (runner.Host{Name: "missing"}).Run(context.Background(), runner.Turn{})
	if err == nil {
		t.Fatal("expected an error")
	}
}
