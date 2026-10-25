package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestFlowCommandLifecycle(t *testing.T) {
	t.Setenv("FLOWHUB_DATA_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer

	stdout.Reset()
	if code := runFlowCommand([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "default-youtrack") || !strings.Contains(stdout.String(), "off") {
		t.Fatalf("list output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runFlowCommand([]string{"mode", "default-youtrack", "shadow", "--project", "TEST", "--project", "SP"}, &stdout, &stderr); code != 0 {
		t.Fatalf("mode code = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	if code := runFlowCommand([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list code = %d", code)
	}
	if !strings.Contains(stdout.String(), "shadow") || !strings.Contains(stdout.String(), "TEST,SP") {
		t.Fatalf("list output = %q", stdout.String())
	}

	stdout.Reset()
	if code := runFlowCommand([]string{"show", "default-youtrack"}, &stdout, &stderr); code != 0 {
		t.Fatalf("show code = %d", code)
	}
	if !strings.Contains(stdout.String(), `"id": "default-youtrack"`) {
		t.Fatalf("show output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runFlowCommand([]string{"mode", "default-youtrack", "on"}, &stdout, &stderr); code != 0 {
		t.Fatalf("on code = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	if code := runFlowCommand([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list code = %d", code)
	}
	if !strings.Contains(stdout.String(), "on") {
		t.Fatalf("on list output = %q", stdout.String())
	}
	if code := runFlowCommand([]string{"show", "unknown"}, &stdout, &stderr); code == 0 {
		t.Fatal("unknown flow must be refused")
	}
	if code := runFlowCommand(nil, &stdout, &stderr); code == 0 {
		t.Fatal("a missing subcommand must be refused")
	}
}

func TestFlowModeOnPersistsWithoutDispatch(t *testing.T) {
	t.Setenv("FLOWHUB_DATA_DIR", t.TempDir())
	t.Setenv("FLOWHUB_DISPATCH", "0")
	var stdout, stderr bytes.Buffer
	if code := runFlowCommand([]string{"mode", "default-youtrack", "on", "--project", "TEST"}, &stdout, &stderr); code != 0 {
		t.Fatalf("on code = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	if code := runFlowCommand([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list code = %d", code)
	}
	if !strings.Contains(stdout.String(), "on") || !strings.Contains(stdout.String(), "TEST") {
		t.Fatalf("list output = %q", stdout.String())
	}
}
