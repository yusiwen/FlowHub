package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/flow/api"
	"github.com/yusiwen/flowhub/internal/flow/builtin"
	"github.com/yusiwen/flowhub/internal/flow/engine"
	"github.com/yusiwen/flowhub/internal/flow/mode"
	"github.com/yusiwen/flowhub/internal/upstream"
)

type fixture struct {
	handler *api.Handler
	modes   *mode.Store
	runs    *engine.Store
	flowID  string
}

func newFixture(t *testing.T, token string) fixture {
	t.Helper()
	compiled, raw, err := builtin.Load()
	if err != nil {
		t.Fatalf("builtin: %v", err)
	}
	dir := t.TempDir()
	modes, err := mode.Open(dir)
	if err != nil {
		t.Fatalf("mode: %v", err)
	}
	runs, err := engine.NewStore(dir)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	handler, err := api.New(api.Options{
		Token: token, Flow: compiled, Definition: raw, Modes: modes, Runs: runs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	return fixture{handler: handler, modes: modes, runs: runs, flowID: compiled.Definition.ID}
}

func seedRun(t *testing.T, runs *engine.Store, id, state string) *engine.Run {
	t.Helper()
	started := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Second)
	run := &engine.Run{
		ID: id, Flow: "default-youtrack", Revision: "rev", Mode: "shadow",
		State: engine.State(state), Trigger: "all-youtrack-events",
		Event: upstream.Event{
			ID: "evt_" + id, Upstream: "youtrack", Kind: "comment.added",
			Subject: upstream.Subject{Project: "TEST", Key: "TEST-12"},
		},
		StartedAt: started, FinishedAt: &finished,
		Steps: map[string]*engine.Step{
			"evaluate": {ID: "evaluate", Type: "task.evaluate", State: engine.StepSucceeded,
				Output: map[string]any{"action": "analyze", "reason": "test"}},
		},
	}
	if err := runs.Create(run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := runs.Append(run.ID, "run.started", map[string]any{"flow": run.Flow}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := runs.Append(run.ID, "run.finished", map[string]any{"state": state}); err != nil {
		t.Fatalf("append: %v", err)
	}
	return run
}

func request(t *testing.T, handler http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, recorder.Body.String())
	}
	return body
}

func TestDisabledWithoutTokenReturns503(t *testing.T) {
	f := newFixture(t, "")
	recorder := request(t, f.handler, "/hub/v1/flows", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestTokenIsRequired(t *testing.T) {
	f := newFixture(t, "sekret")
	if recorder := request(t, f.handler, "/hub/v1/flows", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d", recorder.Code)
	}
	if recorder := request(t, f.handler, "/hub/v1/flows", "wrong"); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", recorder.Code)
	}
	if recorder := request(t, f.handler, "/hub/v1/ping", "sekret"); recorder.Code != http.StatusOK {
		t.Fatalf("ping status = %d", recorder.Code)
	}
}

func TestFlowsAndDefinition(t *testing.T) {
	f := newFixture(t, "sekret")
	if err := f.modes.Set(f.flowID, mode.Shadow, "test"); err != nil {
		t.Fatalf("mode: %v", err)
	}
	seedRun(t, f.runs, "run_1", string(engine.StateSucceeded))
	body := decode(t, request(t, f.handler, "/hub/v1/flows", "sekret"))
	flows, ok := body["flows"].([]any)
	if !ok || len(flows) != 1 {
		t.Fatalf("flows = %+v", body["flows"])
	}
	summary := flows[0].(map[string]any)
	if summary["id"] != f.flowID || summary["mode"] != mode.Shadow {
		t.Fatalf("summary = %+v", summary)
	}
	if summary["last_run"] == nil {
		t.Fatal("last_run is missing")
	}

	definition := decode(t, request(t, f.handler, "/hub/v1/flows/"+f.flowID, "sekret"))
	if definition["mode"] != mode.Shadow || definition["definition"] == nil {
		t.Fatalf("definition = %+v", definition)
	}
	if recorder := request(t, f.handler, "/hub/v1/flows/unknown", "sekret"); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown flow status = %d", recorder.Code)
	}
}

func TestRunsFilterAndCursor(t *testing.T) {
	f := newFixture(t, "sekret")
	seedRun(t, f.runs, "run_1", string(engine.StateSucceeded))
	seedRun(t, f.runs, "run_2", string(engine.StateFailed))
	seedRun(t, f.runs, "run_3", string(engine.StateSucceeded))

	first := decode(t, request(t, f.handler, "/hub/v1/runs?limit=1", "sekret"))
	runs := first["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	cursor, _ := first["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("next_cursor is empty")
	}
	second := decode(t, request(t, f.handler, "/hub/v1/runs?limit=1&cursor="+cursor, "sekret"))
	if len(second["runs"].([]any)) != 1 {
		t.Fatalf("second page = %+v", second)
	}
	failed := decode(t, request(t, f.handler, "/hub/v1/runs?state=failed", "sekret"))
	if got := len(failed["runs"].([]any)); got != 1 {
		t.Fatalf("failed runs = %d", got)
	}
	summary := failed["runs"].([]any)[0].(map[string]any)
	if summary["action"] != "analyze" || summary["task"] != "TEST-12" {
		t.Fatalf("summary = %+v", summary)
	}
	if recorder := request(t, f.handler, "/hub/v1/runs/unknown", "sekret"); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown run status = %d", recorder.Code)
	}
}

func TestRunEventsAfterSequence(t *testing.T) {
	f := newFixture(t, "sekret")
	seedRun(t, f.runs, "run_1", string(engine.StateSucceeded))
	body := decode(t, request(t, f.handler, "/hub/v1/runs/run_1/events?after=1", "sekret"))
	events, ok := body["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("events = %+v", body["events"])
	}
	if body["next"].(float64) != 2 {
		t.Fatalf("next = %v", body["next"])
	}
	if recorder := request(t, f.handler, "/hub/v1/runs/run_1/events?after=2", "sekret"); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
}
