package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggingDefaultsServePayloadAnalysis(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.LogHeaders {
		t.Fatal("header logging must be on by default: comparing the real payload is the point of phase 1")
	}
	if !cfg.DetailLog {
		t.Fatal("the human readable payload log must be on by default")
	}
	if cfg.LogMaxBytes != DefaultLogMaxBytes || cfg.LogMaxFiles != DefaultLogMaxFiles {
		t.Fatalf("rotation defaults = %d x %d", cfg.LogMaxBytes, cfg.LogMaxFiles)
	}
	if cfg.DetailLogMaxBodyBytes != DefaultDetailMaxBody {
		t.Fatalf("detail body cap = %d", cfg.DetailLogMaxBodyBytes)
	}

	want := filepath.Join(DefaultDataDir, DefaultLogFile)
	if got := cfg.ResolvedLogFile(); got != want {
		t.Fatalf("resolved log file = %q, want %q", got, want)
	}
}

func TestResolvedLogFileHonoursExplicitPathAndDisable(t *testing.T) {
	cfg := Config{DataDir: "/var/lib/flowhub", LogFile: "/var/log/flowhub.log"}
	if got := cfg.ResolvedLogFile(); got != "/var/log/flowhub.log" {
		t.Fatalf("explicit path ignored: %q", got)
	}
	cfg.LogFile = "-"
	if got := cfg.ResolvedLogFile(); got != "-" {
		t.Fatalf("disable marker ignored: %q", got)
	}
}

func TestLoadReadsLoggingEnvironment(t *testing.T) {
	t.Setenv("FLOWHUB_LOG_FILE", "/tmp/flowhub-test.log")
	t.Setenv("FLOWHUB_LOG_MAX_BYTES", "1024")
	t.Setenv("FLOWHUB_LOG_MAX_FILES", "2")
	t.Setenv("FLOWHUB_LOG_HEADERS", "false")
	t.Setenv("FLOWHUB_DETAIL_LOG", "false")
	t.Setenv("FLOWHUB_DETAIL_MAX_BODY_BYTES", "2048")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ResolvedLogFile() != "/tmp/flowhub-test.log" {
		t.Fatalf("log file = %q", cfg.ResolvedLogFile())
	}
	if cfg.LogMaxBytes != 1024 || cfg.LogMaxFiles != 2 {
		t.Fatalf("rotation = %d x %d", cfg.LogMaxBytes, cfg.LogMaxFiles)
	}
	if cfg.LogHeaders || cfg.DetailLog {
		t.Fatalf("toggles ignored: headers=%t detail=%t", cfg.LogHeaders, cfg.DetailLog)
	}
	if cfg.DetailLogMaxBodyBytes != 2048 {
		t.Fatalf("detail body cap = %d", cfg.DetailLogMaxBodyBytes)
	}
}

func TestLoadRejectsBadLoggingValues(t *testing.T) {
	t.Setenv("FLOWHUB_LOG_MAX_BYTES", "0")
	if _, err := Load(); err == nil {
		t.Fatal("want an error for a zero rotation size")
	}

	t.Setenv("FLOWHUB_LOG_MAX_BYTES", "1024")
	t.Setenv("FLOWHUB_LOG_MAX_FILES", "-1")
	if _, err := Load(); err == nil {
		t.Fatal("want an error for a negative archive count")
	}

	t.Setenv("FLOWHUB_LOG_MAX_FILES", "3")
	t.Setenv("FLOWHUB_DETAIL_MAX_BODY_BYTES", "0")
	if _, err := Load(); err == nil {
		t.Fatal("want an error for a zero detail body cap")
	}
}

func TestReportShowsLoggingTargets(t *testing.T) {
	cfg := Config{
		Addr:                  DefaultAddr,
		HookPath:              DefaultHookPath,
		DataDir:               "/tmp/flowhub-data",
		LogMaxBytes:           DefaultLogMaxBytes,
		LogMaxFiles:           DefaultLogMaxFiles,
		DetailLog:             true,
		DetailLogMaxBodyBytes: DefaultDetailMaxBody,
		LogFormat:             "text",
		LogLevel:              "info",
	}
	report := cfg.Report()
	for _, want := range []string{
		"log_file:           /tmp/flowhub-data/flowhub.log",
		"detail_log:         true (/tmp/flowhub-data/payload-<date>.log",
		"log_headers:",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report is missing %q:\n%s", want, report)
		}
	}
}
