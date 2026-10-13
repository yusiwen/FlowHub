package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseSourcesAcceptsIPsAndCIDRs(t *testing.T) {
	networks, err := ParseSources(" 10.8.0.5 , 192.168.0.0/16,fd00::/8 ")
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if len(networks) != 3 {
		t.Fatalf("got %d networks, want 3", len(networks))
	}
	if networks[0].String() != "10.8.0.5/32" {
		t.Fatalf("bare IP became %s, want a /32", networks[0])
	}
	if networks[1].String() != "192.168.0.0/16" {
		t.Fatalf("cidr became %s", networks[1])
	}
}

func TestParseSourcesRejectsGarbage(t *testing.T) {
	if _, err := ParseSources("not-an-ip"); err == nil {
		t.Fatal("want an error for a non-IP entry")
	}
	if _, err := ParseSources("10.0.0.0/33"); err == nil {
		t.Fatal("want an error for an invalid mask")
	}
}

func TestParseSourcesEmptyMeansDisabled(t *testing.T) {
	networks, err := ParseSources("  ")
	if err != nil {
		t.Fatalf("ParseSources: %v", err)
	}
	if networks != nil {
		t.Fatalf("got %v, want nil", networks)
	}
}

func TestLoadUsesSafeDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != DefaultAddr {
		t.Fatalf("addr = %q", cfg.Addr)
	}
	if cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Fatalf("max body = %d", cfg.MaxBodyBytes)
	}
	if cfg.ReplayWindow != DefaultReplayWindow {
		t.Fatalf("replay window = %s", cfg.ReplayWindow)
	}
	if cfg.TokenHeader != DefaultTokenHeader {
		t.Fatalf("token header = %q", cfg.TokenHeader)
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("FLOWHUB_ADDR", "10.8.0.5:9000")
	t.Setenv("FLOWHUB_HOOK_KEY", "abc")
	t.Setenv("FLOWHUB_TOKEN", "def")
	t.Setenv("FLOWHUB_ALLOWED_SOURCES", "10.8.0.0/24")
	t.Setenv("FLOWHUB_REPLAY_WINDOW", "0")
	t.Setenv("FLOWHUB_LOG_HEADERS", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "10.8.0.5:9000" || cfg.HookKey != "abc" || cfg.Token != "def" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if len(cfg.AllowedSources) != 1 {
		t.Fatalf("allowed sources = %v", cfg.AllowedSources)
	}
	if cfg.ReplayWindow != 0 {
		t.Fatalf("replay window = %s, want 0", cfg.ReplayWindow)
	}
	if !cfg.LogHeaders {
		t.Fatal("log headers = false")
	}
	if got := strings.Join(cfg.ActiveLocks(), ","); got != "url_key,header_token,source_ip" {
		t.Fatalf("active locks = %q", got)
	}
}

// TestLoadReadsShellWrappers pins the environment level of the wrapper list. The
// default is empty on purpose: nothing is stripped from a command unless the
// operator says which prefix is trusted (internal/agent/opencode/wrapper.go).
func TestLoadReadsShellWrappers(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ShellWrappers) != 0 {
		t.Fatalf("shell wrappers default to %v, want none", cfg.ShellWrappers)
	}

	t.Setenv("FLOWHUB_SHELL_WRAPPERS", "rtk, my-tool ")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(cfg.ShellWrappers, ","); got != "rtk,my-tool" {
		t.Fatalf("shell wrappers = %q, want %q", got, "rtk,my-tool")
	}
	if !strings.Contains(cfg.Report(), "shell_wrappers:     rtk,my-tool") {
		t.Fatalf("the report does not name the wrappers:\n%s", cfg.Report())
	}
}

// TestOneRequestIsBoundedFarBelowTheTurn pins the setting that exists because the two
// bounds were confused: the turn deadline once doubled as the per-request timeout, so a
// host that went silent without closing the connection held a worker for the whole turn
// (issue #33).
func TestOneRequestIsBoundedFarBelowTheTurn(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenCodeRequestTimeout != DefaultOpenCodeRequestTimeout {
		t.Fatalf("request timeout = %s, want the %s default", cfg.OpenCodeRequestTimeout, DefaultOpenCodeRequestTimeout)
	}
	if DefaultOpenCodeRequestTimeout >= DefaultTaskDeadline {
		t.Fatalf("the default request bound (%s) is not below the default turn budget (%s)",
			DefaultOpenCodeRequestTimeout, DefaultTaskDeadline)
	}

	t.Setenv("FLOWHUB_OPENCODE_REQUEST_TIMEOUT", "5s")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenCodeRequestTimeout != 5*time.Second {
		t.Fatalf("request timeout = %s, want 5s", cfg.OpenCodeRequestTimeout)
	}
	if !strings.Contains(cfg.Report(), "opencode_request_timeout: 5s") {
		t.Fatalf("the report does not name the request bound:\n%s", cfg.Report())
	}

	// Zero is refused rather than read as "no bound", which is what net/http would do.
	t.Setenv("FLOWHUB_OPENCODE_REQUEST_TIMEOUT", "0")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "FLOWHUB_OPENCODE_REQUEST_TIMEOUT") {
		t.Fatalf("Load with a zero request timeout = %v, want a refusal naming the variable", err)
	}

	// At or above the turn budget is the shape of the bug, so it is warned about — the
	// same treatment FLOWHUB_FIRST_RESPONSE gets, because the two are the same mistake.
	t.Setenv("FLOWHUB_OPENCODE_REQUEST_TIMEOUT", "30s")
	t.Setenv("FLOWHUB_TASK_DEADLINE", "30s")
	t.Setenv("FLOWHUB_DISPATCH", "1")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var warned bool
	for _, warning := range cfg.Warnings() {
		if strings.Contains(warning, "FLOWHUB_OPENCODE_REQUEST_TIMEOUT") && strings.Contains(warning, "FLOWHUB_TASK_DEADLINE") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("a request bound equal to the turn budget produced no warning: %v", cfg.Warnings())
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	t.Setenv("FLOWHUB_REPLAY_WINDOW", "fortnight")
	if _, err := Load(); err == nil {
		t.Fatal("want an error for an unparsable duration")
	}

	t.Setenv("FLOWHUB_REPLAY_WINDOW", "10m")
	t.Setenv("FLOWHUB_ADDR", "no-port")
	if _, err := Load(); err == nil {
		t.Fatal("want an error for an address without a port")
	}

	t.Setenv("FLOWHUB_ADDR", "127.0.0.1:8080")
	t.Setenv("FLOWHUB_HOOK_PATH", "hooks/youtrack")
	if _, err := Load(); err == nil {
		t.Fatal("want an error for a hook path without a leading slash")
	}
}

