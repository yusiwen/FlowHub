//go:build e2e

// Package e2e drives a real FlowHub process against a configured YouTrack test
// project and a local opencode server. It is opt-in: the normal test
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/upstream"
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
	ytClient    *http.Client
	binary      string
	configPath  string
	cmd         *exec.Cmd
	extraEnv    []string
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
		ytClient:    &http.Client{Timeout: 60 * time.Second},
	}
	if e.ocBase == "" {
		e.ocBase = "http://127.0.0.1:4096"
	}
	required := map[string]string{
		"FLOWHUB_HOOK_KEY": e.hookKey,
		"FLOWHUB_TOKEN":    e.hookToken,
		"YOUTRACK_URL":     e.ytBase,
		"YOUTRACK_TOKEN":   e.ytToken,
		"YOUTRACK_PROJECT": e.ytProject,
		"E2E_REPO":         e.repo,
		"E2E_ISSUE":        e.ytIssue,
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
			fatalf("e2e: E2E_ADDR is required in live mode (the address the YouTrack app posts to)")
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

	configPath := filepath.Join(e.dir, "config.json")
	promptFile := ""
	if os.Getenv("E2E_PERMIT") == "1" {
		// The permit test needs a permission refusal. Issue text is untrusted, so
		// asking the model there is correctly ignored; a project-level prompt file
		// is the operator's trusted instruction and can require the inspection.
		promptFile = filepath.Join(e.dir, "permit-prompt.md")
		if err := os.WriteFile(promptFile, []byte("Before answering, run this shell command with the shell tool and record its output: git worktree list\n"), 0o600); err != nil {
			fatalf("e2e: permit prompt: %v", err)
		}
	}
	if err := writeTestConfig(configPath, e.repo, filepath.Join(e.dir, "worktrees"), e.ytProject, promptFile, e.ocBase); err != nil {
		fatalf("e2e: write config: %v", err)
	}
	e.configPath = configPath
	if os.Getenv("E2E_CLEANUP") == "1" {
		return e
	}
	e.binary = os.Getenv("E2E_BIN")
	if e.binary == "" {
		e.binary = filepath.Join(repoRoot(), "bin", "flowhub")
	}
	if _, err := os.Stat(e.binary); err != nil {
		fatalf("e2e: binary %s: %v (run make build)", e.binary, err)
	}
	if os.Getenv("E2E_SILENT") == "1" || os.Getenv("E2E_DOWN") == "1" || os.Getenv("E2E_POLICY") == "1" || os.Getenv("E2E_SERIALIZATION") == "1" || os.Getenv("E2E_CRASH_EXECUTE") == "1" || os.Getenv("E2E_CRASH_ON") == "1" || os.Getenv("E2E_INGRESS") == "1" || os.Getenv("E2E_ATTACHMENTS") == "1" || os.Getenv("E2E_LIMITS") == "1" || os.Getenv("E2E_FLOW_CONTROL") == "1" || os.Getenv("E2E_API") == "1" || os.Getenv("E2E_ON_RESTART") == "1" || os.Getenv("E2E_AGED_CURSOR") == "1" || os.Getenv("E2E_REBIND") == "1" || os.Getenv("E2E_CAPACITY") == "1" || os.Getenv("E2E_CANCEL") == "1" || os.Getenv("E2E_FLOW_CONCURRENCY") == "1" || os.Getenv("E2E_FLOW_SERIAL") == "1" {
		// Failure-path tests start their own fake (or no) runtime; starting the real
		// receiver first would make them depend on opencode.
		return e
	}
	e.start()
	return e
}

func (e *env) start() {
	if e.cmd != nil && e.cmd.Process != nil {
		e.stop()
	}
	e.cmd = e.command()
	e.cmd.Stdout = os.Stdout
	e.cmd.Stderr = os.Stderr
	if err := e.cmd.Start(); err != nil {
		fatalf("e2e: start: %v", err)
	}
	if err := e.waitHealthy(15 * time.Second); err != nil {
		fatalf("e2e: FlowHub did not become healthy: %v", err)
	}
}

func (e *env) command() *exec.Cmd {
	cmd := exec.Command(e.binary)
	cmd.Dir = repoRoot()
	env := []string{
		"FLOWHUB_ENV_FILE=-",
		"FLOWHUB_ADDR=" + e.addr,
		"FLOWHUB_DATA_DIR=" + filepath.Join(e.dir, "data"),
		"FLOWHUB_CONFIG_FILE=" + e.configPath,
		"FLOWHUB_HOOK_KEY=" + e.hookKey,
		"FLOWHUB_TOKEN=" + e.hookToken,
		"FLOWHUB_HUB_TOKEN=" + e.hubToken,
		"FLOWHUB_DISPATCH=1",
		"FLOWHUB_OPENCODE_URL=" + e.ocBase,
		"FLOWHUB_ALLOWED_SOURCES=127.0.0.1,10.1.0.1",
		"FLOWHUB_PAUSE_FILE=" + filepath.Join(e.dir, "DISPATCH_OFF"),
		"FLOWHUB_LOG_FILE=none",
		"FLOWHUB_TASK_DEADLINE=5m",
	}
	env = append(env, e.extraEnv...)
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

// startExpectFailure proves the process refuses to start, rather than becoming
// healthy and failing at the first task. The output is returned for the assertion.
func (e *env) startExpectFailure(t *testing.T, timeout time.Duration) string {
	t.Helper()
	cmd := e.command()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FlowHub started despite the runtime being unavailable")
		}
		return out.String()
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("FlowHub did not refuse the unavailable runtime within %s:\n%s", timeout, out.String())
		return ""
	}
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
	e.cmd = nil
}

func (e *env) kill() {
	if e == nil || e.cmd == nil || e.cmd.Process == nil {
		return
	}
	_ = e.cmd.Process.Kill()
	_, _ = e.cmd.Process.Wait()
	e.cmd = nil
}

func (e *env) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := e.client.Get(e.baseURL + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				e.client.CloseIdleConnections()
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
		payload := issueUpdatedPayload(issue, description, stamp, suite.ytProject)
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
		if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Created by the FlowHub E2E turn test.", suite.ytProject)); status != http.StatusAccepted {
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
	if e.ytProjectID == "" {
		if err := e.resolveProjectID(); err != nil {
			return "", "", err
		}
	}
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

func (e *env) dispatchIgnored(t *testing.T) float64 {
	t.Helper()
	var body struct {
		Dispatch struct {
			Ignored float64 `json:"ignored"`
		} `json:"dispatch"`
	}
	if err := e.getJSON(t, "/healthz", &body); err != nil {
		t.Fatalf("health: %v", err)
	}
	return body.Dispatch.Ignored
}

func (e *env) dispatchPaused(t *testing.T) bool {
	t.Helper()
	var body struct {
		Dispatch struct {
			Paused bool `json:"paused"`
		} `json:"dispatch"`
	}
	if err := e.getJSON(t, "/healthz", &body); err != nil {
		t.Fatalf("health: %v", err)
	}
	return body.Dispatch.Paused
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

func (e *env) resolveProjectID() error {
	var projects []struct {
		ID        string `json:"id"`
		ShortName string `json:"shortName"`
	}
	if err := e.ytRequest(http.MethodGet, "/api/admin/projects?fields=id,shortName&$top=100", nil, &projects); err != nil {
		return err
	}
	for _, project := range projects {
		if project.ShortName == e.ytProject {
			e.ytProjectID = project.ID
			return nil
		}
	}
	return fmt.Errorf("YouTrack project %q was not found", e.ytProject)
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

func issueCreatedPayload(key, summary, description, project string) []byte {
	return issueCreatedPayloadBy(key, summary, description, project, "yusiwen")
}

func issueCreatedPayloadBy(key, summary, description, project, reporter string) []byte {
	payload := map[string]any{
		"event":       "issueCreated",
		"timestamp":   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"id":          key,
		"summary":     summary,
		"project":     map[string]any{"key": project, "name": project, "shortName": project},
		"description": description,
		"created":     time.Now().UnixMilli(),
		"reporter":    map[string]any{"login": reporter, "fullName": reporter},
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
		payload := issueUpdatedPayload(suite.ytIssue, updateIssueDescription, time.Now().UTC().Format("2006-01-02T15:04:05Z"), suite.ytProject)
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
	if err := suite.setMode("on", []string{suite.ytProject}); err != nil {
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
		if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Created by the FlowHub E2E on test.", suite.ytProject)); status != http.StatusAccepted {
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
		if status := suite.postWebhook(t, issueUpdatedPayload(suite.ytIssue, description, time.Now().UTC().Format("2006-01-02T15:04:05Z"), suite.ytProject)); status != http.StatusAccepted {
			t.Fatalf("rollback webhook status = %d", status)
		}
	}
	suite.waitForDispatchHandled(t, handledBefore+1, 10*time.Second)
}

type flowHealth struct {
	Mode             string
	Runs             float64
	Submitted        float64
	Pending          float64
	Dropped          float64
	ShadowCompared   float64
	ShadowMismatches float64
}

func (e *env) flowHealth(t *testing.T) flowHealth {
	t.Helper()
	var body struct {
		Flows struct {
			Mode             string  `json:"mode"`
			Runs             float64 `json:"runs"`
			Submitted        float64 `json:"submitted"`
			Pending          float64 `json:"pending"`
			Dropped          float64 `json:"dropped"`
			ShadowCompared   float64 `json:"shadow_compared"`
			ShadowMismatches float64 `json:"shadow_mismatches"`
		} `json:"flows"`
	}
	if err := e.getJSON(t, "/healthz", &body); err != nil {
		t.Fatalf("health: %v", err)
	}
	return flowHealth{Mode: body.Flows.Mode, Runs: body.Flows.Runs, Submitted: body.Flows.Submitted, Pending: body.Flows.Pending, Dropped: body.Flows.Dropped,
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

func TestCommentTriggerStartsAPlan(t *testing.T) {
	if os.Getenv("E2E_COMMENT") != "1" {
		t.Skip("set E2E_COMMENT=1 to spend a real comment-trigger turn")
	}
	if suite.live {
		t.Skip("comment trigger currently runs in direct mode; issueCreated would race the webhook")
	}
	t.Cleanup(func() { _ = suite.setMode("off", nil) })
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}
	baseline := suite.dispatchHandled(t)
	summary := "FlowHub E2E trigger " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, "Comment trigger E2E.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	comment, err := suite.addComment(key, "/opencode start")
	if err != nil {
		t.Fatalf("add comment: %v", err)
	}
	if status := suite.postWebhook(t, commentAddedPayload(key, comment, "/opencode start", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("webhook status = %d", status)
	}
	suite.waitForDispatchHandled(t, baseline+1, 6*time.Minute)
	suite.waitForComment(t, key, "<!-- flowhub-auto -->", 6*time.Minute)
	suite.waitForTurnState(t, key, 2*time.Minute)
}

func TestDuplicateWebhookIsIdempotent(t *testing.T) {
	if suite.live {
		t.Skip("direct duplicate injection is checked in direct mode")
	}
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}
	beforeDuplicates := suite.counters(t)["duplicates"]
	beforeEvents := eventLineCount(t, suite.dataDir)
	payload := issueUpdatedPayload(suite.ytIssue, "FlowHub duplicate "+time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format("2006-01-02T15:04:05Z"), suite.ytProject)
	if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
		t.Fatalf("first status = %d", status)
	}
	if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
		t.Fatalf("second status = %d", status)
	}
	if got := suite.counters(t)["duplicates"]; got <= beforeDuplicates {
		t.Fatalf("duplicates did not grow: before=%v after=%v", beforeDuplicates, got)
	}
	if got := eventLineCount(t, suite.dataDir); got != beforeEvents+1 {
		t.Fatalf("event lines = %d, want %d (one duplicate suppressed)", got, beforeEvents+1)
	}
}

func TestPauseDoesNotDispatch(t *testing.T) {
	if suite.live {
		t.Skip("pause is checked in direct mode")
	}
	pause := filepath.Join(suite.dir, "DISPATCH_OFF")
	if err := os.WriteFile(pause, []byte("test"), 0o600); err != nil {
		t.Fatalf("pause file: %v", err)
	}
	defer func() { _ = os.Remove(pause) }()
	beforeIgnored := suite.dispatchIgnored(t)
	payload := issueUpdatedPayload(suite.ytIssue, "FlowHub paused "+time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format("2006-01-02T15:04:05Z"), suite.ytProject)
	if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
		t.Fatalf("status = %d", status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if suite.dispatchIgnored(t) > beforeIgnored {
			if !suite.dispatchPaused(t) {
				t.Fatal("dispatch did not report paused")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("dispatch did not ignore a delivery while paused")
}

func TestShadowSoak(t *testing.T) {
	if suite.live {
		t.Skip("soak posts directly so the real webhook app does not duplicate it")
	}
	count := 10
	if raw := os.Getenv("E2E_SOAK"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			count = parsed
		}
	}
	delay := 50
	if raw := os.Getenv("E2E_SOAK_DELAY_MS"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			delay = parsed
		}
	}
	if err := suite.setMode("shadow", nil); err != nil {
		t.Fatalf("set shadow: %v", err)
	}
	t.Cleanup(func() { _ = suite.setMode("off", nil) })
	before := suite.flowHealth(t)
	beforeEvents := eventLineCount(t, suite.dataDir)
	for i := 0; i < count; i++ {
		description := fmt.Sprintf("FlowHub soak %d %s", i, time.Now().UTC().Format(time.RFC3339Nano))
		if err := suite.updateIssue(suite.ytIssue, description); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		payload := issueUpdatedPayload(suite.ytIssue, description, time.Now().UTC().Format("2006-01-02T15:04:05Z"), suite.ytProject)
		if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
			t.Fatalf("webhook %d status = %d", i, status)
		}
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}
	suite.waitForFlowHealth(t, func(f flowHealth) bool {
		return f.ShadowCompared >= before.ShadowCompared+float64(count)
	}, time.Duration(count)*2*time.Second+20*time.Second)
	health := suite.flowHealth(t)
	if health.ShadowMismatches != 0 {
		t.Fatalf("soak mismatches = %v", health.ShadowMismatches)
	}
	// Pending comparisons are possible when dispatch's own smaller queue drops a
	// burst; they stay visible rather than becoming a false match.
	if health.Pending != 0 {
		t.Logf("soak left %v comparisons pending", health.Pending)
	}
	if health.Runs < before.Runs+float64(count) {
		t.Fatalf("flow runs = %v, want at least %v", health.Runs, before.Runs+float64(count))
	}
	if got := eventLineCount(t, suite.dataDir); got != beforeEvents+count {
		t.Fatalf("event lines = %d, want %d after %d deliveries", got, beforeEvents+count, count)
	}
	if dropped := suite.flowDropped(t); dropped != 0 {
		t.Fatalf("flow queue dropped %v events", dropped)
	}
}

func TestBurstSoak(t *testing.T) {
	if os.Getenv("E2E_BURST") != "1" {
		t.Skip("set E2E_BURST=1 to run a burst")
	}
	if suite.live {
		t.Skip("burst posts directly; the live app would duplicate every event")
	}
	count := 200
	if raw := os.Getenv("E2E_BURST_COUNT"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			count = parsed
		}
	}
	concurrency := 16
	if raw := os.Getenv("E2E_BURST_CONCURRENCY"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			concurrency = parsed
		}
	}
	if err := suite.setMode("shadow", nil); err != nil {
		t.Fatalf("set shadow: %v", err)
	}
	t.Cleanup(func() { _ = suite.setMode("off", nil) })
	before := suite.flowHealth(t)
	beforeEvents := eventLineCount(t, suite.dataDir)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, concurrency)
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			description := fmt.Sprintf("FlowHub burst %d %s", i, time.Now().UTC().Format(time.RFC3339Nano))
			payload := issueUpdatedPayload(suite.ytIssue, description, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), suite.ytProject)
			status, err := suite.postWebhookRaw(payload)
			if err != nil || status != http.StatusAccepted {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("delivery %d: status=%d err=%v", i, status, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("burst delivery failed: %v", firstErr)
	}
	suite.waitForFlowHealth(t, func(f flowHealth) bool {
		return f.Runs >= before.Runs+float64(count)
	}, 2*time.Minute)
	health := suite.flowHealth(t)
	if health.Dropped > before.Dropped {
		t.Fatalf("burst dropped %v flow events", health.Dropped-before.Dropped)
	}
	if health.ShadowMismatches != 0 {
		t.Fatalf("burst mismatches = %v", health.ShadowMismatches)
	}
	// Dispatch still has its own smaller queue, so some shadow comparisons can
	// remain pending when a burst outruns it; the flow must not lose an event.
	t.Logf("burst: runs=%v compared=%v pending=%v dropped=%v", health.Runs, health.ShadowCompared, health.Pending, health.Dropped)
	if got := eventLineCount(t, suite.dataDir); got != beforeEvents+count {
		t.Fatalf("event lines = %d, want %d after %d deliveries", got, beforeEvents+count, count)
	}
}

