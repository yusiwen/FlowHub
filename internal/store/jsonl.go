package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JSONL is an append-only audit log that writes one JSON object per line and
// rotates to a new file at each UTC day boundary.
//
// Files are created 0600 inside a 0700 directory: payloads contain issue text
// and the design documents require the audit trail to stay private.
type JSONL struct {
	dir string
	now func() time.Time

	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	day  string
	path string
}

const filePrefix = "webhook-"

// OpenJSONL prepares dir for audit logging.
func OpenJSONL(dir string) (*JSONL, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	return &JSONL{dir: dir, now: time.Now}, nil
}

// Record appends one record as a single line.
func (s *JSONL) Record(rec *Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	day := s.now().UTC().Format(time.DateOnly)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.f == nil || s.day != day {
		if err := s.rotateLocked(day); err != nil {
			return err
		}
	}
	if _, err := s.w.Write(line); err != nil {
		return err
	}
	if err := s.w.WriteByte('\n'); err != nil {
		return err
	}
	// Flush per record so `tail -f` shows deliveries immediately. The page cache
	// is durable enough for an audit trail that can be re-derived from YouTrack.
	return s.w.Flush()
}

// Path returns the file currently being written to, or "" before the first
// record of the process.
func (s *JSONL) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// Close flushes and closes the current file.
func (s *JSONL) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *JSONL) rotateLocked(day string) error {
	if err := s.closeLocked(); err != nil {
		return err
	}
	path := filepath.Join(s.dir, filePrefix+day+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log %s: %w", path, err)
	}
	s.f = f
	s.w = bufio.NewWriter(f)
	s.day = day
	s.path = path
	return nil
}

func (s *JSONL) closeLocked() error {
	var err error
	if s.w != nil {
		err = s.w.Flush()
		s.w = nil
	}
	if s.f != nil {
		if cerr := s.f.Close(); err == nil {
			err = cerr
		}
		s.f = nil
	}
	return err
}
