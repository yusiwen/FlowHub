package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/runtimes"
)

// fakeRunner is a host described by maps, so the checks can be tested without
// executing anything or touching the real filesystem.
type fakeRunner struct {
	paths    map[string]string
	dirs     map[string]bool
	files    map[string]bool
	outputs  map[string]string
	failures map[string]bool
	statErrs map[string]error
	env      map[string]string
	identity Identity
	tempDir  string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		paths:    map[string]string{},
		dirs:     map[string]bool{},
		files:    map[string]bool{},
		statErrs: map[string]error{},
		outputs:  map[string]string{},
		failures: map[string]bool{},
		env:      map[string]string{},
		identity: Identity{User: "yusiwen", UID: "501", Home: "/home/yusiwen"},
		tempDir:  "/tmp",
	}
}

func (f *fakeRunner) key(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

func (f *fakeRunner) Lookup(name string) (string, bool) {
	path, ok := f.paths[name]
	return path, ok
}

func (f *fakeRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	key := f.key(name, args...)
	output := f.outputs[key]
	if f.failures[key] {
		return output, fmt.Errorf("exit status 1")
	}
	if output == "" {
		return "", fmt.Errorf("no such command: %s", key)
	}
	return output, nil
}

func (f *fakeRunner) Getenv(key string) (string, bool) {
	value, ok := f.env[key]
	return value, ok
}

func (f *fakeRunner) Stat(path string) (bool, bool, error) {
	if err, ok := f.statErrs[path]; ok {
		return false, false, err
	}
	if f.dirs[path] {
		return true, true, nil
	}
	if f.files[path] {
		return true, false, nil
	}
	return false, false, nil
}

func (f *fakeRunner) TempDir() string { return f.tempDir }

func (f *fakeRunner) Identity() Identity { return f.identity }

func (f *fakeRunner) Hostname() string { return "builder-a" }

func (f *fakeRunner) GOOS() string { return "linux" }

func (f *fakeRunner) GOARCH() string { return "arm64" }

// healthyHost is a host that satisfies everything a Gitea project needs.
func healthyHost() *fakeRunner {
	f := newFakeRunner()
	f.paths = map[string]string{
		"opencode": "/usr/local/bin/opencode",
		"git":      "/usr/bin/git",
		"tea":      "/usr/local/bin/tea",
	}
	f.dirs = map[string]bool{
		"/home/yusiwen/.config/opencode": true,
	}
	f.files = map[string]bool{
		"/home/yusiwen/.config/tea/config.yml": true,
	}
	f.outputs = map[string]string{
		f.key("opencode", "--version"):                                                     "1.18.31",
		f.key("git", "--version"):                                                          "git version 2.43.0",
		f.key("tea", "--version"):                                                          "tea version 0.9.1",
		f.key("tea", "login", "list"):                                                      "git.yusiwen.cn  yusiwen",
		f.key("git", "rev-parse", "--is-inside-work-tree"):                                 "true",
		f.key("git", "remote", "get-url", "origin"):                                        "git@git.yusiwen.cn:Pipechina-CJPT/beap-be.git",
		f.key("git", "ls-remote", "--exit-code", "origin", "HEAD"):                         "abc123\tHEAD",
		f.key("git", "push", "--dry-run", "origin", "HEAD:refs/heads/flowhub/probe-check"): "Everything up-to-date",
	}
	f.env["YOUTRACK_TOKEN"] = "secret-value-that-must-not-appear"
	return f
}

func giteaOptions() Options {
	return Options{
		Forges:      map[string]string{"git.yusiwen.cn": ForgeGitea},
		Repos:       []RepoExpectation{{Remote: "git@git.yusiwen.cn:Pipechina-CJPT/beap-be.git", Clone: "/srv/repos/beap-be"}},
		RequiredEnv: []string{"YOUTRACK_TOKEN"},
	}
}

func TestCheckPassesOnAHealthyHost(t *testing.T) {
	f := healthyHost()
	f.dirs["/srv/repos/beap-be"] = true

	report, err := Check(context.Background(), f, giteaOptions())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.OK {
		t.Fatalf("OK = false; failures = %v", report.Failures)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %v", report.Failures)
	}
	if report.Agent == nil || report.Agent.Version != "1.18.31" || !report.Agent.ConfigDirExists {
		t.Fatalf("agent = %+v", report.Agent)
	}
	if report.Invoker.User != "yusiwen" || report.Invoker.Home != "/home/yusiwen" {
		t.Fatalf("invoker = %+v", report.Invoker)
	}
	if report.Agent.ConfigDir != "~/.config/opencode" {
		t.Fatalf("config dir = %q, want it shortened to ~", report.Agent.ConfigDir)
	}
	forge := report.Forges["git.yusiwen.cn"]
	if forge.Kind != ForgeGitea || !forge.ToolReady || forge.Authenticated == nil || !*forge.Authenticated {
		t.Fatalf("forge = %+v", forge)
	}
	repo := report.Repos[0]
	if !repo.CloneExists || !repo.IsWorkTree || !repo.OriginMatches || !repo.LSRemote || repo.PushDryRun != "ok" {
		t.Fatalf("repo = %+v", repo)
	}
	if !report.EnvPresent["YOUTRACK_TOKEN"] {
		t.Fatal("YOUTRACK_TOKEN was not recorded as present")
	}
}

// TestReportNeverCarriesASecretValue is the property that makes the report safe
// to copy into a ticket: it may say a variable is present, never what it holds.
func TestReportNeverCarriesASecretValue(t *testing.T) {
	f := healthyHost()
	f.dirs["/srv/repos/beap-be"] = true

	report, err := Check(context.Background(), f, giteaOptions())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	encoded, err := report.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if bytes.Contains(encoded, []byte("secret-value-that-must-not-appear")) {
		t.Fatal("the environment value leaked into the report")
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
}

// TestConfigDirIsCheckedEvenWhenTheBinaryIsMissing is a regression test for a
// report that lied: an early return on "opencode is not on PATH" left
// config_dir_exists at its zero value, so a directory that exists was printed as
// "does not exist". A zero-valued bool is indistinguishable from a checked-and-
// absent one, which is why the stat now always runs.
func TestConfigDirIsCheckedEvenWhenTheBinaryIsMissing(t *testing.T) {
	f := healthyHost()
	delete(f.paths, "opencode")

	report, err := Check(context.Background(), f, Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Agent == nil {
		t.Fatal("no agent section in the report")
	}
	if !report.Agent.ConfigDirExists {
		t.Fatalf("the configuration directory was reported as missing while it exists: %+v", report.Agent)
	}
	if report.Agent.ConfigDirError != "" {
		t.Fatalf("unexpected stat error: %s", report.Agent.ConfigDirError)
	}
}

// TestConfigRootIsWhatTheReportNames is a regression test for a report that
// described the wrong host: `init --check --config-root /srv/agent` read the
// profile under /srv/agent while the report still printed the default
// ~/.config/opencode, so the operator was sent to inspect a file this host does
// not run.
func TestConfigRootIsWhatTheReportNames(t *testing.T) {
	f := healthyHost()
	f.dirs["/srv/agent"] = true

	report, err := Check(context.Background(), f, Options{ConfigRoot: "/srv/agent"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Agent == nil || report.Agent.ConfigDir != "/srv/agent" {
		t.Fatalf("config dir = %+v, want the directory the check read", report.Agent)
	}
	if !report.Agent.ConfigDirExists {
		t.Fatal("the overridden configuration directory exists but was reported as missing")
	}

	// And a check that could not see the overridden root reports that root, not a
	// reassuring default.
	f2 := healthyHost()
	f2.statErrs["/srv/agent"] = fmt.Errorf("permission denied")
	report2, err := Check(context.Background(), f2, Options{ConfigRoot: "/srv/agent"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report2.Agent.ConfigDir != "/srv/agent" || report2.Agent.ConfigDirError == "" {
		t.Fatalf("agent = %+v, want the unreadable override named", report2.Agent)
	}
}

// TestUnreadablePathIsNotReportedAsMissing covers the other half of the same
// mistake: a directory the check cannot look at must say so, not "does not exist".
func TestUnreadablePathIsNotReportedAsMissing(t *testing.T) {
	f := healthyHost()
	f.statErrs["/home/yusiwen/.config/opencode"] = fmt.Errorf("permission denied")

	report, err := Check(context.Background(), f, Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Agent.ConfigDirExists {
		t.Fatal("a path that could not be inspected was reported as present")
	}
	if report.Agent.ConfigDirError == "" {
		t.Fatal("the inspection error was dropped")
	}
	if !containsSubstring(report.Warnings, "could not be inspected") {
		t.Fatalf("warnings = %v", report.Warnings)
	}
}

func TestMissingAgentIsAFailureWithGuidance(t *testing.T) {
	f := healthyHost()
	delete(f.paths, "opencode")

	report, err := Check(context.Background(), f, Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.OK {
		t.Fatal("OK = true with no agent runtime")
	}
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0], "install the agent runtime") {
		t.Fatalf("failures = %v", report.Failures)
	}
}

func TestMissingForgeToolIsAFailureButOnlyForItsHost(t *testing.T) {
	f := healthyHost()
	delete(f.paths, "tea")
	f.dirs["/srv/repos/beap-be"] = true

	report, err := Check(context.Background(), f, giteaOptions())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.OK {
		t.Fatal("OK = true without the tool the declared forge needs")
	}
	if !containsSubstring(report.Failures, "needs the tea command-line tool") {
		t.Fatalf("failures = %v", report.Failures)
	}
	// A project that needs no forge tooling is unaffected by the same host.
	plain, err := Check(context.Background(), f, Options{
		Forges: map[string]string{"git.internal": ForgeNone},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !plain.OK {
		t.Fatalf("a forge=none host should not need gh or tea; failures = %v", plain.Failures)
	}
}

func TestUnauthenticatedForgeToolIsAFailure(t *testing.T) {
	f := healthyHost()
	f.failures[f.key("tea", "login", "list")] = true
	f.dirs["/srv/repos/beap-be"] = true

	report, err := Check(context.Background(), f, giteaOptions())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.OK {
		t.Fatal("OK = true while the forge tool is unauthenticated")
	}
	if !containsSubstring(report.Failures, "is not authenticated") {
		t.Fatalf("failures = %v", report.Failures)
	}
}

// TestReadOnlyCloneIsAFailure covers the case the workflow actually depends on:
// everything looks healthy except the push, which is the one thing that has to
// work.
func TestReadOnlyCloneIsAFailure(t *testing.T) {
	f := healthyHost()
	f.dirs["/srv/repos/beap-be"] = true
	f.failures[f.key("git", "push", "--dry-run", "origin", "HEAD:refs/heads/flowhub/probe-check")] = true
	f.outputs[f.key("git", "push", "--dry-run", "origin", "HEAD:refs/heads/flowhub/probe-check")] =
		"remote: Repository not found.\nfatal: repository 'x' not found"

	report, err := Check(context.Background(), f, giteaOptions())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.OK {
		t.Fatal("OK = true with a read-only clone")
	}
	if report.Repos[0].PushDryRun == "ok" || !strings.HasPrefix(report.Repos[0].PushDryRun, "failed:") {
		t.Fatalf("push dry run = %q", report.Repos[0].PushDryRun)
	}
	if !containsSubstring(report.Failures, "write access") {
		t.Fatalf("failures = %v", report.Failures)
	}
}

func TestOriginMismatchIsAFailure(t *testing.T) {
	f := healthyHost()
	f.dirs["/srv/repos/beap-be"] = true
	f.outputs[f.key("git", "remote", "get-url", "origin")] = "git@git.yusiwen.cn:someone/other.git"

	report, err := Check(context.Background(), f, giteaOptions())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.OK || report.Repos[0].OriginMatches {
		t.Fatalf("origin mismatch was accepted: %+v", report.Repos[0])
	}
}

// TestSameRemoteIgnoresSpelling keeps the origin comparison from failing on a
// trailing ".git" or a case difference in the host.
func TestSameRemoteIgnoresSpelling(t *testing.T) {
	pairs := [][2]string{
		{"git@git.yusiwen.cn:o/r.git", "git@git.yusiwen.cn:o/r"},
		{"git@Git.Yusiwen.CN:o/r.git", "git@git.yusiwen.cn:o/r.git"},
	}
	for _, pair := range pairs {
		if !sameRemote(pair[0], pair[1]) {
			t.Errorf("sameRemote(%q, %q) = false", pair[0], pair[1])
		}
	}
	if sameRemote("git@git.yusiwen.cn:o/r.git", "git@git.yusiwen.cn:o/other.git") {
		t.Error("different repositories compared equal")
	}
}

func TestMissingRequiredEnvIsAWarningAndStrictMakesItFail(t *testing.T) {
	f := healthyHost()
	delete(f.env, "YOUTRACK_TOKEN")

	options := giteaOptions()
	f.dirs["/srv/repos/beap-be"] = true

	report, err := Check(context.Background(), f, options)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.OK {
		t.Fatalf("a missing credential file is a warning, not a failure: %v", report.Failures)
	}
	if !containsSubstring(report.Warnings, "YOUTRACK_TOKEN is not set") {
		t.Fatalf("warnings = %v", report.Warnings)
	}

	options.Strict = true
	strict, err := Check(context.Background(), f, options)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if strict.OK {
		t.Fatal("--strict did not turn the warning into a failure")
	}
}

func TestUnknownForgeKindIsRejected(t *testing.T) {
	_, err := Check(context.Background(), newFakeRunner(), Options{
		Forges: map[string]string{"git.example.com": "gitlab"},
	})
	if err == nil || !strings.Contains(err.Error(), "want github, gitea or none") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownAgentIsRejected(t *testing.T) {
	_, err := Check(context.Background(), newFakeRunner(), Options{Agent: "claude-code"})
	if err == nil || !strings.Contains(err.Error(), "this build knows") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoCloneSkipsTheWriteCheckWithAWarning(t *testing.T) {
	f := healthyHost()
	f.outputs[f.key("git", "ls-remote", "--exit-code", "git@git.yusiwen.cn:o/r.git", "HEAD")] = "abc\tHEAD"

	report, err := Check(context.Background(), f, Options{
		Forges: map[string]string{"git.yusiwen.cn": ForgeGitea},
		Repos:  []RepoExpectation{{Remote: "git@git.yusiwen.cn:o/r.git"}},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.OK {
		t.Fatalf("failures = %v", report.Failures)
	}
	if !strings.HasPrefix(report.Repos[0].PushDryRun, "skipped:") {
		t.Fatalf("push dry run = %q", report.Repos[0].PushDryRun)
	}
	if !containsSubstring(report.Warnings, "write access was not checked") {
		t.Fatalf("warnings = %v", report.Warnings)
	}
}

// TestForgeIsDerivedFromTheRepository keeps the tool requirement honest: a host
// that serves a GitHub repository needs gh without being told, and a host that
// serves a self-hosted Gitea repository must not be forced to install it.
func TestForgeIsDerivedFromTheRepository(t *testing.T) {
	// healthyHost has opencode, git and tea but no gh, which is the host this
	// test is about: a Gitea-only machine that should never be asked for gh.
	f := healthyHost()

	// A Gitea repository with the forge declared: gh is not required at all.
	report, err := Check(context.Background(), f, Options{
		Forges: map[string]string{"git.yusiwen.cn": ForgeGitea},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.OK {
		t.Fatalf("a Gitea-only host was rejected: %v", report.Failures)
	}
	if _, present := report.Tools["gh"]; present {
		t.Fatal("the GitHub CLI was required by a Gitea-only host")
	}

	// A GitHub repository implies the GitHub forge without a flag.
	derived, err := effectiveForges(Options{Repos: []RepoExpectation{{Remote: "git@github.com:o/r.git"}}})
	if err != nil {
		t.Fatalf("effectiveForges: %v", err)
	}
	if derived["github.com"] != ForgeGitHub {
		t.Fatalf("derived = %v", derived)
	}

	// An unknown host is refused rather than guessed at.
	if _, err := effectiveForges(Options{
		Repos: []RepoExpectation{{Remote: "git@git.example.com:o/r.git"}},
	}); err == nil || !strings.Contains(err.Error(), "--forge") {
		t.Fatalf("an undeclared host was accepted: %v", err)
	}

	// forge=none is how an operator says "no pull requests on this host".
	quiet, err := effectiveForges(Options{
		Forges: map[string]string{"github.com": ForgeNone},
		Repos:  []RepoExpectation{{Remote: "git@github.com:o/r.git"}},
	})
	if err != nil {
		t.Fatalf("effectiveForges: %v", err)
	}
	if quiet["github.com"] != ForgeNone {
		t.Fatalf("declared forge was overridden: %v", quiet)
	}
}

func TestRemoteHostSpellings(t *testing.T) {
	cases := map[string]string{
		"git@git.yusiwen.cn:Pipechina-CJPT/beap-be.git": "git.yusiwen.cn",
		"ssh://git@git.yusiwen.cn:2222/o/r.git":         "git.yusiwen.cn",
		"https://github.com/o/r.git":                    "github.com",
		"git://git.example.com/o/r.git":                 "git.example.com",
		"/local/path/repo":                              "",
		"":                                              "",
	}
	for remote, want := range cases {
		if got := remoteHost(remote); got != want {
			t.Errorf("remoteHost(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestParseInitSplitsRemoteFromClone(t *testing.T) {
	parsed, err := parseInit([]string{
		"--check",
		"--repo", "git@git.yusiwen.cn:o/r.git=/srv/repos/r",
		"--forge", "git.yusiwen.cn=gitea",
		"--require-env", "YOUTRACK_TOKEN",
		"--strict",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseInit: %v", err)
	}
	if parsed.Repos[0].Clone != "/srv/repos/r" {
		t.Fatalf("repos = %+v", parsed.Repos)
	}
	if parsed.Forges["git.yusiwen.cn"] != ForgeGitea || !parsed.Strict || len(parsed.RequiredEnv) != 1 {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestParseInitRejectsMalformedFlags(t *testing.T) {
	cases := [][]string{
		{"--check", "--forge", "git.example.com"},
		{"--check", "--repo", ""},
		{"--check", "--timeout", "0s"},
		{"--check", "extra"},
	}
	for _, args := range cases {
		if _, err := parseInit(args, &bytes.Buffer{}); err == nil {
			t.Errorf("parseInit(%v) accepted malformed input", args)
		}
	}
}

// TestParseDoctorSharesTheCheckFlags: doctor and `init --check` have to describe
// the same machine, so they parse the same flags. A flag only one of them honoured
// would make one report differ from the other for no reason an operator can see.
func TestParseDoctorSharesTheCheckFlags(t *testing.T) {
	parsed, err := parseDoctor([]string{
		"--push",
		"--agent", "opencode",
		"--config-root", "/srv/agent",
		"--opencode-url", "http://127.0.0.1:4096/",
		"--repo", "git@git.yusiwen.cn:o/r.git=/srv/repos/r",
		"--forge", "git.yusiwen.cn=gitea",
		"--require-env", "YOUTRACK_TOKEN",
		"--json",
		"--strict",
		"--server", "http://gateway.lan:8081/",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseDoctor: %v", err)
	}
	if !parsed.push || !parsed.json || !parsed.Strict {
		t.Fatalf("parsed = %+v", parsed)
	}
	if parsed.Agent != "opencode" || parsed.configRoot != "/srv/agent" {
		t.Fatalf("parsed = %+v", parsed)
	}
	if parsed.openCodeURL != "http://127.0.0.1:4096" || parsed.server != "http://gateway.lan:8081" {
		t.Fatalf("the trailing slashes were not trimmed: %q %q", parsed.openCodeURL, parsed.server)
	}
	if parsed.Forges["git.yusiwen.cn"] != ForgeGitea || len(parsed.Repos) != 1 || len(parsed.RequiredEnv) != 1 {
		t.Fatalf("parsed = %+v", parsed)
	}

	for _, args := range [][]string{{"--timeout", "0s"}, {"--forge", "git.example.com"}, {"extra"}} {
		if _, err := parseDoctor(args, &bytes.Buffer{}); err == nil {
			t.Errorf("parseDoctor(%v) accepted malformed input", args)
		}
	}
}

// TestPushUsesTheRuntimeSecretAndCarriesTheModels is the data-plane half of
// `doctor --push`: the report and the models travel under the runtime secret, so a
// worker needs no admin token to keep the control plane's view of it current.
func TestPushUsesTheRuntimeSecretAndCarriesTheModels(t *testing.T) {
	var (
		auth string
		body map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"runtime":{"name":"builder-a","state":"active","models":{"devops":"deepseek/deepseek-flash"}}}`))
	}))
	defer server.Close()

	report := &Report{Profiles: []ProfileCheck{{Agent: "devops", Model: "deepseek/deepseek-flash", Source: "installed"}}}
	client := runtimes.NewClient(server.URL, "")
	runtime, err := Push(context.Background(), client, RuntimeIdentity{Name: "builder-a", Server: server.URL, Secret: "s3cret"}, report)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if auth != "Bearer s3cret" {
		t.Fatalf("authorization = %q, want the runtime secret", auth)
	}
	if body["report"] == nil {
		t.Fatal("the capability report was not sent")
	}
	models, _ := body["models"].(map[string]any)
	if models["devops"] != "deepseek/deepseek-flash" {
		t.Fatalf("the pinned model was not sent: %v", body["models"])
	}
	if runtime.Name != "builder-a" || runtime.State != "active" {
		t.Fatalf("runtime = %+v", runtime)
	}

	if _, err := Push(context.Background(), client, RuntimeIdentity{Name: "builder-a"}, report); err == nil {
		t.Fatal("a push without a secret was accepted")
	}
	if _, err := Push(context.Background(), client, RuntimeIdentity{Secret: "s3cret"}, report); err == nil {
		t.Fatal("a push without a name was accepted")
	}
}

func TestCommandExitStatuses(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main("nonsense", nil, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("unknown command exit = %d", code)
	}
	// No unit test runs `runtime init` without --config-root: on a machine that
	// has an agent runtime installed it would write into the operator's real
	// configuration directory. The install path is covered by the tests that pass
	// a throwaway root.
	stdout.Reset()
	stderr.Reset()
	if code := runtimeCommand([]string{"reconfigure"}, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("unimplemented subcommand exit = %d", code)
	}
	if code := runtimeCommand(nil, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("no subcommand exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "not implemented in this build") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestTextReportIsStableAndReadable(t *testing.T) {
	f := healthyHost()
	f.dirs["/srv/repos/beap-be"] = true

	report, err := Check(context.Background(), f, giteaOptions())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	var first, second bytes.Buffer
	report.Text(&first)
	report.Text(&second)
	if first.String() != second.String() {
		t.Fatal("the text report is not deterministic")
	}
	// The report describes the host only. Whether a run wrote anything is the
	// caller's statement, so it must not appear here (it once did, and told an
	// installing run that it had written nothing).
	for _, want := range []string{"builder-a", "yusiwen", "opencode", "tea", "git.yusiwen.cn", "push", "result: OK"} {
		if !strings.Contains(first.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, first.String())
		}
	}
	for _, unwanted := range []string{"nothing was written", "not implemented"} {
		if strings.Contains(first.String(), unwanted) {
			t.Errorf("the report claims something about the caller's run: %q", unwanted)
		}
	}
}

// fakeCatalogue is a server's model list, or an error when it cannot be read.
type fakeCatalogue struct {
	models map[string][]string
	err    error
}

func (f fakeCatalogue) Catalogue(context.Context) (map[string][]string, error) {
	return f.models, f.err
}

// TestProfileModelMustExistOnTheServer is the check that turns a provider quietly
// dropping a model id into an enrolment failure. It cost a real task to discover
// when it was missing.
func TestProfileModelMustExistOnTheServer(t *testing.T) {
	profiles, err := agentProfiles(AgentOpenCode, "")
	if err != nil {
		t.Fatalf("agentProfiles: %v", err)
	}
	if len(profiles) == 0 {
		t.Fatal("the embedded agent asset pins no model")
	}
	pinned := profiles[0].Model
	provider, model, ok := splitModel(pinned)
	if !ok {
		t.Fatalf("the embedded asset pins %q, which is not provider/model", pinned)
	}

	// Available: no failure, and the report says so.
	f := healthyHost()
	available, err := Check(context.Background(), f, Options{
		Catalogue: fakeCatalogue{models: map[string][]string{provider: {model, "another"}}},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(available.Profiles) != 1 || !available.Profiles[0].Available || !available.Profiles[0].Checked {
		t.Fatalf("profiles = %+v", available.Profiles)
	}
	if containsSubstring(available.Failures, "does not offer") {
		t.Fatalf("an available model was reported as missing: %v", available.Failures)
	}

	// Missing: a failure that names the model and what is on offer.
	missing, err := Check(context.Background(), f, Options{
		Catalogue: fakeCatalogue{models: map[string][]string{provider: {"something-else"}}},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if missing.OK {
		t.Fatal("a missing model was accepted")
	}
	if !containsSubstring(missing.Failures, pinned) || !containsSubstring(missing.Failures, "does not offer") {
		t.Fatalf("failures = %v", missing.Failures)
	}

	// Unreachable server: a warning, never a failure. Not running yet is a
	// different problem with a different message.
	unreachable, err := Check(context.Background(), f, Options{
		Catalogue: fakeCatalogue{err: fmt.Errorf("connection refused")},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !unreachable.OK {
		t.Fatalf("an unreachable server failed the check: %v", unreachable.Failures)
	}
	if len(unreachable.Profiles) != 1 || unreachable.Profiles[0].Checked {
		t.Fatalf("profiles = %+v", unreachable.Profiles)
	}
	if !containsSubstring(unreachable.Warnings, "could not be verified") {
		t.Fatalf("warnings = %v", unreachable.Warnings)
	}

	// No catalogue injected: reported as not checked, not as missing.
	skipped, err := Check(context.Background(), f, Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if skipped.OK != true {
		t.Fatal("a skipped model check failed the host")
	}
	if len(skipped.Profiles) != 1 || skipped.Profiles[0].Checked {
		t.Fatalf("profiles = %+v", skipped.Profiles)
	}
}

func TestFrontmatterValueReadsOnlyTheHeader(t *testing.T) {
	content := "---\ndescription: x\nmodel: deepseek/deepseek-flash\nsteps: 40\n---\n\nmodel: not-this-one\n"
	if got := frontmatterValue(content, "model"); got != "deepseek/deepseek-flash" {
		t.Fatalf("frontmatterValue = %q", got)
	}
	if got := frontmatterValue("model: outside\n", "model"); got != "" {
		t.Fatalf("a file without frontmatter returned %q", got)
	}
	if got := frontmatterValue("---\nmodel: \"quoted/id\"\n---\n", "model"); got != "quoted/id" {
		t.Fatalf("quotes were not stripped: %q", got)
	}
}

func containsSubstring(items []string, want string) bool {
	for _, item := range items {
		if strings.Contains(item, want) {
			return true
		}
	}
	return false
}