func (e *env) postWebhookRaw(payload []byte) (int, error) {
	request, err := http.NewRequest(http.MethodPost, e.baseURL+"/hooks/youtrack/"+e.hookKey, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-YouTrack-Token", e.hookToken)
	response, err := e.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode, nil
}

func TestReplayCursor(t *testing.T) {
	if os.Getenv("E2E_REPLAY") != "1" {
		t.Skip("set E2E_REPLAY=1 to verify periodic catch-up and startup replay")
	}
	if suite.live {
		t.Skip("replay is checked in direct mode")
	}
	if err := suite.setMode("shadow", nil); err != nil {
		t.Fatalf("set shadow: %v", err)
	}
	t.Cleanup(func() { _ = suite.setMode("off", nil) })
	beforeRuns := suite.flowHealth(t).Runs

	// Establish a cursor with one normal delivery.
	payload := issueUpdatedPayload(suite.ytIssue, "FlowHub replay seed "+time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format("2006-01-02T15:04:05Z"), suite.ytProject)
	if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
		t.Fatalf("seed webhook status = %d", status)
	}
	suite.waitForFlowHealth(t, func(f flowHealth) bool { return f.Runs >= beforeRuns+1 }, 20*time.Second)
	time.Sleep(200 * time.Millisecond)

	// 1. Append events while the process is still running. Only the periodic
	// catch-up can see these; no HTTP delivery wakes the engine.
	const liveCount = 3
	appendReplayEvents(t, "live", liveCount)
	suite.waitForFlowHealth(t, func(f flowHealth) bool {
		return f.Runs >= beforeRuns+1+liveCount
	}, 30*time.Second)
	if got := suite.flowHealth(t).Runs; got != beforeRuns+1+liveCount {
		t.Fatalf("periodic catch-up produced %v runs, want %v", got, beforeRuns+1+liveCount)
	}

	// 2. Append events while the process is stopped. Only startup replay can see
	// these.
	suite.stop()
	const restartCount = 5
	appendReplayEvents(t, "restart", restartCount)
	suite.start()
	// The engine's runs counter is per process: after the restart it counts the
	// replayed events, not the events that belonged to the previous process.
	suite.waitForFlowHealth(t, func(f flowHealth) bool {
		return f.Runs >= restartCount
	}, 30*time.Second)
	if got := suite.flowHealth(t).Runs; got != restartCount {
		t.Fatalf("startup replay produced %v runs, want %d", got, restartCount)
	}
}

