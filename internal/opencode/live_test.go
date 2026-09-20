package opencode

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveTask drives one real turn against a running `opencode serve`.
//
// It is skipped unless OPENCODE_LIVE=1 so that `make test` never spends tokens or
// touches a developer's live sessions. Run it with:
//
//	make test-live
//	OPENCODE_LIVE=1 OPENCODE_LIVE_DIR=/path/to/repo make test-live
//
// With OPENCODE_LIVE_DIR unset it uses a throwaway directory and a prompt whose
// answer can be checked exactly. With it set, the prompt is a read-only
// inspection of that repository, which is how the routing table is verified
// against a real project.
func TestLiveTask(t *testing.T) {
	if os.Getenv("OPENCODE_LIVE") != "1" {
		t.Skip("set OPENCODE_LIVE=1 to run against a live opencode server")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	client := New(Options{
		BaseURL:  os.Getenv("OPENCODE_URL"),
		Username: os.Getenv("OPENCODE_SERVER_USERNAME"),
		Password: os.Getenv("OPENCODE_SERVER_PASSWORD"),
		Timeout:  30 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	health, err := client.Health(ctx)
	if err != nil {
		t.Skipf("no opencode server at %s: %v", client.BaseURL(), err)
	}
	t.Logf("opencode %s healthy=%t", health.Version, health.Healthy)

	directory := os.Getenv("OPENCODE_LIVE_DIR")
	prompt := ""
	if directory == "" {
		directory = t.TempDir()
		// t.TempDir is already canonical on Linux; macOS /var is a symlink, and
		// opencode keys sessions by the literal directory string.
		directory = canonicalForTest(t, directory)
		if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("trial\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(directory, "internal"), 0o755); err != nil {
			t.Fatal(err)
		}
		prompt = "Run `ls -a`. Then reply with only the number of entries it listed."
	} else {
		directory = canonicalForTest(t, directory)
		prompt = "Inspect this repository read-only: run `git status --short` and `git log --oneline -3`. " +
			"Then answer in at most three lines: the current branch, whether the working tree is clean, and the subject of the newest commit."
	}
	t.Logf("directory: %s", directory)

	ruleset := []PermissionRule{
		{Permission: "read", Pattern: "*", Action: "allow"},
		{Permission: "edit", Pattern: "*", Action: "deny"},
		{Permission: "external_directory", Pattern: "*", Action: "deny"},
		{Permission: "webfetch", Pattern: "*", Action: "deny"},
		{Permission: "websearch", Pattern: "*", Action: "deny"},
		{Permission: "bash", Pattern: "*", Action: "ask"},
	}

	runner := NewRunner(client, DefaultArbiter(), logger)
	runner.Poll = 2 * time.Second

	result, err := runner.Run(ctx, Task{
		Directory: directory,
		Prompt:    prompt,
		Agent:     os.Getenv("OPENCODE_AGENT"),
		Title:     "flowhub live check",
		Ruleset:   ruleset,
		Metadata:  map[string]any{"task_key": "live-check", "source": "flowhub test-live"},
		Deadline:  2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Logf("session=%s finished=%t timedout=%t elapsed=%s cost=%.6f tokens=%d permissions=%d",
		result.SessionID, result.Finished, result.TimedOut, result.Elapsed.Round(time.Millisecond),
		result.Cost, result.Tokens.Total, len(result.Permissions))
	for _, answered := range result.Permissions {
		t.Logf("  permission %s %q -> %s (%s)", answered.Permission, answered.Command, answered.Reply, answered.Reason)
	}
	t.Logf("answer: %s", strings.ReplaceAll(result.Text, "\n", " | "))

	if !result.Finished {
		t.Fatalf("turn did not finish (timed out=%t): the session may still be running", result.TimedOut)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Fatal("the turn finished without producing any text")
	}
	// Any rejection means the default policy blocked something the agent judged
	// necessary; that is a signal to review the allowlist, not to ignore.
	for _, answered := range result.Permissions {
		if !answered.Allowed() {
			t.Errorf("policy rejected %q (%s); review the arbiter allowlist", answered.Command, answered.Reason)
		}
	}

	if os.Getenv("OPENCODE_LIVE_DIR") == "" {
		// The temp directory holds README.md, internal/, and the two dot entries
		// that `ls -a` always lists.
		if !strings.Contains(result.Text, "4") {
			t.Errorf("expected the entry count 4 in the answer, got %q", result.Text)
		}
	}
}

// canonicalForTest resolves symlinks the way the dispatcher must, because
// opencode treats /tmp and /private/tmp as different projects.
func canonicalForTest(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}
