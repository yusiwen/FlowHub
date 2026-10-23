package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCursorPersistsAndLoads(t *testing.T) {
	dir := t.TempDir()
	first := &Engine{opts: Options{DataDir: dir}}
	if first.Cursor() != "" {
		t.Fatalf("new cursor = %q, want empty", first.Cursor())
	}
	if err := first.SetCursor("evt_1"); err != nil {
		t.Fatal(err)
	}
	if got := first.Cursor(); got != "evt_1" {
		t.Fatalf("cursor = %q", got)
	}
	info, err := os.Stat(filepath.Join(dir, "flow-cursor.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cursor mode = %o, want 600", info.Mode().Perm())
	}

	second := &Engine{opts: Options{DataDir: dir}}
	if err := second.loadCursor(); err != nil {
		t.Fatal(err)
	}
	if got := second.Cursor(); got != "evt_1" {
		t.Fatalf("reloaded cursor = %q", got)
	}
	if err := second.SetCursor(""); err != nil {
		t.Fatalf("empty set: %v", err)
	}
	if got := second.Cursor(); got != "evt_1" {
		t.Fatalf("empty set changed cursor to %q", got)
	}
}
