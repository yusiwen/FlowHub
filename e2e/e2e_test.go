//go:build e2e

// Package e2e drives a real FlowHub process against the self-hosted YouTrack
// TEST project and the local opencode server. It is opt-in: the normal test
// suite never starts a process that talks to production services.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type env struct {
	t           *testing.T
	dir         string
	dataDir     string
	baseURL     string
	addr        string
	live        bool
	hookKey     string
	hookToken   string
	hubToken    string
	ytBase      string
	ytToken     string
	ytProject   string
	ytProjectID string
	ytIssue     string
	repo        string
	ocBase      string
	ocPass      string
	cmd         *exec.Cmd
	logPath     string
	client      *http.Client
}

var suite *env

func TestMain(m *testing.M) {
	if os.Getenv("FLOWHUB_E2E") != "1" {
		fmt.Fprintln(os.Stderr, "e2e: set FLOWHUB_E2E=1 (or use make e2e)")
		os.Exit(0)
	}
	suite = setup()
	code := m.Run()
	suite.stop()
	os.Exit(code)
}

func setup() *env {
	e := &env{
		live:        os.Getenv("FLOWHUB_E2E_LIVE") == "1",
		hookKey:     os.Getenv("FLOWHUB_HOOK_KEY"),
		hookToken:   os.Getenv("FLOWHUB_TOKEN"),
		hubToken:    fmt.Sprintf("e2e-%d", time.Now().UnixNano()),
		ytBase:      strings.TrimRight(os.Getenv("YOUTRACK_URL"), "/"),
		ytToken:     os.Getenv("YOUTRACK_TOKEN"),
		ytProject:   os.Getenv("YOUTRACK_PROJECT"),
		ytProjectID: os.Getenv("YOUTRACK_PROJECT_ID"),
		ytIssue:     os.Getenv("E2E_ISSUE"),
		repo:        os.Getenv("E2E_REPO"),
		ocBase:      strings.TrimRight(os.Getenv("OPENCODE_URL"), "/"),
		ocPass:      firstNonEmpty(os.Getenv("FLOWHUB_OPENCODE_PASSWORD"), os.Getenv("HOMELAB_OPENCODE_PASSWORD")),
		client:      &http.Client{Timeout: 30 * time.Second},
	}
	if e.ytBase == "" {
		e.ytBase = "https://pm.yusiwen.cn"
	}
	if e.ytProject == "" {
		e.ytProject = "TEST"
	}
	if e.ytProjectID == "" {
		e.ytProjectID = "0-2"
	}
	if e.ocBase == "" {
		e.ocBase = "http://127.0.0.1:4096"
	}
	if e.repo == "" {
		e.repo = "/Users/yusiwen/git/mine/test"
	}
	if e.ytIssue == "" {
		e.ytIssue = "TEST-38"
	}
	required := map[string]string{
		"FLOWHUB_HOOK_KEY": e.hookKey,
		"FLOWHUB_TOKEN":    e.hookToken,
		"YOUTRACK_TOKEN":   e.ytToken,
	}
	for name, value := range required {
		if value == "" {
			fatalf("e2e: %s is required (source /tmp/flowhub-e2e.env and ~/.config/flowhub/.env)", name)
		}
	}
	var err error
	e.dir, err = os.MkdirTemp("", "flowhub-e2e-*")
	if err != nil {
		fatalf("e2e: temp dir: %v", err)
	}
	if e.live {
		e.addr = os.Getenv("E2E_ADDR")
		if e.addr == "" {
			e.addr = "192.168.8.20:8080"
		}
	} else {
		e.addr = "127.0.0.1:18080"
	}
	host, _, _ := strings.Cut(e.addr, ":")
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	e.baseURL = "http://" + host + ":" + port(e.addr)
	e.dataDir = filepath.Join(e.dir, "data")
	e.logPath = filepath.Join(e.dir, "flowhub.log")

	configPath := filepath.Join(e.dir, "config.json")
	if err := writeTestConfig(configPath, e.repo, filepath.Join(e.dir, "worktrees"), e.ytProject); err != nil {
		fatalf("e2e: write config: %v", err)
	}
	binary := os.Getenv("E2E_BIN")
	if binary == "" {
		binary = filepath.Join(repoRoot(), "bin", "flowhub")
	}
	if _, err := os.Stat(binary); err != nil {
		fatalf("e2e: binary %s: %v (run make build)", binary, err)
	}
	e.cmd = exec.Command(binary)
	e.cmd.Dir = repoRoot()
	e.cmd.Env = append(os.Environ(),
		"FLOWHUB_ENV_FILE=-",
		"FLOWHUB_ADDR="+e.addr,
		"FLOWHUB_DATA_DIR="+filepath.Join(e.dir, "data"),
		"FLOWHUB_CONFIG_FILE="+configPath,
		"FLOWHUB_HOOK_KEY="+e.hookKey,
		"FLOWHUB_TOKEN="+e.hookToken,
		"FLOWHUB_HUB_TOKEN="+e.hubToken,
		"FLOWHUB_DISPATCH=1",
		"FLOWHUB_ALLOWED_SOURCES=127.0.0.1,10.1.0.1",
		"FLOWHUB_PAUSE_FILE="+filepath.Join(e.dir, "DISPATCH_OFF"),
		"FLOWHUB_LOG_FILE=none",
		"FLOWHUB_TASK_DEADLINE=5m",
	)
	logFile, err := os.Create(e.logPath)
	if err != nil {
		fatalf("e2e: log: %v", err)
	}
	e.cmd.Stdout = logFile
	e.cmd.Stderr = logFile
	if err := e.cmd.Start(); err != nil {
		fatalf("e2e: start: %v", err)
	}
	if err := e.waitHealthy(15 * time.Second); err != nil {
		data, _ := os.ReadFile(e.logPath)
		fatalf("e2e: FlowHub did not become healthy: %v\n%s", err, tail(string(data), 40))
	}
	return e
}

