package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/agent"
	"github.com/yusiwen/flowhub/internal/config"
	"github.com/yusiwen/flowhub/internal/dispatch"
	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/projectmap"
	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/runtimes"
	"github.com/yusiwen/flowhub/internal/source"
	"github.com/yusiwen/flowhub/internal/source/youtrack"
)

// versionLine feeds -version, the startup banner and /healthz. The Makefile
// injects Version from `git describe --always --dirty`, which already contains
// the short sha while the repository has no tags, so the line must not repeat it.
func TestVersionLineDoesNotRepeatTheCommit(t *testing.T) {
	original := struct{ version, commit, buildTime string }{Version, CommitSHA, BuildTime}
	t.Cleanup(func() {
		Version, CommitSHA, BuildTime = original.version, original.commit, original.buildTime
	})

	cases := map[string]struct {
		version, commit, buildTime string
		want                       string
	}{
		"describe already carries the sha": {
			version: "23ece6e-dirty", commit: "23ece6e",
			want: "23ece6e-dirty",
		},
		"tagged release prepends the commit once": {
			version: "v1.2.3", commit: "23ece6e",
			want: "v1.2.3 23ece6e",
		},
		"unknown commit is omitted": {
			version: "dev", commit: "unknown",
			want: "dev",
		},
		"build time is appended": {
			version: "v1.2.3", commit: "23ece6e", buildTime: "2026-09-20T12:00:00Z",
			want: "v1.2.3 23ece6e built 2026-09-20T12:00:00Z",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			Version, CommitSHA, BuildTime = tc.version, tc.commit, tc.buildTime
			got := versionLine()
			// vcsRevision() may add a revision when the commit is unknown; that is
			// environment dependent, so only assert the injected parts.
			if tc.commit == "unknown" {
				if !strings.HasPrefix(got, "dev") {
					t.Fatalf("versionLine() = %q, want it to start with %q", got, "dev")
				}
				return
			}
			if !strings.HasPrefix(got, tc.want) {
				t.Fatalf("versionLine() = %q, want it to start with %q", got, tc.want)
			}
			if tc.commit != "" && strings.Count(got, tc.commit) > 1 {
				t.Fatalf("versionLine() = %q repeats the commit", got)
			}
		})
	}
}

// TestWebhookOptionsNeverHoldATypedNilDispatcher guards a regression measured
// against a real process: with FLOWHUB_DISPATCH off, the receiver answered every
// delivery with an empty reply. The cause was a typed nil (*dispatch.Dispatcher)
// stored in the webhook.Dispatcher interface, which is not == nil, so the hook
// was called and panicked after the 202 had been written.
func TestWebhookOptionsNeverHoldATypedNilDispatcher(t *testing.T) {
	var absent *dispatch.Dispatcher
	if absent != nil {
		t.Fatal("precondition: a nil pointer must compare equal to nil")
	}
	if opts := webhookOptions(config.Config{}, absent, youtrack.New(rules.Policy{}, "")); opts.Dispatcher != nil {
		t.Fatal("webhookOptions put a nil dispatcher behind the interface")
	}
}

func TestWebhookOptionsCarryTheReceiverLocks(t *testing.T) {
	cfg := config.Config{
		HookKey:      "k",
		TokenHeader:  "X-YouTrack-Token",
		Token:        "t",
		MaxBodyBytes: 1024,
		ReplayWindow: time.Minute,
		LogHeaders:   true,
	}
	opts := webhookOptions(cfg, &dispatch.Dispatcher{}, youtrack.New(rules.Policy{}, ""))
	if opts.HookKey != "k" || opts.Token != "t" || opts.Dispatcher == nil {
		t.Fatalf("options = %+v", opts)
	}
}