// appendReplayEvents writes valid events straight to the durable log, bypassing
// the webhook. This is what lets the test separate periodic catch-up (running)
// from startup replay (stopped).
func appendReplayEvents(t *testing.T, phase string, count int) {
	t.Helper()
	now := time.Now().UTC()
	path := filepath.Join(suite.dataDir, "events", now.Format("2006-01-02")+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	for i := 0; i < count; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		event := upstream.Event{
			ID: upstream.NewID(at), Schema: upstream.Schema, Upstream: "youtrack", Kind: upstream.KindIssueUpdated,
			OccurredAt: at, ReceivedAt: at,
			Subject:   upstream.Subject{Type: "issue", Project: suite.ytProject, Key: suite.ytIssue, Title: "replay " + phase},
			Actor:     upstream.Actor{Login: "yusiwen"},
			DedupeKey: fmt.Sprintf("replay-%s-%d-%d", phase, now.UnixNano(), i),
			Origin:    "upstream:youtrack",
		}
		data, err := json.Marshal(event)
		if err != nil {
			_ = file.Close()
			t.Fatalf("marshal replay event: %v", err)
		}
		if _, err := file.Write(append(data, '\n')); err != nil {
			_ = file.Close()
			t.Fatalf("append replay event: %v", err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close event log: %v", err)
	}
}
func attachmentCreatedPayload(key, summary, project, attachment string) []byte {
	payload := map[string]any{
		"event": "issueCreated", "timestamp": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"id": key, "summary": summary, "description": "Attachment E2E.",
		"project":  map[string]any{"key": project, "name": project, "shortName": project},
		"reporter": map[string]any{"login": "yusiwen", "fullName": "Siwen Yu"},
		"attachments": []map[string]any{{
			"name": attachment, "mimeType": "text/plain", "size": 42,
		}},
	}
	data, _ := json.Marshal(payload)
	return data
}

func otherEventPayload(kind, key, project string) []byte {
	payload := map[string]any{
		"event": kind, "timestamp": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"id": key, "summary": "Other event E2E",
		"project": map[string]any{"key": project, "name": project, "shortName": project},
	}
	data, _ := json.Marshal(payload)
	return data
}

func (e *env) removeTestWorktree(key string) {
	path := filepath.Join(e.dir, "worktrees", key)
	_ = exec.Command("git", "-C", e.repo, "worktree", "remove", "--force", path).Run()
	_ = exec.Command("git", "-C", e.repo, "branch", "-D", "flowhub/"+key).Run()
}

func TestAttachmentsAndOtherEventKinds(t *testing.T) {
	if os.Getenv("E2E_ATTACHMENTS") != "1" {
		t.Skip("set E2E_ATTACHMENTS=1 to run attachment and other-kind checks")
	}
	if suite.live {
		t.Skip("attachment checks use the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	fake.setMode("every")
	server := fake.start()
	t.Cleanup(server.Close)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}
	deliver := func(payload []byte) {
		t.Helper()
		if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
	}
	waitPrompts := func(want int) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if fake.promptCount() >= want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("prompts = %d, want %d", fake.promptCount(), want)
	}
	waitReply := func(prompt int, want string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if got := fake.permissionReplyFor(prompt); got == want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("permission reply %d = %q, want %q", prompt, fake.permissionReplyFor(prompt), want)
	}

	// Known-but-other events are accepted and recorded, but must not create work.
	beforeEvents := eventLineCount(t, suite.dataDir)
	beforePrompts := fake.promptCount()
	for _, tc := range []struct{ kind, key string }{
		{"issueAttachmentAdded", "TEST-OTHER-1"},
		{"issueAttachmentDeleted", "TEST-OTHER-2"},
		{"issueDeleted", "TEST-OTHER-3"},
		{"workItemAdded", "TEST-OTHER-4"},
	} {
		deliver(otherEventPayload(tc.kind, tc.key, suite.ytProject))
		if row, ok := suite.lastTaskRow(tc.key); ok {
			t.Fatalf("%s created a task row: %+v", tc.kind, row)
		}
	}
	if got := eventLineCount(t, suite.dataDir); got != beforeEvents+4 {
		t.Fatalf("other events appended %d lines, want 4", got-beforeEvents)
	}
	if got := fake.promptCount(); got != beforePrompts {
		t.Fatalf("other events caused %d prompt(s)", got-beforePrompts)
	}

	// A signed attachment URL on the configured host is an allowed download.
	t.Cleanup(func() {
		suite.removeTestWorktree("TEST-ATTACH-1")
		suite.removeTestWorktree("TEST-ATTACH-2")
	})
	fake.setPermissionCommand(`curl -sSL -o .flowhub/attachments/log.txt "https://pm.yusiwen.cn/api/files/1"`)
	deliver(attachmentCreatedPayload("TEST-ATTACH-1", "Attachment allowed", suite.ytProject, "log.txt"))
	waitPrompts(1)
	waitReply(1, "once")
	suite.waitForRegistry(t, "TEST-ATTACH-1", func(row taskRow) bool { return row.Worktree != "" }, 20*time.Second)
	if prompts := fake.allPrompts(); len(prompts) == 0 || !strings.Contains(prompts[0], ".flowhub/attachments") {
		t.Fatalf("attachment prompt does not name the download directory: %q", prompts)
	}

	// The same shape for a host outside the download policy must be refused.
	fake.setPermissionCommand(`curl -sSL -o .flowhub/attachments/log.txt "https://evil.example/log.txt"`)
	deliver(attachmentCreatedPayload("TEST-ATTACH-2", "Attachment denied", suite.ytProject, "log.txt"))
	waitPrompts(2)
	waitReply(2, "reject")
}

func (e *env) postWebhookRequest(t *testing.T, path, token, contentType string, body []byte) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, e.baseURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("X-YouTrack-Token", token)
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	reply, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	return response.StatusCode, strings.TrimSpace(string(reply))
}

func issueUpdatedPayloadAt(key, description, timestamp, project string) []byte {
	payload := map[string]any{
		"event":       "issueUpdated",
		"timestamp":   timestamp,
		"id":          key,
		"summary":     "FlowHub E2E ingress",
		"project":     map[string]any{"key": project, "name": project, "shortName": project},
		"description": description,
		"updatedBy":   map[string]any{"login": "yusiwen", "fullName": "Siwen Yu"},
		"changedFields": []map[string]any{{
			"name": "description", "value": description,
		}},
	}
	data, _ := json.Marshal(payload)
	return data
}

func TestIngressRejectionsAndAppendFailure(t *testing.T) {
	if os.Getenv("E2E_INGRESS") != "1" {
		t.Skip("set E2E_INGRESS=1 to run ingress rejection and append-failure checks")
	}
	if suite.live {
		t.Skip("ingress failures use the direct receiver")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	server := fake.start()
	t.Cleanup(server.Close)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })

	count := func() int { return eventLineCount(t, suite.dataDir) }
	deliver := func(payload []byte) int {
		t.Helper()
		status, _ := suite.postWebhookRequest(t, "/hooks/youtrack/"+suite.hookKey, suite.hookToken, "application/json", payload)
		return status
	}
	baseline := count()
	validPayload := issueUpdatedPayload(suite.ytIssue, "FlowHub ingress baseline "+time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format("2006-01-02T15:04:05Z"), suite.ytProject)
	if status := deliver(validPayload); status != http.StatusAccepted {
		t.Fatalf("baseline status = %d", status)
	}
	if got := count(); got != baseline+1 {
		t.Fatalf("baseline event count = %d, want %d", got, baseline+1)
	}

	cases := []struct {
		name        string
		path        string
		token       string
		contentType string
		body        []byte
		want        int
	}{
		{"wrong URL key", "/hooks/youtrack/not-the-key", suite.hookToken, "application/json", validPayload, http.StatusAccepted},
		{"wrong token", "/hooks/youtrack/" + suite.hookKey, "wrong-token", "application/json", validPayload, http.StatusAccepted},
		{"bad content type", "/hooks/youtrack/" + suite.hookKey, suite.hookToken, "text/plain", validPayload, http.StatusAccepted},
		{"malformed JSON", "/hooks/youtrack/" + suite.hookKey, suite.hookToken, "application/json", []byte("{"), http.StatusAccepted},
		{"unknown event", "/hooks/youtrack/" + suite.hookKey, suite.hookToken, "application/json", []byte(`{"event":"notARealEvent"}`), http.StatusAccepted},
		{"replay window", "/hooks/youtrack/" + suite.hookKey, suite.hookToken, "application/json", issueUpdatedPayloadAt(suite.ytIssue, "old", "2020-01-01T00:00:00Z", suite.ytProject), http.StatusAccepted},
		{"oversized body", "/hooks/youtrack/" + suite.hookKey, suite.hookToken, "application/json", bytes.Repeat([]byte("x"), 300*1024), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		before := count()
		status, _ := suite.postWebhookRequest(t, tc.path, tc.token, tc.contentType, tc.body)
		if status != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, status, tc.want)
		}
		if got := count(); got != before {
			t.Errorf("%s: event count changed %d -> %d", tc.name, before, got)
		}
	}

	// Make today's event file unwritable after stopping: the next process opens it
	// for append and gets EACCES. (A read-only directory alone is not enough: the
	// file already exists and its own mode permits the owner to open it.)
	suite.stop()
	dayPath := filepath.Join(suite.dataDir, "events", time.Now().UTC().Format("2006-01-02")+".jsonl")
	if err := os.Chmod(dayPath, 0o400); err != nil {
		t.Fatalf("chmod event file: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dayPath, 0o600) })
	suite.start()
	failedPayload := issueUpdatedPayload(suite.ytIssue, "FlowHub ingress append failure "+time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format("2006-01-02T15:04:05Z"), suite.ytProject)
	before := count()
	if status := deliver(failedPayload); status != http.StatusServiceUnavailable {
		t.Fatalf("append failure status = %d, want 503", status)
	}
	if got := count(); got != before {
		t.Fatalf("failed append changed event count %d -> %d", before, got)
	}
	if err := os.Chmod(dayPath, 0o600); err != nil {
		t.Fatalf("restore event file: %v", err)
	}
	if status := deliver(failedPayload); status != http.StatusAccepted {
		t.Fatalf("retry after append failure status = %d, want 202 (dedupe key was released)", status)
	}
	if got := count(); got != before+1 {
		t.Fatalf("retry event count = %d, want %d", got, before+1)
	}

	// A source outside the allowlist is rejected before the event append.
	suite.stop()
	suite.extraEnv = []string{"FLOWHUB_ALLOWED_SOURCES=192.0.2.1"}
	suite.start()
	before = count()
	if status := deliver(validPayload); status != http.StatusAccepted {
		t.Fatalf("denied-source status = %d", status)
	}
	if got := count(); got != before {
		t.Fatalf("denied source appended an event: %d -> %d", before, got)
	}
}

func TestSilentTurnNudgeAndFailure(t *testing.T) {
	if os.Getenv("E2E_SILENT") != "1" {
		t.Skip("set E2E_SILENT=1 to run the silent-turn nudge/failure path")
	}
	if suite.live {
		t.Skip("silent turns run against the fake runtime in direct mode")
	}
	suite.stop()
	fake := newFakeOpencode(t)
	fake.setMode("nudge")
	server := fake.start()
	t.Cleanup(server.Close)
	suite.ocBase = server.URL
	if err := writeTestConfig(suite.configPath, suite.repo, filepath.Join(suite.dir, "worktrees"), suite.ytProject, "", suite.ocBase); err != nil {
		t.Fatalf("rewrite config for fake runtime: %v", err)
	}
	suite.start()
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}

	waitPrompts := func(want int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if fake.promptCount() >= want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("fake runtime saw %d prompts, want %d", fake.promptCount(), want)
	}
	createAndDeliver := func(summary string) string {
		t.Helper()
		key, _, err := suite.createIssue(summary, "Silent-turn E2E.")
		if err != nil {
			t.Fatalf("create issue: %v", err)
		}
		if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Silent-turn E2E.", suite.ytProject)); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
		return key
	}

	// First turn silent, nudge posts the comment: the task is answered.
	nudgeKey := createAndDeliver("FlowHub E2E silent nudge " + time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	waitPrompts(2)
	row := suite.waitForRegistry(t, nudgeKey, func(row taskRow) bool {
		return row.State == "awaiting_input"
	}, 2*time.Minute)
	if row.Plan != "none" {
		t.Fatalf("nudged analysis plan = %q, want none", row.Plan)
	}

	// Both turns silent: the task must be failed, never awaiting_input.
	fake.setMode("silent")
	failKey := createAndDeliver("FlowHub E2E silent failure " + time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	waitPrompts(4)
	failed := suite.waitForRegistry(t, failKey, func(row taskRow) bool {
		return row.State == "failed"
	}, 2*time.Minute)
	if !strings.Contains(failed.LastError, "without posting a comment") {
		t.Fatalf("failure reason = %q, want the no-comment reason", failed.LastError)
	}
	if failed.State == "awaiting_input" {
		t.Fatal("a silent task was recorded awaiting_input")
	}
}

func (e *env) pointAtRuntime(t *testing.T, baseURL string) {
	t.Helper()
	e.ocBase = baseURL
	if err := writeTestConfig(e.configPath, e.repo, filepath.Join(e.dir, "worktrees"), e.ytProject, "", e.ocBase); err != nil {
		t.Fatalf("rewrite config for runtime %s: %v", baseURL, err)
	}
}

func (e *env) setProjectAuthors(t *testing.T, authors []string) {
	t.Helper()
	data, err := os.ReadFile(e.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	projects, _ := raw["projects"].([]any)
	found := false
	for _, item := range projects {
		entry, ok := item.(map[string]any)
		if ok && entry["project"] == e.ytProject {
			entry["authors"] = authors
			found = true
		}
	}
	if !found {
		t.Fatalf("project %q not in config", e.ytProject)
	}
	data, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(e.configPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func (e *env) setProjectRepo(t *testing.T, repo, worktrees string) {
	t.Helper()
	if err := os.MkdirAll(worktrees, 0o700); err != nil {
		t.Fatalf("worktrees for %s: %v", repo, err)
	}
	data, err := os.ReadFile(e.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	projects, _ := raw["projects"].([]any)
	found := false
	for _, item := range projects {
		entry, ok := item.(map[string]any)
		if !ok || entry["project"] != e.ytProject {
			continue
		}
		repoEntry, _ := entry["repo"].(map[string]any)
		repoEntry["path"] = repo
		entry["worktrees"] = worktrees
		found = true
	}
	if !found {
		t.Fatalf("project %q not in config", e.ytProject)
	}
	data, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(e.configPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func (e *env) setRuntimeMaxConcurrent(t *testing.T, max int) {
	t.Helper()
	data, err := os.ReadFile(e.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	runtimes, _ := raw["runtimes"].(map[string]any)
	for _, value := range runtimes {
		if runtime, ok := value.(map[string]any); ok {
			runtime["max_concurrent"] = max
		}
	}
	data, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(e.configPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestRepositoryRebindRefused(t *testing.T) {
	if os.Getenv("E2E_REBIND") != "1" {
		t.Skip("set E2E_REBIND=1 to bind a task to one repo and then move the mapping")
	}
	if suite.live {
		t.Skip("repository rebind uses the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	fake.setMode("every")
	server := fake.start()
	t.Cleanup(server.Close)
	t.Cleanup(fake.release)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })

	key := "TEST-REBIND-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { suite.removeTestWorktree(key) })
	summary := "FlowHub E2E rebind " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	deliver := func() {
		t.Helper()
		if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Rebind E2E.", suite.ytProject)); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
	}
	deliver()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && fake.promptCount() < 1 {
		time.Sleep(100 * time.Millisecond)
	}
	if fake.promptCount() != 1 {
		t.Fatalf("prompts = %d, want 1", fake.promptCount())
	}
	bound := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Turns == 1 && row.Worktree != "" && row.Session != ""
	}, 30*time.Second)
	if bound.Worktree == "" {
		t.Fatal("first turn did not bind a worktree")
	}

	rebindRepo := filepath.Join(suite.dir, "rebind-repo")
	cmd := exec.Command("git", "clone", "--quiet", suite.repo, rebindRepo)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone second repo: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rebindRepo) })
	rebindWorktrees := filepath.Join(suite.dir, "rebind-worktrees")
	suite.stop()
	suite.setProjectRepo(t, rebindRepo, rebindWorktrees)
	suite.start()

	deliver()
	failed := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.State == "failed" && strings.Contains(row.LastError, "bound to")
	}, 20*time.Second)
	if !strings.Contains(failed.LastError, rebindRepo) {
		t.Fatalf("failure does not name the new repo: %q", failed.LastError)
	}
	time.Sleep(time.Second)
	if got := fake.promptCount(); got != 1 {
		t.Fatalf("prompts after rebind = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(rebindWorktrees, key)); !os.IsNotExist(err) {
		t.Fatalf("a worktree was created under the new repo: %v", err)
	}
}

func TestCrossTaskCapacitySaturation(t *testing.T) {
	if os.Getenv("E2E_CAPACITY") != "1" {
		t.Skip("set E2E_CAPACITY=1 to saturate a runtime with two distinct tasks")
	}
	if suite.live {
		t.Skip("capacity saturation uses the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	fake.setMode("every")
	hold := make(chan struct{})
	fake.setHold(hold)
	server := fake.start()
	t.Cleanup(server.Close)
	t.Cleanup(fake.release)
	suite.pointAtRuntime(t, server.URL)
	suite.setRuntimeMaxConcurrent(t, 1)
	suite.start()
	t.Cleanup(func() { suite.stop() })

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	keys := []string{"TEST-CAP-A-" + suffix, "TEST-CAP-B-" + suffix}
	for _, key := range keys {
		key := key
		t.Cleanup(func() { suite.removeTestWorktree(key) })
		summary := "FlowHub E2E capacity " + key
		if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Capacity E2E.", suite.ytProject)); status != http.StatusAccepted {
			t.Fatalf("webhook for %s = %d", key, status)
		}
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && fake.promptCount() < 1 {
		time.Sleep(100 * time.Millisecond)
	}
	if fake.promptCount() != 1 {
		t.Fatalf("prompts before release = %d, want 1", fake.promptCount())
	}
	time.Sleep(500 * time.Millisecond)
	if fake.promptCount() != 1 {
		t.Fatalf("a second task started while the runtime had one slot: prompts=%d", fake.promptCount())
	}
	if got := fake.maxConcurrentPrompts(); got != 1 {
		t.Fatalf("max concurrent prompts = %d, want 1", got)
	}

	fake.release()
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && fake.promptCount() < 2 {
		time.Sleep(100 * time.Millisecond)
	}
	if fake.promptCount() != 2 {
		t.Fatalf("prompts after release = %d, want 2", fake.promptCount())
	}
	for _, key := range keys {
		row := suite.waitForRegistry(t, key, func(row taskRow) bool {
			return row.Turns == 1 && row.State == "awaiting_input"
		}, 30*time.Second)
		if row.Session == "" || row.Worktree == "" {
			t.Fatalf("task %s has no worktree/session: %+v", key, row)
		}
	}
	if got := fake.maxConcurrentPrompts(); got != 1 {
		t.Fatalf("max concurrent prompts after release = %d, want 1", got)
	}
}

func TestCrashDuringExecute(t *testing.T) {
	if os.Getenv("E2E_CRASH_EXECUTE") != "1" {
		t.Skip("set E2E_CRASH_EXECUTE=1 to SIGKILL during execution")
	}
	if suite.live {
		t.Skip("crash-during-execute uses the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	fake.setMode("every")
	hold := make(chan struct{})
	fake.setHold(hold)
	fake.setHoldAt(3)
	server := fake.start()
	t.Cleanup(server.Close)
	t.Cleanup(fake.release)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}

	waitPrompts := func(want int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if fake.promptCount() >= want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("fake runtime saw %d prompts, want %d", fake.promptCount(), want)
	}
	summary := "FlowHub E2E crash execute " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, "Crash-during-execute E2E.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	deliver := func(payload []byte) {
		t.Helper()
		if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
	}

	// 1. Analysis.
	deliver(issueCreatedPayload(key, summary, "Crash-during-execute E2E.", suite.ytProject))
	waitPrompts(1)
	suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Turns == 1 && row.Plan == "none" && row.Session != ""
	}, 30*time.Second)

	// 2. Plan.
	comment, err := suite.addComment(key, "/opencode start")
	if err != nil {
		t.Fatalf("plan comment: %v", err)
	}
	deliver(commentAddedPayload(key, comment, "/opencode start", suite.ytProject))
	waitPrompts(2)
	suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Turns == 2 && row.Plan == "draft"
	}, 30*time.Second)

	// 3. Execute, held inside the prompt, then SIGKILL the receiver.
	// Comment identity is issue + second + actor + text: wait one second so the
	// execute comment is a distinct event, not a duplicate of the plan comment.
	time.Sleep(1100 * time.Millisecond)
	comment, err = suite.addComment(key, "/opencode start")
	if err != nil {
		t.Fatalf("execute comment: %v", err)
	}
	deliver(commentAddedPayload(key, comment, "/opencode start", suite.ytProject))
	waitPrompts(3)
	before := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.State == "executing"
	}, 30*time.Second)
	suite.kill()
	fake.release()
	time.Sleep(300 * time.Millisecond)
	suite.start()

	row := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.State == "failed" && strings.Contains(row.LastError, "interrupted by a restart")
	}, 20*time.Second)
	if row.Worktree != before.Worktree || row.Worktree == "" {
		t.Fatalf("worktree changed across crash: before=%q after=%q", before.Worktree, row.Worktree)
	}
	if row.Session != before.Session || row.Session == "" {
		t.Fatalf("session changed across crash: before=%q after=%q", before.Session, row.Session)
	}
	if row.Turns != before.Turns {
		t.Fatalf("turns changed across crash: before=%d after=%d", before.Turns, row.Turns)
	}
	time.Sleep(500 * time.Millisecond)
	if got := fake.promptCount(); got != 3 {
		t.Fatalf("prompts after restart = %d, want 3 (no replayed turn)", got)
	}
	if fake.sessionCount() != 1 {
		t.Fatalf("sessions after restart = %d, want one", fake.sessionCount())
	}
}

