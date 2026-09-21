package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDispatchIsOffByDefault(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Dispatch {
		t.Fatal("dispatch must default to off: recording only is phase 1")
	}
	if cfg.OpenCodeURL != DefaultOpenCodeURL {
		t.Fatalf("opencode url = %q", cfg.OpenCodeURL)
	}
	if cfg.DispatchAgent != DefaultDispatchAgent {
		t.Fatalf("agent = %q", cfg.DispatchAgent)
	}
	if cfg.DispatchQueueSize != DefaultDispatchQueue {
		t.Fatalf("queue = %d", cfg.DispatchQueueSize)
	}
	if cfg.TaskDeadline != DefaultTaskDeadline {
		t.Fatalf("deadline = %s", cfg.TaskDeadline)
	}
	if cfg.MaxTurns != DefaultMaxTurns {
		t.Fatalf("max turns = %d", cfg.MaxTurns)
	}
	if cfg.Trigger != DefaultTrigger {
		t.Fatalf("trigger = %q", cfg.Trigger)
	}
	if len(cfg.StartStates) != 1 || cfg.StartStates[0] != DefaultStartState {
		t.Fatalf("start states = %v", cfg.StartStates)
	}
	if cfg.SkipAnalyzeOnCreate {
		t.Fatal("analysis on creation must be on by default")
	}
	if got := cfg.ResolvedRegistryFile(); got != filepath.Join(cfg.DataDir, DefaultRegistryName) {
		t.Fatalf("registry = %q", got)
	}
	if got := cfg.ResolvedPauseFile(); got != filepath.Join(cfg.DataDir, DefaultPauseName) {
		t.Fatalf("pause file = %q", got)
	}
}

func TestLoadReadsDispatchEnvironment(t *testing.T) {
	t.Setenv("FLOWHUB_DISPATCH", "1")
	t.Setenv("FLOWHUB_OPENCODE_URL", "http://127.0.0.1:5000")
	t.Setenv("FLOWHUB_DISPATCH_AGENT", "flowhub")
	t.Setenv("FLOWHUB_DISPATCH_QUEUE", "4")
	t.Setenv("FLOWHUB_TASK_DEADLINE", "90s")
	t.Setenv("FLOWHUB_MAX_TURNS", "3")
	t.Setenv("FLOWHUB_TRIGGER", "/flowhub go")
	t.Setenv("FLOWHUB_START_STATES", "In Progress, 进行中 ")
	t.Setenv("FLOWHUB_SKIP_ANALYZE_ON_CREATE", "true")
	t.Setenv("FLOWHUB_TASK_MAX_COST", "1.5")
	t.Setenv("FLOWHUB_REGISTRY_FILE", "/tmp/registry.jsonl")
	t.Setenv("FLOWHUB_PAUSE_FILE", "/tmp/PAUSE")
	t.Setenv("FLOWHUB_WORKTREE_BASE", "/tmp/worktrees")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Dispatch || cfg.OpenCodeURL != "http://127.0.0.1:5000" || cfg.DispatchAgent != "flowhub" {
		t.Fatalf("dispatch settings = %+v", cfg)
	}
	if cfg.DispatchQueueSize != 4 || cfg.MaxTurns != 3 || cfg.TaskDeadline != 90*time.Second {
		t.Fatalf("limits = %d/%d/%s", cfg.DispatchQueueSize, cfg.MaxTurns, cfg.TaskDeadline)
	}
	if cfg.Trigger != "/flowhub go" {
		t.Fatalf("trigger = %q", cfg.Trigger)
	}
	if strings.Join(cfg.StartStates, "|") != "In Progress|进行中" {
		t.Fatalf("start states = %q", cfg.StartStates)
	}
	if !cfg.SkipAnalyzeOnCreate || cfg.TaskMaxCost != 1.5 {
		t.Fatalf("skip_analyze=%t cost=%v", cfg.SkipAnalyzeOnCreate, cfg.TaskMaxCost)
	}
	if cfg.ResolvedRegistryFile() != "/tmp/registry.jsonl" || cfg.ResolvedPauseFile() != "/tmp/PAUSE" {
		t.Fatalf("paths = %q/%q", cfg.ResolvedRegistryFile(), cfg.ResolvedPauseFile())
	}
	if cfg.WorktreeBase != "/tmp/worktrees" {
		t.Fatalf("worktree base = %q", cfg.WorktreeBase)
	}
}

func TestLoadRejectsBadDispatchValues(t *testing.T) {
	cases := []struct {
		key, value, want string
	}{
		{"FLOWHUB_DISPATCH", "maybe", "FLOWHUB_DISPATCH"},
		{"FLOWHUB_DISPATCH_QUEUE", "0", "FLOWHUB_DISPATCH_QUEUE"},
		{"FLOWHUB_MAX_TURNS", "-1", "FLOWHUB_MAX_TURNS"},
		{"FLOWHUB_TASK_DEADLINE", "-5s", "FLOWHUB_TASK_DEADLINE"},
		{"FLOWHUB_TASK_MAX_COST", "free", "FLOWHUB_TASK_MAX_COST"},
		{"FLOWHUB_TASK_MAX_COST", "-1", "FLOWHUB_TASK_MAX_COST"},
		{"FLOWHUB_OPENCODE_URL", "127.0.0.1:4096", "FLOWHUB_OPENCODE_URL"},
		{"FLOWHUB_OPENCODE_URL", "unix:///tmp/sock", "FLOWHUB_OPENCODE_URL"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			_, err := Load()
			if err == nil {
				t.Fatalf("want an error for %s=%s", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

func TestReportShowsDispatchSettings(t *testing.T) {
	t.Setenv("FLOWHUB_DISPATCH", "1")
	t.Setenv("FLOWHUB_DISPATCH_AGENT", "devops")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	report := cfg.Report()
	for _, want := range []string{
		"dispatch:           true",
		"dispatch_agent:     devops",
		"trigger:            \"/opencode start\"",
		"start_states:       In Progress",
		"task_max_cost:      <disabled>",
		"touch it to pause dispatch",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q:\n%s", want, report)
		}
	}
}