// fakeAgentServer stands in for an opencode host during activation. Each case
// changes exactly one of the three things the probe reads.
func fakeAgentServer(t *testing.T, agents string, providers string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true,"version":"1.18.31"}`))
		case "/agent":
			_, _ = w.Write([]byte(agents))
		case "/provider":
			_, _ = w.Write([]byte(providers))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestProberRefusesAHostWithoutTheClaimedAgent is ADR 0002 step 5's acceptance
// criterion: a host whose agent server answers but has no such agent must fail
// activation with that reason. opencode would otherwise accept the session and run
// it under its own default agent, silently dropping the profile's step budget and
// permission block.
func TestProberRefusesAHostWithoutTheClaimedAgent(t *testing.T) {
	server := fakeAgentServer(t,
		`[{"name":"build","mode":"primary"}]`,
		`{"all":[{"id":"deepseek","models":{"deepseek-flash":{}}}]}`)

	err := opencodeProber{timeout: 2 * time.Second}.Probe(context.Background(), runtimes.Claim{
		Name: "builder-a", Advertise: server.URL, Agent: "opencode", AgentProfile: "devops",
		Models: map[string]string{"devops": "deepseek/deepseek-flash"},
	})
	if err == nil {
		t.Fatal("a host without the claimed agent was activated")
	}
	if !strings.Contains(err.Error(), `no agent named "devops"`) {
		t.Fatalf("the refusal does not name the missing agent: %v", err)
	}
}

// TestProberRefusesAModelTheHostCannotRun covers the other half: the model the
// host reported has to exist on the server that will run it, or the first turn is
// where the operator finds out.
func TestProberRefusesAModelTheHostCannotRun(t *testing.T) {
	server := fakeAgentServer(t,
		`[{"name":"devops","mode":"primary","model":{"providerID":"deepseek","modelID":"deepseek-v4-flash"}}]`,
		`{"all":[{"id":"deepseek","models":{"deepseek-flash":{},"deepseek-v4-pro":{}}}]}`)

	err := opencodeProber{timeout: 2 * time.Second}.Probe(context.Background(), runtimes.Claim{
		Name: "builder-a", Advertise: server.URL, Agent: "opencode", AgentProfile: "devops",
		Models: map[string]string{"devops": "deepseek/deepseek-v4-flash"},
	})
	if err == nil {
		t.Fatal("a model the provider does not offer was activated")
	}
	if !strings.Contains(err.Error(), "does not offer model deepseek/deepseek-v4-flash") ||
		!strings.Contains(err.Error(), "deepseek-flash") {
		t.Fatalf("the refusal does not name the gap and what is offered: %v", err)
	}
}

// TestProberAcceptsAStaleServerModel: the server's own registry entry may be older
// than the profile on disk, and that is no longer fatal — the dispatcher pins the
// model the host reported. It is still worth a warning, which is why this case
// asserts success with the stale entry present.
func TestProberAcceptsAStaleServerModel(t *testing.T) {
	server := fakeAgentServer(t,
		`[{"name":"devops","mode":"primary","model":{"providerID":"deepseek","modelID":"deepseek-v4-flash"}}]`,
		`{"all":[{"id":"deepseek","models":{"deepseek-flash":{}}}]}`)
	var logged bytes.Buffer

	err := opencodeProber{
		timeout: 2 * time.Second,
		log:     slog.New(slog.NewTextHandler(&logged, nil)),
	}.Probe(context.Background(), runtimes.Claim{
		Name: "builder-a", Advertise: server.URL, Agent: "opencode", AgentProfile: "devops",
		Models: map[string]string{"devops": "deepseek/deepseek-flash"},
	})
	if err != nil {
		t.Fatalf("a repaired profile was refused because the server's cache is stale: %v", err)
	}
	if !strings.Contains(logged.String(), "still reports an older model") {
		t.Fatalf("the stale server registry was not reported: %s", logged.String())
	}
}

// TestProberNeedsOnlyLivenessWhenNoProfileIsClaimed keeps the check honest for a
// host that names no agent profile: there is nothing to compare, so liveness is
// the whole question.
func TestProberNeedsOnlyLivenessWhenNoProfileIsClaimed(t *testing.T) {
	server := fakeAgentServer(t, `[]`, `{"all":[]}`)
	if err := (opencodeProber{timeout: 2 * time.Second}).Probe(context.Background(), runtimes.Claim{
		Name: "builder-a", Advertise: server.URL, Agent: "opencode",
	}); err != nil {
		t.Fatalf("a host with no claimed profile was refused: %v", err)
	}
}

// TestAgentCallsCarryTheConfiguredCredentials covers the wiring ADR 0001 step 2
// adds: `FLOWHUB_OPENCODE_USER`/`PASSWORD` have to reach *both* client builders —
// the runtime factory the dispatcher uses for every turn, and the activation prober
// that decides whether a host may take work. If only one of them sent the
// credentials, a remote runtime would enrol and then fail every turn.
//
// Since the credentials surface gained a shared file and per-runtime blocks, both
// builders resolve through projectmap.ResolveCredential, so this test pins the
// process-wide default reaching both paths.
func TestAgentCallsCarryTheConfiguredCredentials(t *testing.T) {
	t.Setenv("FLOWHUB_OPENCODE_USER", "flowhub")
	t.Setenv("FLOWHUB_OPENCODE_PASSWORD", "hunter2")

	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if ok {
			seen = append(seen, user+":"+password)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true,"version":"1.18.31"}`))
		case "/agent":
			_, _ = w.Write([]byte(`[{"name":"devops","mode":"primary"}]`))
		case "/provider":
			_, _ = w.Write([]byte(`{"all":[{"id":"deepseek","models":{"deepseek-flash":{}}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	projects := emptyProjects(t)
	cfg := config.Config{OpenCodeURL: server.URL, TaskDeadline: time.Minute}

	// The runtime the dispatcher builds for each host.
	runtime := newRuntimeFactory(cfg, projects, slog.New(slog.NewTextHandler(io.Discard, nil)))("builder-a", server.URL)
	if _, err := runtime.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}

	// The prober that decides whether the host can take work at all, with a claimed
	// profile and model so it reads the agent registry and the model catalogue too:
	// every path to a remote server has to carry the credentials.
	if err := (opencodeProber{cfg: cfg, projects: projects, timeout: 2 * time.Second}).Probe(context.Background(), runtimes.Claim{
		Name: "builder-a", Advertise: server.URL, AgentProfile: "devops",
		Models: map[string]string{"devops": "deepseek/deepseek-flash"},
	}); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if len(seen) < 4 {
		t.Fatalf("only %d request(s) carried credentials, want every path to send them", len(seen))
	}
	for _, credentials := range seen {
		if credentials != "flowhub:hunter2" {
			t.Fatalf("credentials = %q, want flowhub:hunter2", credentials)
		}
	}
}

// TestRuntimeCredentialsComeFromTheCredentialsFile is the acceptance test for the
// shared file: a runtime whose `auth` block only names a user takes its password from
// `credentials_file`, and **both** client builders use it — a file that reached only
// one of them would enrol a host and then fail every turn.
func TestRuntimeCredentialsComeFromTheCredentialsFile(t *testing.T) {
	t.Setenv("FLOWHUB_OPENCODE_USER", "")
	t.Setenv("FLOWHUB_OPENCODE_PASSWORD", "")

	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); ok {
			seen = append(seen, user+":"+password)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true,"version":"1.18.31"}`))
		case "/agent":
			_, _ = w.Write([]byte(`[{"name":"devops","mode":"primary"}]`))
		case "/provider":
			_, _ = w.Write([]byte(`{"all":[{"id":"deepseek","models":{"deepseek-flash":{}}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	credentials := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(credentials, []byte(`{"runtimes": {"builder-a": {"user": "flowhub", "password": "from-file"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	body := `{"version": 2, "credentials_file": "credentials.json",
	  "runtimes": {"builder-a": {"url": "` + server.URL + `", "auth": {"user": "flowhub"}}},
	  "projects": [{"source": "youtrack", "project": "TEST", "repo": {"path": "/tmp/test"}, "runtime": "builder-a"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(path)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}
	if problems := declaredAuthProblems(projects); len(problems) != 0 {
		t.Fatalf("declaredAuthProblems = %v, want none", problems)
	}

	cfg := config.Config{OpenCodeURL: server.URL, TaskDeadline: time.Minute}
	runtime := newRuntimeFactory(cfg, projects, slog.New(slog.NewTextHandler(io.Discard, nil)))("builder-a", server.URL)
	if _, err := runtime.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if err := (opencodeProber{cfg: cfg, projects: projects, timeout: 2 * time.Second}).Probe(context.Background(), runtimes.Claim{
		Name: "builder-a", Advertise: server.URL, AgentProfile: "devops",
		Models: map[string]string{"devops": "deepseek/deepseek-flash"},
	}); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if len(seen) < 4 {
		t.Fatalf("only %d request(s) carried credentials, want every path to send them", len(seen))
	}
	for _, got := range seen {
		if got != "flowhub:from-file" {
			t.Fatalf("credentials = %q, want flowhub:from-file", got)
		}
	}
}

// TestAWorldReadableConfigRefusesToStartWhenItHoldsAPassword pins the permission
// gate: a file that carries an inline password is itself a secret, and a group- or
// world-readable one refuses the start. A file that only names a variable is exempt,
// because refusing it would break deployments that hold no secret at all.
func TestAWorldReadableConfigRefusesToStartWhenItHoldsAPassword(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	load := func(body string, mode os.FileMode) *projectmap.Map {
		t.Helper()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		projects, err := projectmap.Load(path)
		if err != nil {
			t.Fatalf("projectmap.Load: %v", err)
		}
		return projects
	}
	inline := `{"version": 2, "runtimes": {"local": {"url": "http://127.0.0.1:4096", "auth": {"user": "opencode", "password": "s3cret"}}},
	  "projects": [{"source": "youtrack", "project": "TEST", "repo": {"path": "/tmp/test"}, "runtime": "local"}]}`
	viaEnv := `{"version": 2, "runtimes": {"local": {"url": "http://127.0.0.1:4096", "auth": {"user": "opencode", "password_env": "LOCAL_PASSWORD"}}},
	  "projects": [{"source": "youtrack", "project": "TEST", "repo": {"path": "/tmp/test"}, "runtime": "local"}]}`

	projects := load(inline, 0o600)
	if problems := configFileSecretProblems(filepath.Join(dir, "config.json"), projects); len(problems) != 0 {
		t.Fatalf("a 0600 file holding a password was refused: %v", problems)
	}
	if !projects.HasInlinePassword() {
		t.Fatal("HasInlinePassword = false for a file that carries one")
	}

	worldReadable := load(inline, 0o644)
	if problems := configFileSecretProblems(filepath.Join(dir, "config.json"), worldReadable); len(problems) == 0 {
		t.Fatal("a 0644 file holding a password was accepted")
	}

	noSecret := load(viaEnv, 0o644)
	if noSecret.HasInlinePassword() {
		t.Fatal("HasInlinePassword = true for a file that only names a variable")
	}
	if problems := configFileSecretProblems(filepath.Join(dir, "config.json"), noSecret); len(problems) != 0 {
		t.Fatalf("a 0644 file naming a variable was refused: %v", problems)
	}
}

// emptyProjects is a loaded-but-declares-nothing project map, for the tests that
// exercise a wiring path which only needs the type.
func emptyProjects(t *testing.T) *projectmap.Map {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"version": 2, "projects": [{"source": "youtrack", "project": "TEST", "repo": {"path": "/tmp/test"}}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(path)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}
	return projects
}

// TestTheSourcePromptFileReachesThePrompt pins the wiring main performs for a
// source-level `prompt_file`: `sources.Register` plus `sources.Build` with the text
// the loader read.
//
// It exists because the feature was read, validated, and then never passed on — the
// source adapter was built with the policy alone, so a configured `prompt_file` had
// no effect at all (found 2026-10-08, fixed with source.Factory's `instructions`
// parameter). A test that only exercised `youtrack.New` directly cannot catch that
// class of defect: the call site is what was wrong.
func TestTheSourcePromptFileReachesThePrompt(t *testing.T) {
	dir := t.TempDir()
	prompts := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(prompts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prompts, "youtrack.md"), []byte("Ask before touching the billing module.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	body := `{"version": 2,
	  "sources": {"youtrack": {"prompt_file": "prompts/youtrack.md"}},
	  "projects": [{"source": "youtrack", "project": "TEST", "repo": {"path": "/tmp/test"}}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(path)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}

	// Exactly the two calls main makes.
	sources := source.NewRegistry()
	if err := sources.Register(youtrack.SourceName, func(policy rules.Policy, instructions string) source.Source {
		return youtrack.New(policy, instructions)
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resolved := projects.Policy("youtrack", projectmap.PolicyDefaults{})
	src, err := sources.Build("youtrack", resolved.Policy, projects.SourcePromptExtra("youtrack"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	delivery := event.Event{Subject: event.Subject{Key: "TEST-1", Project: "TEST", Title: "t"}}
	prompt := src.Prompt(rules.ActionAnalyze, &delivery, rules.PromptContext{Worktree: "/wt/TEST-1"})
	if !strings.Contains(prompt, "Ask before touching the billing module.") {
		t.Fatalf("the source prompt file did not reach the prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "### This source") {
		t.Fatalf("the source instructions were not rendered under their own heading:\n%s", prompt)
	}
}

// TestTheEnvFileReportNamesVariablesNeverValues keeps the report safe to paste: the
// environment file holds the URL key and the token, so the report may say which
// variables it supplied and which the real environment overrode, and nothing else.
func TestTheEnvFileReportNamesVariablesNeverValues(t *testing.T) {
	result := config.EnvFileResult{
		Path:    "/etc/flowhub/.env",
		Found:   true,
		Applied: []string{"FLOWHUB_HOOK_KEY", "FLOWHUB_TOKEN"},
		Skipped: []string{"FLOWHUB_ADDR"},
	}
	rendered := envFileReport(result, nil)

	for _, want := range []string{
		"/etc/flowhub/.env",
		"FLOWHUB_HOOK_KEY",
		"FLOWHUB_TOKEN",
		"FLOWHUB_ADDR",
		"already set in the environment",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the report is missing %q:\n%s", want, rendered)
		}
	}

	// The three states a reader has to be able to tell apart.
	if got := envFileReport(config.EnvFileResult{Path: "/etc/flowhub/.env"}, nil); !strings.Contains(got, "absent") {
		t.Errorf("an absent file was not reported as absent: %q", got)
	}
	if got := envFileReport(config.EnvFileResult{}, nil); !strings.Contains(got, "disabled") {
		t.Errorf("a disabled file was not reported as disabled: %q", got)
	}
	if got := envFileReport(config.EnvFileResult{}, errEnvFileProbe); got != "" {
		t.Errorf("a failure belongs to the start's error, not to a report line: %q", got)
	}
}

var errEnvFileProbe = errors.New("environment file refused")

// TestTheFallbackProbeUsesTheRuntimeCredentials is the regression test for a 401
// found live on 2026-10-08: a runtime whose `auth` block named a variable was refused
// at startup — "opencode at … is not answering: GET /global/health: HTTP 401" — while
// the same credentials served the turns fine.
//
// The fallback runtime (`FLOWHUB_OPENCODE_URL`, named `default`) was built from
// `cfg.OpenCodeUser`/`Password`, which only ever holds the process-wide environment
// pair, and it is the runtime the reachability probe uses when no host is enrolled.
// The test asserts the probe accepts exactly the right pair and that a wrong one still
// fails, so a future change cannot make the probe "pass" by dropping the header.
func TestTheFallbackProbeUsesTheRuntimeCredentials(t *testing.T) {
	// The variables this test drives are cleared so the assertion is about the file.
	for _, name := range []string{"OSCRUB_OPENCODE_PASSWORD", "FLOWHUB_OPENCODE_USER", "FLOWHUB_OPENCODE_PASSWORD"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("OSCRUB_OPENCODE_PASSWORD", "from-the-config")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "opencode" || password != "from-the-config" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"healthy":true,"version":"1.18.33"}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// The runtime is declared under its own name pointing at FLOWHUB_OPENCODE_URL,
	// which is the shape that was refused live: `default` is not the name the
	// credentials live under, so probing as `default` sent no header at all.
	body := `{"version": 2,
	  "runtimes": {"local": {"url": "` + server.URL + `", "auth": {"user": "opencode", "password_env": "OSCRUB_OPENCODE_PASSWORD"}}},
	  "projects": [{"source": "youtrack", "project": "TEST", "repo": {"path": "/tmp/test"}, "runtime": "local"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(path)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}

	cfg := config.Config{OpenCodeURL: server.URL, TaskDeadline: time.Minute}
	runtime := buildDefaultRuntime(cfg, projects, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := runtime.Health(context.Background()); err != nil {
		t.Fatalf("the fallback runtime did not authenticate with the runtime's own credentials: %v", err)
	}

	// The control: without the credential the same probe must fail, so the assertion
	// above is proving that the header was sent and accepted, not that the server is
	// permissive.
	noCredentials := buildDefaultRuntime(cfg, emptyProjects(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := noCredentials.Health(context.Background()); err == nil {
		t.Fatal("a runtime without credentials was accepted by a server that requires them")
	}
}

// TestShellWrappersResolvePerRuntime pins the wrapper precedence: the environment is
// the outermost default and a `runtimes.<name>.shell_wrappers` block overrides it, the
// same shape runtimeDeadline uses. A laptop that rewrites commands and a server that
// does not must be describable by one configuration file.
func TestShellWrappersResolvePerRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"version": 2,
	  "runtimes": {
	    "builder-a": {"url": "http://10.0.0.1:4096", "shell_wrappers": ["my-tool"]},
	    "local": {"url": "http://127.0.0.1:4096"}
	  },
	  "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "/tmp/x"}, "runtime": "local"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(path)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}
	cfg := config.Config{ShellWrappers: []string{"rtk"}}

	if got := strings.Join(shellWrappers(cfg, projects, "builder-a"), ","); got != "my-tool" {
		t.Fatalf("builder-a wrappers = %q, want the block's my-tool", got)
	}
	// A block that declares none falls back to the process-wide default, and so does a
	// runtime name no block mentions — the single-host case with no configuration file.
	for _, name := range []string{"local", "default", "never-declared"} {
		if got := strings.Join(shellWrappers(cfg, projects, name), ","); got != "rtk" {
			t.Fatalf("%s wrappers = %q, want the environment default rtk", name, got)
		}
	}
	if got := shellWrappers(config.Config{}, projects, "default"); len(got) != 0 {
		t.Fatalf("with nothing configured the list is %v, want empty (nothing is stripped)", got)
	}
}

// TestShellWrapperProblemsRefuseNamesThatCannotMatch pins the gate: a wrapper is
// matched against a command's first word, so an entry that is not a command name would
// silently do nothing. Both levels are checked, because the mistake is the same
// whether it was written in the environment or in a runtime block.
func TestShellWrapperProblemsRefuseNamesThatCannotMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"version": 2,
	  "runtimes": {"builder-a": {"url": "http://10.0.0.1:4096", "shell_wrappers": ["rtk git"]}},
	  "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "/tmp/x"}, "runtime": "builder-a"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(path)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}

	problems := shellWrapperProblems(config.Config{ShellWrappers: []string{"rtk", "/usr/bin/rtk"}}, projects)
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want one for the environment and one for the block", problems)
	}
	if !strings.Contains(problems[0], "FLOWHUB_SHELL_WRAPPERS") || !strings.Contains(problems[0], `"/usr/bin/rtk"`) {
		t.Fatalf("the environment problem does not name the variable and the entry: %s", problems[0])
	}
	if !strings.Contains(problems[1], "runtimes.builder-a.shell_wrappers") || !strings.Contains(problems[1], `"rtk git"`) {
		t.Fatalf("the block problem does not name the field and the entry: %s", problems[1])
	}

	good := config.Config{ShellWrappers: []string{"rtk", "my-tool"}}
	if problems := shellWrapperProblems(good, projects); len(problems) != 1 {
		t.Fatalf("problems = %v, want only the block's", problems)
	}
	clean := projectmap.Map{}
	if problems := shellWrapperProblems(good, &clean); len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
}

// TestTheRuntimeFactoryCarriesTheConfiguredShellWrappers is the wiring test for the
// whole path: the prefix is written into the configuration file, the factory builds the
// runtime the dispatcher will use, and that runtime's arbiter has to answer a
// rewritten permission request with "once". A ShellWrappers field that never reaches
// the arbiter is exactly the silently inert setting this project refuses — the same
// class of defect as a `prompt_file` that parses and then does nothing.
func TestTheRuntimeFactoryCarriesTheConfiguredShellWrappers(t *testing.T) {
	type replied struct {
		body string
	}
	var mu sync.Mutex
	var seen []replied
	answered := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_, _ = w.Write([]byte(`{"id":"ses_1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses_1/prompt_async":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses_1/message":
			if !answered {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			// One completed assistant message, which is the only reliable marker
			// that the turn really ended.
			_, _ = w.Write([]byte(`[{"info":{"id":"msg_1","role":"assistant","finish":"stop",
			  "cost":0.001,"tokens":{"total":10,"input":8,"output":2},
			  "time":{"created":1,"completed":2}},"parts":[{"type":"text","text":"the tree is clean"}]}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/session/status":
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/permission":
			if answered {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			// The measured request: the model wrote `git status`, a host plugin
			// rewrote it before the permission was raised.
			_, _ = w.Write([]byte(`[{"id":"per_1","sessionID":"ses_1","permission":"bash",
			  "patterns":["rtk git status"],"metadata":{"command":"rtk git status"}}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/permission/per_1/reply":
			raw, _ := io.ReadAll(r.Body)
			seen = append(seen, replied{body: string(raw)})
			answered = true
			_, _ = w.Write([]byte(`true`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"version": 2,
	  "runtimes": {"local": {"url": "` + server.URL + `", "shell_wrappers": ["rtk"],
	                         "auth": {"user": "opencode", "password": "secret"}}},
	  "projects": [{"source": "youtrack", "project": "T", "repo": {"path": "/tmp/x"}, "runtime": "local"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	projects, err := projectmap.Load(path)
	if err != nil {
		t.Fatalf("projectmap.Load: %v", err)
	}

	cfg := config.Config{
		OpenCodeURL: server.URL, TaskDeadline: time.Minute, FirstResponse: 5 * time.Second,
		ShellWrappers: []string{"never-used-the-block-wins"},
	}
	runtime := newRuntimeFactory(cfg, projects, slog.New(slog.NewTextHandler(io.Discard, nil)))("local", server.URL)
	if _, err := runtime.Run(context.Background(), agent.Turn{
		Directory: "/tmp/x",
		Prompt:    "check the git status",
		Agent:     "devops",
		Title:     "BEAP_BE-46",
		Phase:     agent.PhaseAnalysis,
		Deadline:  time.Minute,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("%d permission replies reached the server, want 1", len(seen))
	}
	if !strings.Contains(seen[0].body, `"reply":"once"`) {
		t.Fatalf("the rewritten command was answered %s, want once — the configured wrapper did not reach the arbiter", seen[0].body)
	}
}