func (e *env) stop() {
	if e == nil || e.cmd == nil || e.cmd.Process == nil {
		return
	}
	_ = e.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = e.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = e.cmd.Process.Kill()
	}
}

func (e *env) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := e.client.Get(e.baseURL + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("timeout")
}

func TestWebhookDeliversAnIssueUpdate(t *testing.T) {
	issue := suite.ytIssue
	if issue == "" {
		t.Fatal("E2E_ISSUE is empty")
	}
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	description := "FlowHub E2E webhook check " + stamp
	if err := suite.updateIssue(issue, description); err != nil {
		t.Fatalf("update issue: %v", err)
	}
	if suite.live {
		suite.waitForAudit(t, issue, "issueUpdated", 20*time.Second)
	} else {
		payload := issueUpdatedPayload(issue, description, stamp)
		response := suite.postWebhook(t, payload)
		if response != http.StatusAccepted {
			t.Fatalf("webhook status = %d, want 202", response)
		}
		suite.waitForEvent(t, issue, "issue.updated", 10*time.Second)
	}
	counters := suite.counters(t)
	if counters["accepted"] < 1 {
		t.Fatalf("accepted counter = %v", counters["accepted"])
	}
	if suite.live {
		suite.waitForFlowSubmitted(t, 10*time.Second)
	}
}

func TestIssueCreatedRunsOneDispatchTurn(t *testing.T) {
	if os.Getenv("E2E_TURN") != "1" {
		t.Skip("set E2E_TURN=1 to spend a real opencode turn")
	}
	summary := "FlowHub E2E turn " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, id, err := suite.createIssue(summary, "Created by the FlowHub E2E turn test.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Logf("created %s (%s)", key, id)
	if !suite.live {
		if status := suite.postWebhook(t, issueCreatedPayload(key, summary)); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d, want 202", status)
		}
	}
	suite.waitForEvent(t, key, "issue.created", 15*time.Second)
	suite.waitForDispatchHandled(t, 1, 6*time.Minute)
	suite.waitForComment(t, key, "<!-- flowhub-auto -->", 6*time.Minute)
	worktree, state := suite.waitForTurnState(t, key, 2*time.Minute)
	if state == "failed" {
		t.Fatalf("task %s ended failed despite a posted comment", key)
	}
	if count := suite.openCodeSessionsIn(t, worktree); count == 0 {
		t.Fatalf("no opencode session in worktree %s", worktree)
	}
}