type runRow struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Mode   string `json:"mode"`
	Task   string `json:"task"`
	Action string `json:"action"`
}

func (e *env) runRows(t *testing.T) []runRow {
	t.Helper()
	var body struct {
		Runs []runRow `json:"runs"`
	}
	request, err := http.NewRequest(http.MethodGet, e.baseURL+"/hub/v1/runs?limit=100", nil)
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
	return body.Runs
}

func (e *env) waitForRunState(t *testing.T, task, state string, timeout time.Duration) runRow {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, run := range e.runRows(t) {
			if run.Task == task && run.State == state {
				return run
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no %s run for task %s within %s", state, task, timeout)
	return runRow{}
}

func TestCrashDuringOnRun(t *testing.T) {
	if os.Getenv("E2E_CRASH_ON") != "1" {
		t.Skip("set E2E_CRASH_ON=1 to SIGKILL during an on run")
	}
	if suite.live {
		t.Skip("crash-during-on uses the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	fake.setMode("every")
	hold := make(chan struct{})
	fake.setHold(hold)
	fake.setHoldAt(3)
	server := fake.start()
	t.Cleanup(server.Close)
	t.Cleanup(fake.release)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("on", []string{suite.ytProject}); err != nil {
		t.Fatalf("set on: %v", err)
	}

	waitPrompts := func(want int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if fake.promptCount() >= want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("fake runtime saw %d prompts, want %d", fake.promptCount(), want)
	}
	summary := "FlowHub E2E crash on " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, "Crash-during-on E2E.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	deliver := func(payload []byte) {
		t.Helper()
		if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
	}

	deliver(issueCreatedPayload(key, summary, "Crash-during-on E2E.", suite.ytProject))
	waitPrompts(1)
	suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Turns == 1 && row.Plan == "none" && row.Session != ""
	}, 30*time.Second)

	comment, err := suite.addComment(key, "/opencode start")
	if err != nil {
		t.Fatalf("plan comment: %v", err)
	}
	deliver(commentAddedPayload(key, comment, "/opencode start", suite.ytProject))
	waitPrompts(2)
	suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Turns == 2 && row.Plan == "draft"
	}, 30*time.Second)

	// Comment identity is issue + second + actor + text: wait one second so the
	// execute comment is a distinct event, not a duplicate of the plan comment.
	time.Sleep(1100 * time.Millisecond)
	comment, err = suite.addComment(key, "/opencode start")
	if err != nil {
		t.Fatalf("execute comment: %v", err)
	}
	deliver(commentAddedPayload(key, comment, "/opencode start", suite.ytProject))
	waitPrompts(3)
	before := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.State == "executing"
	}, 30*time.Second)
	suite.kill()
	fake.release()
	time.Sleep(300 * time.Millisecond)
	suite.start()

	row := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.State == "failed" && strings.Contains(row.LastError, "interrupted by a restart")
	}, 20*time.Second)
	if row.Worktree != before.Worktree || row.Worktree == "" {
		t.Fatalf("worktree changed across on crash: before=%q after=%q", before.Worktree, row.Worktree)
	}
	if row.Session != before.Session || row.Session == "" {
		t.Fatalf("session changed across on crash: before=%q after=%q", before.Session, row.Session)
	}
	if row.Turns != before.Turns {
		t.Fatalf("turns changed across on crash: before=%d after=%d", before.Turns, row.Turns)
	}
	run := suite.waitForRunState(t, key, "needs_attention", 20*time.Second)
	if run.Mode != "on" {
		t.Fatalf("interrupted run mode = %q, want on", run.Mode)
	}
	time.Sleep(500 * time.Millisecond)
	if got := fake.promptCount(); got != 3 {
		t.Fatalf("prompts after on restart = %d, want 3 (no replay)", got)
	}

	// Recovery must leave the engine usable for a new event.
	follow := "FlowHub E2E on after crash " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	followKey, _, err := suite.createIssue(follow, "On-after-crash E2E.")
	if err != nil {
		t.Fatalf("create follow-up issue: %v", err)
	}
	deliver(issueCreatedPayload(followKey, follow, "On-after-crash E2E.", suite.ytProject))
	waitPrompts(4)
	suite.waitForRegistry(t, followKey, func(row taskRow) bool {
		return row.Session != "" && row.Turns == 1
	}, 30*time.Second)
	suite.waitForRunState(t, followKey, "succeeded", 20*time.Second)
	if fake.sessionCount() != 2 {
		t.Fatalf("sessions = %d, want one per task", fake.sessionCount())
	}
}

func TestSameTaskSecondDeliveryWaits(t *testing.T) {
	if os.Getenv("E2E_SERIALIZATION") != "1" {
		t.Skip("set E2E_SERIALIZATION=1 to run the same-task serialization path")
	}
	if suite.live {
		t.Skip("serialization uses the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	fake.setMode("every")
	hold := make(chan struct{})
	fake.setHold(hold)
	server := fake.start()
	t.Cleanup(server.Close)
	t.Cleanup(fake.release)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}

	summary := "FlowHub E2E serialization " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, "Serialization E2E.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Serialization E2E.", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("issue webhook status = %d", status)
	}
	suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Worktree != ""
	}, 20*time.Second)
	deadline := time.Now().Add(20 * time.Second)
	for fake.promptCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if fake.promptCount() != 1 {
		t.Fatalf("first turn prompts = %d, want 1 (held)", fake.promptCount())
	}

	// A second delivery for the same task must wait for the first turn, not start
	// a second session or an overlapping prompt.
	comment, err := suite.addComment(key, "/opencode start")
	if err != nil {
		t.Fatalf("plan comment: %v", err)
	}
	if status := suite.postWebhook(t, commentAddedPayload(key, comment, "/opencode start", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("comment webhook status = %d", status)
	}
	time.Sleep(500 * time.Millisecond)
	if fake.promptCount() != 1 {
		t.Fatalf("second delivery started a prompt while the first turn was held: %d", fake.promptCount())
	}
	if got := fake.maxConcurrentPrompts(); got != 1 {
		t.Fatalf("concurrent prompts = %d, want 1", got)
	}

	fake.release()
	row := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Turns == 2 && row.Plan == "draft" && row.Session != ""
	}, 30*time.Second)
	if got := fake.promptCount(); got != 2 {
		t.Fatalf("prompts after both turns = %d, want 2", got)
	}
	if fake.sessionCount() != 1 {
		t.Fatalf("sessions = %d, want one task session", fake.sessionCount())
	}
	if row.State != "awaiting_input" {
		t.Fatalf("state = %q, want awaiting_input after the plan reply", row.State)
	}
}

