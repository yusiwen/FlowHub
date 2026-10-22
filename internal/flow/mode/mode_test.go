package mode_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yusiwen/flowhub/internal/flow/mode"
)

func TestDefaultIsOff(t *testing.T) {
	store, err := mode.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := store.Mode("default-youtrack"); got != mode.Off {
		t.Fatalf("mode = %q, want off", got)
	}
}

func TestSetPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	store, err := mode.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Set("default-youtrack", mode.Shadow, nil, "test"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := store.Mode("default-youtrack"); got != mode.Shadow {
		t.Fatalf("mode = %q", got)
	}
	info, err := os.Stat(mode.Path(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}

	reopened, err := mode.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.Mode("default-youtrack"); got != mode.Shadow {
		t.Fatalf("reopened mode = %q", got)
	}
	entries := reopened.Entries()
	if entries["default-youtrack"].UpdatedBy != "test" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestMutationIsVisibleToAnotherStore(t *testing.T) {
	dir := t.TempDir()
	first, err := mode.Open(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := mode.Open(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := second.Mode("default-youtrack"); got != mode.Off {
		t.Fatalf("initial mode = %q", got)
	}
	if err := first.Set("default-youtrack", mode.Shadow, nil, "first"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := second.Mode("default-youtrack"); got != mode.Shadow {
		t.Fatalf("after set = %q", got)
	}
}

func TestProjectsArePersistedAndCopied(t *testing.T) {
	dir := t.TempDir()
	store, err := mode.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Set("default-youtrack", mode.On, []string{" TEST ", "SP", "TEST", ""}, "test"); err != nil {
		t.Fatalf("set: %v", err)
	}
	entry, ok := store.Entry("default-youtrack")
	if !ok || entry.Mode != mode.On {
		t.Fatalf("entry = %+v, ok = %v", entry, ok)
	}
	if got := strings.Join(entry.Projects, ","); got != "TEST,SP" {
		t.Fatalf("projects = %q, want TEST,SP", got)
	}
	entry.Projects[0] = "MUTATED"
	again, _ := store.Entry("default-youtrack")
	if again.Projects[0] != "TEST" {
		t.Fatalf("projects were not copied: %+v", again.Projects)
	}
}

func TestInvalidModeIsRefused(t *testing.T) {
	store, err := mode.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Set("default-youtrack", "bogus", nil, "test"); err == nil {
		t.Fatal("expected an error")
	}
	if err := store.Set("", mode.Off, nil, "test"); err == nil {
		t.Fatal("expected an error for an empty flow id")
	}
}

func TestMalformedStateStaysOffAndReports(t *testing.T) {
	dir := t.TempDir()
	store, err := mode.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	path := mode.Path(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := store.Mode("default-youtrack"); got != mode.Off {
		t.Fatalf("mode = %q, want off", got)
	}
	if store.Error() == "" {
		t.Fatal("expected the decode error to be reported")
	}
}