func (e *env) createIssue(summary, description string) (key, id string, err error) {
	body := map[string]any{
		"project":     map[string]any{"id": e.ytProjectID},
		"summary":     summary,
		"description": description,
	}
	var response struct {
		ID         string `json:"id"`
		IDReadable string `json:"idReadable"`
	}
	if err := e.ytRequest(http.MethodPost, "/api/issues?fields=id,idReadable", body, &response); err != nil {
		return "", "", err
	}
	return response.IDReadable, response.ID, nil
}

func (e *env) dispatchHandled(t *testing.T) float64 {
	t.Helper()
	var body struct {
		Dispatch struct {
			Handled float64 `json:"handled"`
		} `json:"dispatch"`
	}
	if err := e.getJSON(t, "/healthz", &body); err != nil {
		t.Fatalf("health: %v", err)
	}
	return body.Dispatch.Handled
}

func (e *env) waitForDispatchHandled(t *testing.T, minimum float64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var body struct {
			Dispatch struct {
				Handled float64 `json:"handled"`
			} `json:"dispatch"`
		}
		if e.getJSON(t, "/healthz", &body) == nil && body.Dispatch.Handled >= minimum {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("dispatch handled did not reach %v within %s", minimum, timeout)
}

func (e *env) waitForComment(t *testing.T, issue, contains string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	path := "/api/issues/" + issue + "/comments?fields=id,text,created"
	for time.Now().Before(deadline) {
		var comments []struct {
			Text string `json:"text"`
		}
		if err := e.ytRequest(http.MethodGet, path, nil, &comments); err == nil {
			for _, comment := range comments {
				if strings.Contains(comment.Text, contains) {
					return
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no comment containing %q on %s within %s", contains, issue, timeout)
}

func (e *env) waitForTurnState(t *testing.T, key string, timeout time.Duration) (worktree, state string) {
	t.Helper()
	path := filepath.Join(e.dataDir, "registry.jsonl")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var row struct {
					TaskKey  string `json:"task_key"`
					Worktree string `json:"worktree"`
					Session  string `json:"session_id"`
					State    string `json:"state"`
				}
				if json.Unmarshal([]byte(line), &row) == nil && row.TaskKey == key && row.Session != "" {
					if row.State != "analyzing" && row.State != "planning" && row.State != "executing" {
						return row.Worktree, row.State
					}
					worktree, state = row.Worktree, row.State
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("task %s did not reach a terminal state (last %q)", key, state)
	return "", ""
}

func (e *env) openCodeSessionsIn(t *testing.T, directory string) int {
	t.Helper()
	if e.ocPass == "" {
		t.Skip("no opencode password; cannot inspect sessions")
	}
	endpoint := e.ocBase + "/api/session?directory=" + url.QueryEscape(directory) + "&limit=200"
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("opencode sessions: %v", err)
	}
	request.SetBasicAuth("opencode", e.ocPass)
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatalf("opencode sessions: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("opencode sessions: %d", response.StatusCode)
	}
	var body struct {
		Data []any `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("opencode sessions decode: %v", err)
	}
	return len(body.Data)
}

func issueCreatedPayload(key, summary string) []byte {
	payload := map[string]any{
		"event":       "issueCreated",
		"timestamp":   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"id":          key,
		"summary":     summary,
		"project":     map[string]any{"key": "TEST", "name": "TEST", "shortName": "TEST"},
		"description": "Created by the FlowHub E2E turn test.",
		"created":     time.Now().UnixMilli(),
		"reporter":    map[string]any{"login": "yusiwen", "fullName": "Siwen Yu"},
	}
	data, _ := json.Marshal(payload)
	return data
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func TestShadowComparisonOnIssueUpdate(t *testing.T) {
	if os.Getenv("E2E_FLOW") != "1" {
		t.Skip("set E2E_FLOW=1 to run shadow/on checks")
	}
	t.Cleanup(func() { _ = suite.setMode("off", nil) })
	if err := suite.setMode("shadow", nil); err != nil {
		t.Fatalf("set shadow: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	before := suite.flowHealth(t)
	updateIssueDescription := "FlowHub E2E shadow " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if err := suite.updateIssue(suite.ytIssue, updateIssueDescription); err != nil {
		t.Fatalf("update issue: %v", err)
	}
	if suite.live {
		suite.waitForAudit(t, suite.ytIssue, "issueUpdated", 20*time.Second)
	} else {
		payload := issueUpdatedPayload(suite.ytIssue, updateIssueDescription, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
		if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
	}
	suite.waitForFlowHealth(t, func(f flowHealth) bool {
		return f.Runs > before.Runs && f.ShadowCompared > before.ShadowCompared
	}, 20*time.Second)
	health := suite.flowHealth(t)
	if health.ShadowMismatches != 0 {
		t.Fatalf("shadow mismatches = %v", health.ShadowMismatches)
	}
}

func TestOnExecutesAndRollsBack(t *testing.T) {
	if os.Getenv("E2E_FLOW") != "1" {
		t.Skip("set E2E_FLOW=1 to run shadow/on checks")
	}
	t.Cleanup(func() { _ = suite.setMode("off", nil) })
	if err := suite.setMode("on", []string{"TEST"}); err != nil {
		t.Fatalf("set on: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	before := suite.flowHealth(t)
	summary := "FlowHub E2E on " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, "Created by the FlowHub E2E on test.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if !suite.live {
		if status := suite.postWebhook(t, issueCreatedPayload(key, summary)); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
	}
	suite.waitForComment(t, key, "<!-- flowhub-auto -->", 6*time.Minute)
	suite.waitForFlowHealth(t, func(f flowHealth) bool { return f.Runs > before.Runs }, 30*time.Second)
	worktree, state := suite.waitForTurnState(t, key, 2*time.Minute)
	if state == "failed" {
		t.Fatalf("on task %s ended failed", key)
	}
	if count := suite.openCodeSessionsIn(t, worktree); count == 0 {
		t.Fatalf("no opencode session in on worktree %s", worktree)
	}
	if !suite.hasRunMode(t, "on") {
		t.Fatal("no on run appeared in /hub/v1/runs")
	}
	// Roll back and prove dispatch takes the next delivery again.
	handledBefore := suite.dispatchHandled(t)
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("rollback off: %v", err)
	}
	description := "FlowHub E2E rollback " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if err := suite.updateIssue(suite.ytIssue, description); err != nil {
		t.Fatalf("rollback update: %v", err)
	}
	if !suite.live {
		if status := suite.postWebhook(t, issueUpdatedPayload(suite.ytIssue, description, time.Now().UTC().Format("2006-01-02T15:04:05Z"))); status != http.StatusAccepted {
			t.Fatalf("rollback webhook status = %d", status)
		}
	}
	suite.waitForDispatchHandled(t, handledBefore+1, 10*time.Second)
}

type flowHealth struct {
	Runs             float64
	Submitted        float64
	ShadowCompared   float64
	ShadowMismatches float64
}

func (e *env) flowHealth(t *testing.T) flowHealth {
	t.Helper()
	var body struct {
		Flows struct {
			Runs             float64 `json:"runs"`
			Submitted        float64 `json:"submitted"`
			ShadowCompared   float64 `json:"shadow_compared"`
			ShadowMismatches float64 `json:"shadow_mismatches"`
		} `json:"flows"`
	}
	if err := e.getJSON(t, "/healthz", &body); err != nil {
		t.Fatalf("health: %v", err)
	}
	return flowHealth{Runs: body.Flows.Runs, Submitted: body.Flows.Submitted,
		ShadowCompared: body.Flows.ShadowCompared, ShadowMismatches: body.Flows.ShadowMismatches}
}

func (e *env) waitForFlowHealth(t *testing.T, ok func(flowHealth) bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok(e.flowHealth(t)) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("flow health did not reach the expected state within %s", timeout)
}

func (e *env) setMode(mode string, projects []string) error {
	body := map[string]any{"mode": mode}
	if projects != nil {
		body["projects"] = projects
	}
	data, _ := json.Marshal(body)
	request, err := http.NewRequest(http.MethodPost, e.baseURL+"/hub/v1/flows/default-youtrack/deployment", bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+e.hubToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := e.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("deployment %s: %d %s", mode, response.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (e *env) hasRunMode(t *testing.T, want string) bool {
	t.Helper()
	var body struct {
		Runs []struct {
			Mode string `json:"mode"`
		} `json:"runs"`
	}
	request, err := http.NewRequest(http.MethodGet, e.baseURL+"/hub/v1/runs?limit=50", nil)
	if err != nil {
		t.Fatalf("runs request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+e.hubToken)
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("runs: %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("runs decode: %v", err)
	}
	for _, run := range body.Runs {
		if run.Mode == want {
			return true
		}
	}
	return false
}

func (e *env) updateIssue(issue, description string) error {
	body := map[string]any{"description": description}
	return e.ytRequest(http.MethodPost, "/api/issues/"+issue+"?fields=id,idReadable,description", body, nil)
}

func (e *env) postWebhook(t *testing.T, payload []byte) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, e.baseURL+"/hooks/youtrack/"+e.hookKey, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-YouTrack-Token", e.hookToken)
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func (e *env) waitForAudit(t *testing.T, issue, kind string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	path := filepath.Join(e.dir, "data", "webhook-"+time.Now().UTC().Format("2006-01-02")+".jsonl")
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var record struct {
					Event   string `json:"event"`
					IssueID string `json:"issue_id"`
				}
				if json.Unmarshal([]byte(line), &record) == nil && record.Event == kind && record.IssueID == issue {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no %s audit for %s within %s", kind, issue, timeout)
}

func (e *env) waitForEvent(t *testing.T, issue, kind string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	path := filepath.Join(e.dir, "data", "events", time.Now().UTC().Format("2006-01-02")+".jsonl")
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var record struct {
					Kind    string `json:"kind"`
					Subject struct {
						Key string `json:"key"`
					} `json:"subject"`
				}
				if json.Unmarshal([]byte(line), &record) == nil && record.Kind == kind && record.Subject.Key == issue {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no %s event for %s within %s", kind, issue, timeout)
}

func (e *env) counters(t *testing.T) map[string]float64 {
	t.Helper()
	var body struct {
		Counters map[string]float64 `json:"counters"`
	}
	e.getJSON(t, "/healthz", &body)
	return body.Counters
}

func (e *env) waitForFlowSubmitted(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var body struct {
			Flows struct {
				Submitted float64 `json:"submitted"`
			} `json:"flows"`
		}
		if e.getJSON(t, "/healthz", &body) == nil && body.Flows.Submitted >= 1 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("flow did not receive the event within %s", timeout)
}

func (e *env) getJSON(t *testing.T, path string, out any) error {
	t.Helper()
	response, err := e.client.Get(e.baseURL + path)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %d", path, response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(out)
}

func (e *env) ytRequest(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, e.ytBase+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+e.ytToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := e.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("%s %s: %d %s", method, path, response.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.NewDecoder(response.Body).Decode(out)
	}
	return nil
}

func issueUpdatedPayload(issue, description, timestamp string) []byte {
	payload := map[string]any{
		"event":       "issueUpdated",
		"timestamp":   timestamp,
		"id":          issue,
		"summary":     "FlowHub E2E webhook check",
		"project":     map[string]any{"key": "TEST", "name": "TEST", "shortName": "TEST"},
		"description": description,
		"updated":     time.Now().UnixMilli(),
		"updatedBy":   map[string]any{"login": "yusiwen", "fullName": "Siwen Yu"},
		"changedFields": []map[string]any{{
			"name": "description", "value": description,
		}},
	}
	data, _ := json.Marshal(payload)
	return data
}

func writeTestConfig(path, repo, worktrees, project string) error {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".config", "flowhub", "config.json"))
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	projects, _ := raw["projects"].([]any)
	var kept []any
	for _, item := range projects {
		entry, ok := item.(map[string]any)
		if ok && entry["project"] == project {
			entry["repo"].(map[string]any)["path"] = repo
			entry["worktrees"] = worktrees
			kept = append(kept, entry)
		}
	}
	if len(kept) == 0 {
		return fmt.Errorf("project %q is not in the routing table", project)
	}
	raw["projects"] = kept
	if err := os.MkdirAll(worktrees, 0o700); err != nil {
		return err
	}
	data, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func repoRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func port(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

func tail(text string, lines int) string {
	all := strings.Split(strings.TrimSpace(text), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