func TestAuthorAllowlistAndUnmappedProject(t *testing.T) {
	if os.Getenv("E2E_POLICY") != "1" {
		t.Skip("set E2E_POLICY=1 to run author/unmapped-project policy checks")
	}
	if suite.live {
		t.Skip("policy checks use the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	server := fake.start()
	t.Cleanup(server.Close)
	suite.pointAtRuntime(t, server.URL)
	suite.setProjectAuthors(t, []string{"allowed-user"})
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}

	// A reporter outside the project allowlist must not create a task or a session.
	denied := "FlowHub E2E author denied " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	deniedKey, _, err := suite.createIssue(denied, "Author-deny E2E.")
	if err != nil {
		t.Fatalf("create denied issue: %v", err)
	}
	if status := suite.postWebhook(t, issueCreatedPayload(deniedKey, denied, "Author-deny E2E.", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("denied webhook status = %d", status)
	}
	time.Sleep(700 * time.Millisecond)
	if row, ok := suite.lastTaskRow(deniedKey); ok {
		t.Fatalf("denied author created a task row: %+v", row)
	}
	if got := fake.promptCount(); got != 0 {
		t.Fatalf("denied author caused %d prompt(s)", got)
	}

	// The same project with an allowlisted reporter must still work.
	allowed := "FlowHub E2E author allowed " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	allowedKey, _, err := suite.createIssue(allowed, "Author-allowed E2E.")
	if err != nil {
		t.Fatalf("create allowed issue: %v", err)
	}
	if status := suite.postWebhook(t, issueCreatedPayloadBy(allowedKey, allowed, "Author-allowed E2E.", suite.ytProject, "allowed-user")); status != http.StatusAccepted {
		t.Fatalf("allowed webhook status = %d", status)
	}
	suite.waitForRegistry(t, allowedKey, func(row taskRow) bool {
		return row.Worktree != ""
	}, 20*time.Second)
	if got := fake.promptCount(); got == 0 {
		t.Fatal("allowlisted reporter did not reach the runtime")
	}

	// An unmapped project must be refused before any task is created.
	beforePrompts := fake.promptCount()
	unmapped := "FlowHub E2E unmapped " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if status := suite.postWebhook(t, issueCreatedPayload("OTHER-1", unmapped, "Unmapped E2E.", "OTHER")); status != http.StatusAccepted {
		t.Fatalf("unmapped webhook status = %d", status)
	}
	time.Sleep(700 * time.Millisecond)
	if row, ok := suite.lastTaskRow("OTHER-1"); ok {
		t.Fatalf("unmapped project created a task row: %+v", row)
	}
	if got := fake.promptCount(); got != beforePrompts {
		t.Fatalf("unmapped project caused %d prompt(s)", got-beforePrompts)
	}
}

func (e *env) setSourceMaxTurns(t *testing.T, maxTurns int) {
	t.Helper()
	data, err := os.ReadFile(e.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	sources, _ := raw["sources"].(map[string]any)
	youtrack, _ := sources["youtrack"].(map[string]any)
	policy, _ := youtrack["policy"].(map[string]any)
	if policy == nil {
		policy = map[string]any{}
		youtrack["policy"] = policy
	}
	policy["max_turns"] = maxTurns
	data, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(e.configPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func runLimitPhase(t *testing.T, envValue string, maxTurns int) taskRow {
	t.Helper()
	suite.stop()
	suite.extraEnv = []string{envValue}
	fake := newFakeOpencode(t)
	fake.setMode("every")
	server := fake.start()
	t.Cleanup(server.Close)
	suite.pointAtRuntime(t, server.URL)
	suite.setSourceMaxTurns(t, maxTurns)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}

	summary := "FlowHub E2E limit " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, "Limit E2E.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	deliver := func(payload []byte) {
		t.Helper()
		if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
	}
	deliver(issueCreatedPayload(key, summary, "Limit E2E.", suite.ytProject))
	suite.waitForRegistry(t, key, func(row taskRow) bool { return row.Turns == 1 }, 20*time.Second)
	comment, err := suite.addComment(key, "/opencode start")
	if err != nil {
		t.Fatalf("plan comment: %v", err)
	}
	deliver(commentAddedPayload(key, comment, "/opencode start", suite.ytProject))
	row := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Turns == 2 && row.Plan == "draft"
	}, 20*time.Second)

	// A third trigger must be ignored by the limit before any prompt.
	time.Sleep(1100 * time.Millisecond)
	comment, err = suite.addComment(key, "/opencode start")
	if err != nil {
		t.Fatalf("execute comment: %v", err)
	}
	deliver(commentAddedPayload(key, comment, "/opencode start", suite.ytProject))
	time.Sleep(500 * time.Millisecond)
	if got := fake.promptCount(); got != 2 {
		t.Fatalf("prompts after the limit = %d, want 2", got)
	}
	after := suite.waitForRegistry(t, key, func(row taskRow) bool { return row.Turns == 2 }, 5*time.Second)
	if after.State == "executing" {
		t.Fatalf("limit ignored delivery still entered execution: %+v", after)
	}
	return row
}

func TestCostCapStopsATask(t *testing.T) {
	if os.Getenv("E2E_LIMITS") != "1" {
		t.Skip("set E2E_LIMITS=1 to run cost/turn limit checks")
	}
	if suite.live {
		t.Skip("limit checks use the fake runtime in direct mode")
	}
	row := runLimitPhase(t, "FLOWHUB_TASK_MAX_COST=0.0015", 8)
	if row.Cost < 0.0015 {
		t.Fatalf("task cost = %v, want the two turns above the cap", row.Cost)
	}
}

func TestMaxTurnsStopsATask(t *testing.T) {
	if os.Getenv("E2E_LIMITS") != "1" {
		t.Skip("set E2E_LIMITS=1 to run cost/turn limit checks")
	}
	if suite.live {
		t.Skip("limit checks use the fake runtime in direct mode")
	}
	runLimitPhase(t, "FLOWHUB_MAX_TURNS=2", 2)
}

func (e *env) addProject(t *testing.T, project string) {
	t.Helper()
	data, err := os.ReadFile(e.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	worktrees := filepath.Join(e.dir, "worktrees-"+project)
	if err := os.MkdirAll(worktrees, 0o700); err != nil {
		t.Fatalf("worktrees: %v", err)
	}
	projects, _ := raw["projects"].([]any)
	projects = append(projects, map[string]any{
		"project": project, "source": "youtrack",
		"repo":      map[string]any{"path": e.repo, "default_branch": "main"},
		"worktrees": worktrees, "runtime": "local",
	})
	raw["projects"] = projects
	data, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(e.configPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func (e *env) apiRequest(t *testing.T, method, path, token string, body []byte) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, e.baseURL+path, reader)
	if err != nil {
		t.Fatalf("api request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := e.client.Do(request)
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	reply, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, strings.TrimSpace(string(reply))
}

func TestAPIAuthAndInvalidDeployment(t *testing.T) {
	if os.Getenv("E2E_API") != "1" {
		t.Skip("set E2E_API=1 to run /hub/v1 auth/deployment checks")
	}
	if suite.live {
		t.Skip("API checks use the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	server := fake.start()
	t.Cleanup(server.Close)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	base := "/hub/v1/flows/default-youtrack"

	if status, _ := suite.apiRequest(t, http.MethodGet, "/hub/v1/ping", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", status)
	}
	if status, _ := suite.apiRequest(t, http.MethodGet, "/hub/v1/ping", "wrong", nil); status != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", status)
	}
	if status, _ := suite.apiRequest(t, http.MethodGet, "/hub/v1/flows", suite.hubToken, nil); status != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200", status)
	}

	cases := []struct {
		name string
		path string
		body string
		want int
	}{
		{"bogus mode", base + "/deployment", `{"mode":"bogus"}`, http.StatusBadRequest},
		{"bad project", base + "/deployment", `{"mode":"shadow","projects":["bad key"]}`, http.StatusBadRequest},
		{"unknown field", base + "/deployment", `{"mode":"shadow","extra":1}`, http.StatusBadRequest},
		{"unknown flow", "/hub/v1/flows/unknown/deployment", `{"mode":"shadow"}`, http.StatusNotFound},
		{"no auth deployment", base + "/deployment", `{"mode":"shadow"}`, http.StatusUnauthorized},
		{"on with task service", base + "/deployment", `{"mode":"on","projects":["TEST"]}`, http.StatusOK},
	}
	for _, tc := range cases {
		token := suite.hubToken
		if tc.name == "no auth deployment" {
			token = ""
		}
		status, body := suite.apiRequest(t, http.MethodPost, tc.path, token, []byte(tc.body))
		if status != tc.want {
			t.Fatalf("%s: status = %d, want %d (%s)", tc.name, status, tc.want, body)
		}
	}
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("restore off: %v", err)
	}

	// Disabled API: no FLOWHUB_HUB_TOKEN means every endpoint is 503.
	originalToken := suite.hubToken
	suite.hubToken = ""
	suite.stop()
	suite.start()
	if status, _ := suite.apiRequest(t, http.MethodGet, "/hub/v1/flows", "", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("disabled API status = %d, want 503", status)
	}
	suite.stop()
	suite.hubToken = originalToken

	// on cannot be stored when the process has no task service.
	suite.extraEnv = []string{"FLOWHUB_DISPATCH=0"}
	suite.start()
	if status, body := suite.apiRequest(t, http.MethodPost, base+"/deployment", suite.hubToken, []byte(`{"mode":"on"}`)); status != http.StatusConflict {
		t.Fatalf("on without task service status = %d, want 409 (%s)", status, body)
	}
}

func TestActiveOnGracefulRestart(t *testing.T) {
	if os.Getenv("E2E_ON_RESTART") != "1" {
		t.Skip("set E2E_ON_RESTART=1 to restart during an active on run")
	}
	if suite.live {
		t.Skip("restart hardening uses the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	fake.setMode("every")
	hold := make(chan struct{})
	fake.setHold(hold)
	fake.setHoldAt(3)
	server := fake.start()
	t.Cleanup(server.Close)
	t.Cleanup(fake.release)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("on", []string{suite.ytProject}); err != nil {
		t.Fatalf("set on: %v", err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	key := "TEST-ON-RESTART-" + suffix
	t.Cleanup(func() { suite.removeTestWorktree(key) })
	summary := "FlowHub E2E on restart " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	deliver := func(payload []byte) {
		t.Helper()
		if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
	}
	waitPrompts := func(want int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if fake.promptCount() >= want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("prompts = %d, want %d", fake.promptCount(), want)
	}
	deliver(issueCreatedPayload(key, summary, "On restart E2E.", suite.ytProject))
	waitPrompts(1)
	suite.waitForRegistry(t, key, func(row taskRow) bool { return row.Turns == 1 }, 30*time.Second)
	comment := "plan-" + suffix
	deliver(commentAddedPayload(key, comment, "/opencode start", suite.ytProject))
	waitPrompts(2)
	suite.waitForRegistry(t, key, func(row taskRow) bool { return row.Turns == 2 && row.Plan == "draft" }, 30*time.Second)
	time.Sleep(1100 * time.Millisecond)
	comment = "execute-" + suffix
	deliver(commentAddedPayload(key, comment, "/opencode start", suite.ytProject))
	waitPrompts(3)
	before := suite.waitForRegistry(t, key, func(row taskRow) bool { return row.State == "executing" }, 30*time.Second)
	// Graceful restart while the execute prompt is held: SIGINT lets the engine
	// cancel the turn and unwind, unlike the SIGKILL test.
	suite.stop()
	fake.release()
	time.Sleep(300 * time.Millisecond)
	suite.start()
	row := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.State == "failed"
	}, 20*time.Second)
	if !strings.Contains(strings.ToLower(row.LastError), "interrupt") {
		t.Fatalf("graceful shutdown failure reason = %q", row.LastError)
	}
	if row.Worktree != before.Worktree || row.Session != before.Session {
		t.Fatalf("worktree/session changed: before=%+v after=%+v", before, row)
	}
	// The execute turn was attempted and then interrupted, so it is counted once;
	// it must not be replayed as a fourth turn.
	if row.Turns != before.Turns+1 {
		t.Fatalf("turns = %d, want one interrupted attempt after %d", row.Turns, before.Turns)
	}
	time.Sleep(500 * time.Millisecond)
	if got := fake.promptCount(); got != 3 {
		t.Fatalf("prompts after graceful restart = %d, want 3", got)
	}
	for _, run := range suite.runRows(t) {
		if run.Task == key && run.State == "running" {
			t.Fatalf("run still running after restart: %+v", run)
		}
	}
}

func TestFlowSameTaskSerializes(t *testing.T) {
	if os.Getenv("E2E_FLOW_SERIAL") != "1" {
		t.Skip("set E2E_FLOW_SERIAL=1 to queue two events for one flow task")
	}
	if suite.live {
		t.Skip("flow serialization uses the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = []string{"FLOWHUB_FLOW_CONCURRENCY=2"}
	fake := newFakeOpencode(t)
	fake.setMode("every")
	hold := make(chan struct{})
	fake.setHold(hold)
	server := fake.start()
	t.Cleanup(server.Close)
	t.Cleanup(fake.release)
	suite.pointAtRuntime(t, server.URL)
	suite.setRuntimeMaxConcurrent(t, 2)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("on", []string{suite.ytProject}); err != nil {
		t.Fatalf("set on: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	key := "TEST-FLOW-SERIAL-" + suffix
	t.Cleanup(func() { suite.removeTestWorktree(key) })
	summary := "FlowHub flow serialization " + suffix
	if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Flow serialization E2E.", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("issue webhook = %d", status)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && fake.promptCount() < 1 {
		time.Sleep(100 * time.Millisecond)
	}
	if fake.promptCount() != 1 {
		t.Fatalf("prompts = %d, want 1", fake.promptCount())
	}
	// A second event for the same task must queue behind the held analysis; it
	// must not start a second prompt on the same session.
	if status := suite.postWebhook(t, commentAddedPayload(key, "plan-"+suffix, "/opencode start", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("comment webhook = %d", status)
	}
	time.Sleep(500 * time.Millisecond)
	if got := fake.promptCount(); got != 1 {
		t.Fatalf("same-task event started concurrently: prompts=%d", got)
	}
	fake.release()
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && fake.promptCount() < 2 {
		time.Sleep(100 * time.Millisecond)
	}
	if fake.promptCount() != 2 {
		t.Fatalf("prompts after release = %d, want 2", fake.promptCount())
	}
	row := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Turns == 2 && row.State == "awaiting_input"
	}, 30*time.Second)
	if row.Session == "" || row.Worktree == "" {
		t.Fatalf("task has no worktree/session: %+v", row)
	}
}

func TestFlowConcurrentTasks(t *testing.T) {
	if os.Getenv("E2E_FLOW_CONCURRENCY") != "1" {
		t.Skip("set E2E_FLOW_CONCURRENCY=1 to run two flow tasks concurrently")
	}
	if suite.live {
		t.Skip("flow concurrency uses the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = []string{"FLOWHUB_FLOW_CONCURRENCY=2"}
	fake := newFakeOpencode(t)
	fake.setMode("every")
	hold := make(chan struct{})
	fake.setHold(hold)
	server := fake.start()
	t.Cleanup(server.Close)
	t.Cleanup(fake.release)
	suite.pointAtRuntime(t, server.URL)
	suite.setRuntimeMaxConcurrent(t, 2)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("on", []string{suite.ytProject}); err != nil {
		t.Fatalf("set on: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	keys := []string{"TEST-FLOW-A-" + suffix, "TEST-FLOW-B-" + suffix}
	for _, key := range keys {
		key := key
		t.Cleanup(func() { suite.removeTestWorktree(key) })
		summary := "FlowHub flow concurrency " + key
		if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Flow concurrency E2E.", suite.ytProject)); status != http.StatusAccepted {
			t.Fatalf("webhook for %s = %d", key, status)
		}
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && fake.promptCount() < 2 {
		time.Sleep(100 * time.Millisecond)
	}
	if fake.promptCount() != 2 {
		t.Fatalf("flow prompts = %d, want 2", fake.promptCount())
	}
	if got := fake.maxConcurrentPrompts(); got != 2 {
		t.Fatalf("max concurrent flow prompts = %d, want 2", got)
	}
	fake.release()
	for _, key := range keys {
		row := suite.waitForRegistry(t, key, func(row taskRow) bool {
			return row.Turns == 1 && row.State == "awaiting_input"
		}, 30*time.Second)
		if row.Session == "" || row.Worktree == "" {
			t.Fatalf("task %s has no worktree/session: %+v", key, row)
		}
	}
}

func TestCancelStopsAnActiveRun(t *testing.T) {
	if os.Getenv("E2E_CANCEL") != "1" {
		t.Skip("set E2E_CANCEL=1 to cancel an active on run")
	}
	if suite.live {
		t.Skip("cancel uses the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	fake.setMode("every")
	hold := make(chan struct{})
	fake.setHold(hold)
	fake.setHoldAt(3)
	server := fake.start()
	t.Cleanup(server.Close)
	t.Cleanup(fake.release)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("on", []string{suite.ytProject}); err != nil {
		t.Fatalf("set on: %v", err)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	key := "TEST-CANCEL-" + suffix
	t.Cleanup(func() { suite.removeTestWorktree(key) })
	summary := "FlowHub E2E cancel " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	deliver := func(payload []byte) {
		t.Helper()
		if status := suite.postWebhook(t, payload); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
	}
	waitPrompts := func(want int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if fake.promptCount() >= want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("prompts = %d, want %d", fake.promptCount(), want)
	}
	deliver(issueCreatedPayload(key, summary, "Cancel E2E.", suite.ytProject))
	waitPrompts(1)
	suite.waitForRegistry(t, key, func(row taskRow) bool { return row.Turns == 1 }, 30*time.Second)
	comment := "plan-" + suffix
	deliver(commentAddedPayload(key, comment, "/opencode start", suite.ytProject))
	waitPrompts(2)
	suite.waitForRegistry(t, key, func(row taskRow) bool { return row.Turns == 2 && row.Plan == "draft" }, 30*time.Second)
	time.Sleep(1100 * time.Millisecond)
	comment = "execute-" + suffix
	deliver(commentAddedPayload(key, comment, "/opencode start", suite.ytProject))
	waitPrompts(3)
	run := suite.waitForRunState(t, key, "running", 30*time.Second)
	status, reply := suite.apiRequest(t, http.MethodPost, "/hub/v1/runs/"+run.ID+"/cancel", suite.hubToken, nil)
	if status != http.StatusOK {
		t.Fatalf("cancel status = %d: %s", status, reply)
	}
	var body struct {
		Run struct {
			State string `json:"state"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(reply), &body); err != nil {
		t.Fatalf("cancel body: %v (%s)", err, reply)
	}
	if body.Run.State != "canceled" {
		t.Fatalf("cancel state = %q, want canceled", body.Run.State)
	}
	canceled := suite.waitForRunState(t, key, "canceled", 20*time.Second)
	if canceled.ID != run.ID {
		t.Fatalf("canceled run = %s, want %s", canceled.ID, run.ID)
	}
	status, _ = suite.apiRequest(t, http.MethodPost, "/hub/v1/runs/"+run.ID+"/cancel", suite.hubToken, nil)
	if status != http.StatusConflict {
		t.Fatalf("second cancel status = %d, want 409", status)
	}
	status, _ = suite.apiRequest(t, http.MethodPost, "/hub/v1/runs/run_does_not_exist/cancel", suite.hubToken, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown cancel status = %d, want 404", status)
	}
	time.Sleep(time.Second)
	if got := fake.promptCount(); got != 3 {
		t.Fatalf("prompts after cancel = %d, want 3", got)
	}
	journal, err := os.ReadFile(filepath.Join(suite.dataDir, "runs", run.ID, "journal.jsonl"))
	if err != nil {
		t.Fatalf("read cancel journal: %v", err)
	}
	if !strings.Contains(string(journal), `"type":"run.canceled"`) {
		t.Fatalf("cancel journal has no run.canceled event:\n%s", journal)
	}
	for _, row := range suite.runRows(t) {
		if row.Task == key && row.State == "running" {
			t.Fatalf("run still running after cancel: %+v", row)
		}
	}
}

func TestAgedCursorRefusesStartup(t *testing.T) {
	if os.Getenv("E2E_AGED_CURSOR") != "1" {
		t.Skip("set E2E_AGED_CURSOR=1 to check an aged-out replay cursor")
	}
	if suite.live {
		t.Skip("cursor checks use the direct receiver")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	server := fake.start()
	t.Cleanup(server.Close)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	suite.stop()
	cursor := filepath.Join(suite.dataDir, "flow-cursor.json")
	if err := os.WriteFile(cursor, []byte(`{"event_id":"evt_missing_from_the_log","updated_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	out := suite.startExpectFailure(t, 20*time.Second)
	t.Logf("aged-cursor startup output:\n%s", out)
	if !strings.Contains(strings.ToLower(out), "cursor") || !strings.Contains(strings.ToLower(out), "not found") {
		t.Fatalf("startup did not report the aged cursor:\n%s", out)
	}
}

func TestFlowPauseAndCatchUp(t *testing.T) {
	if os.Getenv("E2E_FLOW_CONTROL") != "1" {
		t.Skip("set E2E_FLOW_CONTROL=1 to run flow pause/project-scope checks")
	}
	if suite.live {
		t.Skip("flow-control checks use the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	fake.setMode("every")
	server := fake.start()
	t.Cleanup(server.Close)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("on", []string{suite.ytProject}); err != nil {
		t.Fatalf("set on: %v", err)
	}
	pause := filepath.Join(suite.dir, "DISPATCH_OFF")
	if err := os.WriteFile(pause, []byte("off"), 0o600); err != nil {
		t.Fatalf("pause file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(pause) })

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	key := "TEST-PAUSE-" + suffix
	t.Cleanup(func() { suite.removeTestWorktree(key) })
	summary := "FlowHub E2E pause " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Pause E2E.", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("webhook status = %d", status)
	}
	time.Sleep(700 * time.Millisecond)
	if row, ok := suite.lastTaskRow(key); ok {
		t.Fatalf("paused flow created a task: %+v", row)
	}
	if got := fake.promptCount(); got != 0 {
		t.Fatalf("paused flow caused %d prompt(s)", got)
	}

	if err := os.Remove(pause); err != nil {
		t.Fatalf("remove pause: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for fake.promptCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if fake.promptCount() == 0 {
		t.Fatal("unpaused flow did not catch up the paused event")
	}
	suite.waitForRegistry(t, key, func(row taskRow) bool { return row.Worktree != "" }, 20*time.Second)
	suite.waitForRunState(t, key, "succeeded", 20*time.Second)
}

func TestProjectScopedMode(t *testing.T) {
	if os.Getenv("E2E_FLOW_CONTROL") != "1" {
		t.Skip("set E2E_FLOW_CONTROL=1 to run flow pause/project-scope checks")
	}
	if suite.live {
		t.Skip("flow-control checks use the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	fake.setMode("every")
	server := fake.start()
	t.Cleanup(server.Close)
	suite.pointAtRuntime(t, server.URL)
	suite.addProject(t, "OTHER")
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("on", []string{suite.ytProject}); err != nil {
		t.Fatalf("set on: %v", err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	testKey := "TEST-SCOPE-" + suffix
	otherKey := "OTHER-" + suffix
	t.Cleanup(func() {
		suite.removeTestWorktree(testKey)
		suite.removeTestWorktree(otherKey)
	})
	for _, tc := range []struct{ key, project string }{{testKey, suite.ytProject}, {otherKey, "OTHER"}} {
		summary := "FlowHub E2E scope " + tc.project + " " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
		if status := suite.postWebhook(t, issueCreatedPayload(tc.key, summary, "Scope E2E.", tc.project)); status != http.StatusAccepted {
			t.Fatalf("%s webhook status = %d", tc.project, status)
		}
	}
	suite.waitForRegistry(t, testKey, func(row taskRow) bool { return row.Worktree != "" }, 20*time.Second)
	other := suite.waitForRegistry(t, otherKey, func(row taskRow) bool { return row.Worktree != "" }, 20*time.Second)
	if other.Worktree == "" {
		t.Fatal("OTHER has no dispatch worktree")
	}
	deadline := time.Now().Add(20 * time.Second)
	for fake.promptCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if fake.promptCount() != 2 {
		t.Fatalf("prompts = %d, want one per project", fake.promptCount())
	}
	if fake.sessionCount() != 2 {
		t.Fatalf("sessions = %d, want two", fake.sessionCount())
	}
	runs := suite.runRows(t)
	flowRuns, otherRuns := 0, 0
	for _, run := range runs {
		switch run.Task {
		case testKey:
			flowRuns++
		case otherKey:
			otherRuns++
		}
	}
	if flowRuns == 0 {
		t.Fatal("TEST did not get a flow run")
	}
	if otherRuns != 0 {
		t.Fatalf("OTHER got %d flow run(s) despite being outside the on scope", otherRuns)
	}
}

func TestOpencodeUnavailableAtStartup(t *testing.T) {
	if os.Getenv("E2E_DOWN") != "1" {
		t.Skip("set E2E_DOWN=1 to run runtime failure paths")
	}
	if suite.live {
		t.Skip("runtime failure paths use the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	suite.pointAtRuntime(t, "http://127.0.0.1:1")
	out := suite.startExpectFailure(t, 20*time.Second)
	if !strings.Contains(strings.ToLower(out), "opencode") {
		t.Fatalf("startup refusal did not mention opencode:\n%s", out)
	}
}

func TestOpencodeUnavailableBeforeDelivery(t *testing.T) {
	if os.Getenv("E2E_DOWN") != "1" {
		t.Skip("set E2E_DOWN=1 to run runtime failure paths")
	}
	if suite.live {
		t.Skip("runtime failure paths use the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	server := fake.start()
	t.Cleanup(server.Close)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}
	server.Close()

	summary := "FlowHub E2E runtime down " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, "Runtime-down E2E.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Runtime-down E2E.", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("webhook status = %d", status)
	}
	// No runtime can serve the project: the delivery is ignored, not turned into a
	// task row that claims work happened.
	time.Sleep(700 * time.Millisecond)
	if row, ok := suite.lastTaskRow(key); ok {
		t.Fatalf("an unavailable runtime still created a task row: %+v", row)
	}
}

func TestOpencodePromptFailure(t *testing.T) {
	if os.Getenv("E2E_DOWN") != "1" {
		t.Skip("set E2E_DOWN=1 to run runtime failure paths")
	}
	if suite.live {
		t.Skip("runtime failure paths use the fake runtime in direct mode")
	}
	suite.stop()
	suite.extraEnv = nil
	fake := newFakeOpencode(t)
	hold := make(chan struct{})
	fake.setHold(hold)
	server := fake.start()
	t.Cleanup(server.Close)
	t.Cleanup(fake.release)
	suite.pointAtRuntime(t, server.URL)
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}

	summary := "FlowHub E2E prompt failure " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, "Prompt-failure E2E.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Prompt-failure E2E.", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("webhook status = %d", status)
	}
	suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Worktree != ""
	}, 20*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for fake.promptCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if fake.promptCount() == 0 {
		t.Fatal("fake runtime never received the prompt")
	}
	fake.setFailPrompt(true)
	fake.release()
	row := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.State == "failed"
	}, 20*time.Second)
	if row.State == "awaiting_input" {
		t.Fatal("a failed prompt was recorded awaiting_input")
	}
	if !strings.Contains(strings.ToLower(row.LastError), "500") &&
		!strings.Contains(strings.ToLower(row.LastError), "internal server error") {
		t.Fatalf("failure reason = %q, want the fake 500", row.LastError)
	}
}

func TestOpencodeRequestTimeout(t *testing.T) {
	if os.Getenv("E2E_DOWN") != "1" {
		t.Skip("set E2E_DOWN=1 to run runtime failure paths")
	}
	if suite.live {
		t.Skip("runtime failure paths use the fake runtime in direct mode")
	}
	suite.stop()
	fake := newFakeOpencode(t)
	fake.setDelay(5 * time.Second)
	server := fake.start()
	t.Cleanup(server.Close)
	suite.pointAtRuntime(t, server.URL)
	suite.extraEnv = []string{"FLOWHUB_OPENCODE_REQUEST_TIMEOUT=1s"}
	suite.start()
	t.Cleanup(func() { suite.stop() })
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}

	start := time.Now()
	summary := "FlowHub E2E request timeout " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, "Request-timeout E2E.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Request-timeout E2E.", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("webhook status = %d", status)
	}
	row := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.State == "failed"
	}, 20*time.Second)
	if row.State == "awaiting_input" {
		t.Fatal("a timed-out turn was recorded awaiting_input")
	}
	if !strings.Contains(strings.ToLower(row.LastError), "deadline") &&
		!strings.Contains(strings.ToLower(row.LastError), "timeout") {
		t.Fatalf("failure reason = %q, want a deadline/timeout error", row.LastError)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("request timeout took %s with a 1s request bound; the turn deadline leaked in", elapsed)
	}
}

func (e *env) addComment(issue, text string) (string, error) {
	body := map[string]any{"text": text}
	var response struct {
		ID string `json:"id"`
	}
	if err := e.ytRequest(http.MethodPost, "/api/issues/"+issue+"/comments?fields=id", body, &response); err != nil {
		return "", err
	}
	return response.ID, nil
}

func (e *env) flowDropped(t *testing.T) float64 {
	t.Helper()
	var body struct {
		Flows struct {
			Dropped float64 `json:"dropped"`
		} `json:"flows"`
	}
	if err := e.getJSON(t, "/healthz", &body); err != nil {
		t.Fatalf("health: %v", err)
	}
	return body.Flows.Dropped
}

func commentAddedPayload(key, commentID, text, project string) []byte {
	payload := map[string]any{
		"event":     "commentAdded",
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"id":        key,
		"summary":   "FlowHub E2E trigger",
		"project":   map[string]any{"key": project, "name": project, "shortName": project},
		"comments": []map[string]any{{
			"id": commentID, "text": text,
			"created": time.Now().UnixMilli(),
			"author":  map[string]any{"login": "yusiwen", "fullName": "Siwen Yu"},
		}},
	}
	data, _ := json.Marshal(payload)
	return data
}

func eventLineCount(t *testing.T, dataDir string) int {
	t.Helper()
	path := filepath.Join(dataDir, "events", time.Now().UTC().Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

func TestRestartKeepsComparisonState(t *testing.T) {
	if os.Getenv("E2E_RESTART") != "1" {
		t.Skip("set E2E_RESTART=1 to stop and restart the test FlowHub")
	}
	if suite.live {
		t.Skip("restart is checked in direct mode")
	}
	t.Cleanup(func() { _ = suite.setMode("off", nil) })
	if err := suite.setMode("shadow", nil); err != nil {
		t.Fatalf("set shadow: %v", err)
	}
	before := suite.flowHealth(t)
	description := "FlowHub restart before " + time.Now().UTC().Format(time.RFC3339Nano)
	if err := suite.updateIssue(suite.ytIssue, description); err != nil {
		t.Fatalf("update: %v", err)
	}
	if status := suite.postWebhook(t, issueUpdatedPayload(suite.ytIssue, description, time.Now().UTC().Format("2006-01-02T15:04:05Z"), suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("webhook status = %d", status)
	}
	suite.waitForFlowHealth(t, func(f flowHealth) bool { return f.ShadowCompared > before.ShadowCompared }, 20*time.Second)
	beforeRestart := suite.flowHealth(t)

	suite.stop()
	suite.start()
	time.Sleep(300 * time.Millisecond)

	afterRestart := suite.flowHealth(t)
	if afterRestart.Mode != "shadow" {
		t.Fatalf("mode after restart = %q, want shadow", afterRestart.Mode)
	}
	if afterRestart.ShadowCompared < beforeRestart.ShadowCompared {
		t.Fatalf("compare count regressed: before=%v after=%v", beforeRestart.ShadowCompared, afterRestart.ShadowCompared)
	}
	description = "FlowHub restart after " + time.Now().UTC().Format(time.RFC3339Nano)
	if err := suite.updateIssue(suite.ytIssue, description); err != nil {
		t.Fatalf("update after restart: %v", err)
	}
	if status := suite.postWebhook(t, issueUpdatedPayload(suite.ytIssue, description, time.Now().UTC().Format("2006-01-02T15:04:05Z"), suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("webhook after restart status = %d", status)
	}
	suite.waitForFlowHealth(t, func(f flowHealth) bool { return f.ShadowCompared > afterRestart.ShadowCompared }, 20*time.Second)
}

func TestPermitAndRevoke(t *testing.T) {
	if os.Getenv("E2E_PERMIT") != "1" {
		t.Skip("set E2E_PERMIT=1 to run the real permit/revoke path")
	}
	runPermit(t, false)
}

func TestOnLivePermitAndRevoke(t *testing.T) {
	if os.Getenv("E2E_ON_PERMIT") != "1" {
		t.Skip("set E2E_ON_PERMIT=1 to run permit/revoke through the on flow")
	}
	if !suite.live {
		t.Skip("the on permit journey requires live webhook delivery")
	}
	runPermit(t, true)
}

func runPermit(t *testing.T, on bool) {
	t.Helper()
	if on && !suite.live {
		t.Skip("the on permit journey requires live webhook delivery")
	}
	t.Cleanup(func() { _ = suite.setMode("off", nil) })
	modeName := "off"
	projectScope := []string(nil)
	if on {
		modeName = "on"
		projectScope = []string{suite.ytProject}
	}
	if err := suite.setMode(modeName, projectScope); err != nil {
		t.Fatalf("set %s: %v", modeName, err)
	}
	time.Sleep(300 * time.Millisecond)
	beforeFlow := suite.flowHealth(t)
	beforeDispatch := suite.dispatchHandled(t)

	prompt := "Inspect the repository and report every git worktree attached to it. " +
		"Follow the project instructions for the required inspection."
	prefix := "FlowHub E2E permit "
	if on {
		prefix = "FlowHub E2E on permit "
	}
	summary := prefix + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, prompt)
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if !suite.live {
		if status := suite.postWebhook(t, issueCreatedPayload(key, summary, prompt, suite.ytProject)); status != http.StatusAccepted {
			t.Fatalf("webhook status = %d", status)
		}
		suite.waitForEvent(t, key, "issue.created", 15*time.Second)
	}
	if on {
		suite.waitForFlowHealth(t, func(f flowHealth) bool { return f.Runs > beforeFlow.Runs }, 6*time.Minute)
	} else {
		suite.waitForDispatchHandled(t, 1, 6*time.Minute)
	}
	suite.waitForComment(t, key, "<!-- flowhub-auto -->", 6*time.Minute)
	row := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return len(row.Refusals) > 0
	}, 6*time.Minute)
	command := row.Refusals[0].Command
	session := row.Session
	t.Logf("refused command: %s", command)

	turnsBefore := row.Turns
	permit, err := suite.addComment(key, "/opencode permit")
	if err != nil {
		t.Fatalf("permit comment: %v", err)
	}
	if !suite.live {
		if status := suite.postWebhook(t, commentAddedPayload(key, permit, "/opencode permit", suite.ytProject)); status != http.StatusAccepted {
			t.Fatalf("permit webhook status = %d", status)
		}
	}
	suite.waitForRegistry(t, key, func(row taskRow) bool {
		if row.Grant == nil || len(row.Grant.Commands) == 0 || row.Turns <= turnsBefore {
			return false
		}
		for _, granted := range row.Grant.Commands {
			if granted == command {
				return true
			}
		}
		return false
	}, 6*time.Minute)
	suite.waitForComment(t, key, "<!-- flowhub-auto -->", 6*time.Minute)
	if on {
		suite.waitForFlowHealth(t, func(f flowHealth) bool { return f.Runs >= beforeFlow.Runs+2 }, 6*time.Minute)
	}
	if row, ok := suite.lastTaskRow(key); ok && row.Session != "" && row.Session != session {
		t.Fatalf("session changed across permit: before=%s after=%s", session, row.Session)
	}

	revoke, err := suite.addComment(key, "/opencode revoke")
	if err != nil {
		t.Fatalf("revoke comment: %v", err)
	}
	if !suite.live {
		if status := suite.postWebhook(t, commentAddedPayload(key, revoke, "/opencode revoke", suite.ytProject)); status != http.StatusAccepted {
			t.Fatalf("revoke webhook status = %d", status)
		}
	}
	suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Grant == nil || len(row.Grant.Commands) == 0
	}, 20*time.Second)
	if on {
		suite.waitForFlowHealth(t, func(f flowHealth) bool { return f.Runs >= beforeFlow.Runs+3 }, 30*time.Second)
		if !suite.hasRunMode(t, "on") {
			t.Fatal("no on run appeared in /hub/v1/runs")
		}
		if after := suite.dispatchHandled(t); after != beforeDispatch {
			t.Fatalf("dispatch handled a delivery while the flow owned the project: before=%v after=%v", beforeDispatch, after)
		}
	}
}

type taskRow struct {
	TaskKey    string  `json:"task_key"`
	State      string  `json:"state"`
	Worktree   string  `json:"worktree"`
	Session    string  `json:"session_id"`
	Plan       string  `json:"plan_state"`
	LastAction string  `json:"last_action"`
	LastError  string  `json:"last_error"`
	Cost       float64 `json:"cost"`
	Turns      int     `json:"turns"`
	Refusals   []struct {
		Command string `json:"command"`
	} `json:"refusals"`
	Grant *struct {
		Commands []string `json:"commands"`
	} `json:"grant"`
}

func (e *env) lastTaskRow(key string) (taskRow, bool) {
	data, err := os.ReadFile(filepath.Join(e.dataDir, "registry.jsonl"))
	if err != nil {
		return taskRow{}, false
	}
	var found taskRow
	ok := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row taskRow
		if json.Unmarshal([]byte(line), &row) == nil && row.TaskKey == key {
			found, ok = row, true
		}
	}
	return found, ok
}

func (e *env) waitForRegistry(t *testing.T, key string, ok func(taskRow) bool, timeout time.Duration) taskRow {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last taskRow
	for time.Now().Before(deadline) {
		if row, found := e.lastTaskRow(key); found {
			last = row
			if ok(row) {
				return row
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("registry for %s did not reach the expected state (last %+v)", key, last)
	return last
}

func TestCrashRecovery(t *testing.T) {
	if os.Getenv("E2E_CRASH") != "1" {
		t.Skip("set E2E_CRASH=1 to SIGKILL the test FlowHub during a turn")
	}
	if suite.live {
		t.Skip("crash recovery is checked in direct mode")
	}
	if err := suite.setMode("off", nil); err != nil {
		t.Fatalf("set off: %v", err)
	}
	summary := "FlowHub E2E crash " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	key, _, err := suite.createIssue(summary, "Crash recovery E2E.")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if status := suite.postWebhook(t, issueCreatedPayload(key, summary, "Crash recovery E2E.", suite.ytProject)); status != http.StatusAccepted {
		t.Fatalf("webhook status = %d", status)
	}
	before := suite.waitForSession(t, key, 20*time.Second)
	if before.Worktree == "" {
		t.Fatalf("task row has no worktree before the crash: %+v", before)
	}
	suite.kill()
	time.Sleep(500 * time.Millisecond)
	suite.start()
	row := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.State == "failed" && strings.Contains(row.LastError, "interrupted by a restart")
	}, 20*time.Second)
	if row.Worktree != before.Worktree {
		t.Fatalf("worktree changed across restart: before=%s after=%s", before.Worktree, row.Worktree)
	}
	if row.Turns > 1 {
		t.Fatalf("turns after restart = %d, want at most one", row.Turns)
	}
	if sessions := suite.openCodeSessionsIn(t, row.Worktree); sessions > 1 {
		t.Fatalf("opencode sessions after restart = %d, want at most one", sessions)
	}
}

func (e *env) waitForSession(t *testing.T, key string, timeout time.Duration) taskRow {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last taskRow
	for time.Now().Before(deadline) {
		if row, ok := e.lastTaskRow(key); ok {
			last = row
			if row.Worktree != "" && e.openCodeSessionsIn(t, row.Worktree) > 0 {
				return row
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("task %s never got an opencode session (last %+v)", key, last)
	return last
}

func TestLiveFullJourney(t *testing.T) {
	if os.Getenv("E2E_LIVE_JOURNEY") != "1" {
		t.Skip("set E2E_LIVE_JOURNEY=1 to run the full live journey")
	}
	runJourney(t, false)
}

func TestOnLiveFullJourney(t *testing.T) {
	if os.Getenv("E2E_ON_JOURNEY") != "1" {
		t.Skip("set E2E_ON_JOURNEY=1 to run the on-mode live journey")
	}
	runJourney(t, true)
}

func runJourney(t *testing.T, on bool) {
	t.Helper()
	if !suite.live {
		t.Skip("the full journey requires live webhook delivery")
	}
	t.Cleanup(func() { _ = suite.setMode("off", nil) })
	modeName := "off"
	if on {
		modeName = "on"
	}
	if err := suite.setMode(modeName, func() []string {
		if on {
			return []string{suite.ytProject}
		}
		return nil
	}()); err != nil {
		t.Fatalf("set %s: %v", modeName, err)
	}
	time.Sleep(300 * time.Millisecond)
	beforeFlow := suite.flowHealth(t)
	beforeDispatch := suite.dispatchHandled(t)
	marker := "E2E-JOURNEY-" + time.Now().UTC().Format("20060102T150405")
	summary := "FlowHub live journey " + marker
	description := fmt.Sprintf("Add one line to README.md containing exactly `%s`. Do not change any other file. Commit the change in the task worktree.", marker)
	key, _, err := suite.createIssue(summary, description)
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}

	// 1. The issueCreated webhook starts the analysis turn.
	suite.waitForComment(t, key, "<!-- flowhub-auto -->", 6*time.Minute)
	_, state := suite.waitForTurnState(t, key, 2*time.Minute)
	analyzed := suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Session != "" && row.Plan == "none" && row.Turns == 1
	}, 20*time.Second)
	if state == "failed" {
		t.Fatalf("analysis ended failed")
	}
	session := analyzed.Session

	// 2. The first /opencode start must plan, not execute.
	beforeComments := suite.commentCount(t, key)
	if _, err := suite.addComment(key, "/opencode start"); err != nil {
		t.Fatalf("plan comment: %v", err)
	}
	suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.Plan == "draft" && row.Turns == 2 && row.Session == session
	}, 6*time.Minute)
	suite.waitForCommentCount(t, key, beforeComments+1, 6*time.Minute)
	planned := suite.lastTaskRowOrFail(t, key)

	// 3. The second /opencode start executes the plan in the same session.
	beforeComments = suite.commentCount(t, key)
	if _, err := suite.addComment(key, "/opencode start"); err != nil {
		t.Fatalf("execute comment: %v", err)
	}
	suite.waitForRegistry(t, key, func(row taskRow) bool {
		return row.State == "done" && row.Turns == 3 && row.Session == session
	}, 6*time.Minute)
	suite.waitForCommentCount(t, key, beforeComments+1, 6*time.Minute)
	executed := suite.lastTaskRowOrFail(t, key)
	if executed.LastAction != "execute" {
		t.Fatalf("last_action = %q, want execute", executed.LastAction)
	}
	if executed.Worktree == "" || planned.Worktree != executed.Worktree {
		t.Fatalf("worktree changed: planned=%q executed=%q", planned.Worktree, executed.Worktree)
	}
	suite.waitForFileContains(t, filepath.Join(executed.Worktree, "README.md"), marker, 2*time.Minute)

	if on {
		suite.waitForFlowHealth(t, func(f flowHealth) bool {
			return f.Mode == "on" && f.Runs >= beforeFlow.Runs+3
		}, 30*time.Second)
		if !suite.hasRunMode(t, "on") {
			t.Fatal("no on run appeared in /hub/v1/runs")
		}
		if after := suite.dispatchHandled(t); after != beforeDispatch {
			t.Fatalf("dispatch handled a delivery while the flow owned the project: before=%v after=%v", beforeDispatch, after)
		}
	}
}
func (e *env) lastTaskRowOrFail(t *testing.T, key string) taskRow {
	t.Helper()
	row, ok := e.lastTaskRow(key)
	if !ok {
		t.Fatalf("no registry row for %s", key)
	}
	return row
}

func (e *env) commentCount(t *testing.T, key string) int {
	t.Helper()
	var comments []any
	if err := e.ytRequest(http.MethodGet, "/api/issues/"+key+"/comments?fields=id", nil, &comments); err != nil {
		t.Fatalf("comments: %v", err)
	}
	return len(comments)
}

func (e *env) waitForCommentCount(t *testing.T, key string, minimum int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.commentCount(t, key) >= minimum {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("comments on %s did not reach %d within %s", key, minimum, timeout)
}

func (e *env) waitForFileContains(t *testing.T, path, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), needle) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s does not contain %q within %s", path, needle, timeout)
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
	var data []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		data = encoded
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var reader io.Reader
		if data != nil {
			reader = bytes.NewReader(data)
		}
		request, err := http.NewRequest(method, e.ytBase+path, reader)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+e.ytToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := e.ytClient.Do(request)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		if response.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s %s: %d", method, path, response.StatusCode)
			payload, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
			_ = response.Body.Close()
			if len(payload) > 0 {
				lastErr = fmt.Errorf("%s %s: %d %s", method, path, response.StatusCode, strings.TrimSpace(string(payload)))
			}
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode < 200 || response.StatusCode > 299 {
			payload, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
			return fmt.Errorf("%s %s: %d %s", method, path, response.StatusCode, strings.TrimSpace(string(payload)))
		}
		if out != nil {
			return json.NewDecoder(response.Body).Decode(out)
		}
		return nil
	}
	return lastErr
}

func issueUpdatedPayload(issue, description, timestamp, project string) []byte {
	payload := map[string]any{
		"event":       "issueUpdated",
		"timestamp":   timestamp,
		"id":          issue,
		"summary":     "FlowHub E2E webhook check",
		"project":     map[string]any{"key": project, "name": project, "shortName": project},
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

func writeTestConfig(path, repo, worktrees, project, promptFile, ocBase string) error {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".config", "flowhub", "config.json"))
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// The runtime declaration owns the URL; FLOWHUB_OPENCODE_URL alone is only the
	// fallback name's default, so a test runtime must be written here too.
	if runtimes, ok := raw["runtimes"].(map[string]any); ok {
		for _, value := range runtimes {
			if runtime, ok := value.(map[string]any); ok {
				runtime["url"] = ocBase
			}
		}
	}
	projects, _ := raw["projects"].([]any)
	var kept []any
	for _, item := range projects {
		entry, ok := item.(map[string]any)
		if ok && entry["project"] == project {
			entry["repo"].(map[string]any)["path"] = repo
			entry["worktrees"] = worktrees
			if promptFile != "" {
				entry["prompt_file"] = promptFile
			}
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

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
