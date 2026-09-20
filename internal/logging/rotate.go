// Package logging builds the FlowHub application logger: always stderr, plus an
// optional size-rotated log file.
//
// The application log is the operator's view (startup, verdicts, errors). The
// detailed view of a single delivery lives in the payload log
// (internal/store.Detail) and the machine readable audit stays in JSONL.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingFile is an io.Writer that rotates a log file once it would grow past
// MaxBytes. Archives are named file.log.1 (newest) up to file.log.N.
type RotatingFile struct {
	path     string
	maxBytes int64
	maxFiles int

	mu   sync.Mutex
	file *os.File
	size int64
}

// OpenRotatingFile opens path for appending, creating parent directories with
// 0700 and the file with 0600.
func OpenRotatingFile(path string, maxBytes int64, maxFiles int) (*RotatingFile, error) {
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	if maxFiles < 1 {
		maxFiles = 1
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create log directory %s: %w", dir, err)
		}
	}
	writer := &RotatingFile{path: path, maxBytes: maxBytes, maxFiles: maxFiles}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

// Path returns the active file path.
func (w *RotatingFile) Path() string { return w.path }

// Write appends p, rotating first when the file would exceed MaxBytes.
func (w *RotatingFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the active file.
func (w *RotatingFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *RotatingFile) open() error {
	file, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", w.path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat log file %s: %w", w.path, err)
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *RotatingFile) rotateLocked() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}

	// Drop the oldest archive, then shift the others up by one: path.N-1 -> path.N.
	_ = os.Remove(archivePath(w.path, w.maxFiles))
	for i := w.maxFiles - 1; i >= 1; i-- {
		if err := os.Rename(archivePath(w.path, i), archivePath(w.path, i+1)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotate log archive %d: %w", i, err)
		}
	}
	if err := os.Rename(w.path, archivePath(w.path, 1)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotate log file: %w", err)
	}

	w.size = 0
	return w.open()
}

func archivePath(path string, n int) string {
	return fmt.Sprintf("%s.%d", path, n)
}