// TestOpenCodeCredentialsMustBeSetTogether covers ADR 0001 step 2's new variables:
// Basic Auth is what lets the dispatcher reach a runtime that is not on loopback, and
// half a credential pair fails every turn instead of refusing the start.
func TestOpenCodeCredentialsMustBeSetTogether(t *testing.T) {
	t.Setenv("FLOWHUB_OPENCODE_USER", "flowhub")
	if _, err := Load(); err == nil {
		t.Fatal("a username without a password was accepted")
	}

	t.Setenv("FLOWHUB_OPENCODE_USER", "")
	t.Setenv("FLOWHUB_OPENCODE_PASSWORD", "hunter2")
	if _, err := Load(); err == nil {
		t.Fatal("a password without a username was accepted")
	}

	t.Setenv("FLOWHUB_OPENCODE_USER", "flowhub")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenCodeUser != "flowhub" || cfg.OpenCodePassword != "hunter2" {
		t.Fatalf("credentials = %q/%q", cfg.OpenCodeUser, cfg.OpenCodePassword)
	}
}

// TestReportMasksTheAgentCredentials: `-print-config` is a document that gets pasted
// into tickets, so the password may only appear as a fingerprint while the username
// (which is not a secret) stays readable.
func TestReportMasksTheAgentCredentials(t *testing.T) {
	cfg := Config{OpenCodeUser: "flowhub", OpenCodePassword: "hunter2"}
	report := cfg.Report()
	if strings.Contains(report, "hunter2") {
		t.Fatalf("the report leaked the agent password:\n%s", report)
	}
	if !strings.Contains(report, "flowhub") {
		t.Fatalf("the report does not name the authenticated user:\n%s", report)
	}
	if !strings.Contains(report, "opencode_auth:") {
		t.Fatalf("the report says nothing about agent authentication:\n%s", report)
	}
}

func TestWarningsNameEveryDisabledLock(t *testing.T) {
	cfg := Config{
		Addr:            "0.0.0.0:8080",
		HookPath:        DefaultHookPath,
		MaxBodyBytes:    DefaultMaxBodyBytes,
		DedupeTTL:       DefaultDedupeTTL,
		QueueSize:       DefaultQueueSize,
		LogFormat:       "text",
		LogLevel:        "info",
		ShutdownTimeout: DefaultShutdown,
	}
	warnings := strings.Join(cfg.Warnings(), "\n")
	for _, want := range []string{"FLOWHUB_HOOK_KEY", "FLOWHUB_TOKEN", "FLOWHUB_ALLOWED_SOURCES", "FLOWHUB_REPLAY_WINDOW"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("warnings do not mention %s:\n%s", want, warnings)
		}
	}
	// The wildcard bind is a startup problem rather than a warning until it is
	// acknowledged; see listen_test.go.
	if problems := cfg.Problems(); len(problems) != 1 {
		t.Fatalf("unacknowledged wildcard bind = %v, want one startup problem", problems)
	}
}

func TestMaskSecretNeverLeaksTheValue(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	masked := MaskSecret(secret)
	if strings.Contains(masked, secret) {
		t.Fatalf("masked value leaks the secret: %q", masked)
	}
	if !strings.Contains(masked, "len=32") {
		t.Fatalf("masked value = %q, want the length", masked)
	}
	if MaskSecret("") != "<unset>" {
		t.Fatalf("empty secret = %q", MaskSecret(""))
	}
}

func TestReportMasksSecrets(t *testing.T) {
	cfg := Config{
		Addr:            DefaultAddr,
		HookPath:        DefaultHookPath,
		HookKey:         "0123456789abcdef0123456789abcdef",
		TokenHeader:     DefaultTokenHeader,
		Token:           "feedfacefeedface",
		DataDir:         DefaultDataDir,
		MaxBodyBytes:    DefaultMaxBodyBytes,
		ReplayWindow:    time.Minute,
		DedupeTTL:       time.Hour,
		QueueSize:       8,
		LogFormat:       "text",
		LogLevel:        "info",
		ShutdownTimeout: time.Second,
	}
	report := cfg.Report()
	if strings.Contains(report, cfg.HookKey) || strings.Contains(report, cfg.Token) {
		t.Fatalf("report leaks a secret:\n%s", report)
	}
	if !strings.Contains(report, "active_locks:       url_key,header_token") {
		t.Fatalf("report lacks the active locks:\n%s", report)
	}
}
