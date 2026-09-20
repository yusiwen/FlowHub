package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeN(t *testing.T, w *RotatingFile, chunk string, times int) {
	t.Helper()
	for i := 0; i < times; i++ {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
}

func TestRotatingFileCreatesDirAndFileWithRestrictiveModes(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "logs", "flowhub.log")

	writer, err := OpenRotatingFile(path, 1024, 3)
	if err != nil {
		t.Fatalf("OpenRotatingFile: %v", err)
	}
	defer writer.Close()

	if _, err := writer.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("log dir perm = %o, want 700", perm)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("log file perm = %o, want 600", perm)
	}
	if got := readFile(t, path); got != "hello\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestRotatingFileShiftsArchivesAndDropsTheOldest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flowhub.log")
	writer, err := OpenRotatingFile(path, 100, 2)
	if err != nil {
		t.Fatalf("OpenRotatingFile: %v", err)
	}
	defer writer.Close()

	// Each write is 60 bytes, so every second write forces a rotation.
	writeN(t, writer, strings.Repeat("A", 60), 1) // -> flowhub.log
	writeN(t, writer, strings.Repeat("B", 60), 1) // rotates A to .1, B in flowhub.log
	writeN(t, writer, strings.Repeat("C", 60), 1) // rotates B to .1 (A to .2)
	writeN(t, writer, strings.Repeat("D", 60), 1) // rotates C to .1 (B to .2, A dropped)

	cases := map[string]string{
		path:        "D",
		path + ".1": "C",
		path + ".2": "B",
	}
	for file, wantPrefix := range cases {
		content := readFile(t, file)
		if !strings.HasPrefix(content, wantPrefix) {
			t.Fatalf("%s = %.12q…, want it to start with %q", filepath.Base(file), content, wantPrefix)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("archive beyond MaxFiles must not exist (err = %v)", err)
	}
}

func TestRotatingFileReopensAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flowhub.log")
	writer, err := OpenRotatingFile(path, 1024, 2)
	if err != nil {
		t.Fatalf("OpenRotatingFile: %v", err)
	}
	if _, err := writer.Write([]byte("first\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := writer.Write([]byte("second\n")); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := readFile(t, path); got != "first\nsecond\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestNewWithoutFileDisablesTheCloser(t *testing.T) {
	for _, path := range []string{"", "-", "none"} {
		logger, closer, err := New(Config{Level: "info", Format: "text", File: path})
		if err != nil {
			t.Fatalf("New(%q): %v", path, err)
		}
		if logger == nil {
			t.Fatalf("New(%q) returned no logger", path)
		}
		if closer != nil {
			t.Fatalf("New(%q) returned a closer, want nil", path)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
