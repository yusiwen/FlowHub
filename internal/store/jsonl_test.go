package store

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/metrics"
)

func TestJSONLWritesOneLinePerRecordAndRotatesDaily(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	audit, err := OpenJSONL(dir)
	if err != nil {
		t.Fatalf("OpenJSONL: %v", err)
	}
	defer audit.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("data dir perm = %o, want 700", perm)
	}

	day := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	audit.now = func() time.Time { return day }

	if err := audit.Record(&Record{Kind: "webhook", Time: day, Accepted: true, Event: "issueCreated"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	first := audit.Path()
	if filepath.Base(first) != "webhook-2026-09-19.jsonl" {
		t.Fatalf("first file = %s", first)
	}

	audit.now = func() time.Time { return day.Add(24 * time.Hour) }
	if err := audit.Record(&Record{Kind: "webhook", Time: day, Accepted: false, Reason: "bad_url_key"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	second := audit.Path()
	if filepath.Base(second) != "webhook-2026-09-20.jsonl" {
		t.Fatalf("second file = %s", second)
	}

	firstLines := readLines(t, first)
	if len(firstLines) != 1 {
		t.Fatalf("first file has %d lines, want 1", len(firstLines))
	}
	secondLines := readLines(t, second)
	if len(secondLines) != 1 {
		t.Fatalf("second file has %d lines, want 1", len(secondLines))
	}

	var record Record
	if err := json.Unmarshal([]byte(firstLines[0]), &record); err != nil {
		t.Fatalf("audit line is not valid JSON: %v", err)
	}
	if record.Event != "issueCreated" || !record.Accepted {
		t.Fatalf("record = %+v", record)
	}

	info, err = os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("audit file perm = %o, want 600", perm)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSuffix(string(raw), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

type slowRecorder struct {
	records []*Record
	fail    bool
}

func (s *slowRecorder) Record(rec *Record) error {
	if s.fail {
		return os.ErrInvalid
	}
	s.records = append(s.records, rec)
	return nil
}

func (s *slowRecorder) Close() error { return nil }

func TestAsyncPersistsEveryRecord(t *testing.T) {
	inner := &slowRecorder{}
	stats := metrics.New()
	sink := NewAsync(inner, 8, slog.New(slog.NewTextHandler(io.Discard, nil)), stats)

	for i := 0; i < 5; i++ {
		sink.Record(&Record{Kind: "webhook", Event: "issueUpdated"})
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(inner.records) != 5 {
		t.Fatalf("persisted %d records, want 5", len(inner.records))
	}
	if stats.Persisted.Load() != 5 {
		t.Fatalf("persisted counter = %d", stats.Persisted.Load())
	}
	if stats.Dropped.Load() != 0 {
		t.Fatalf("dropped counter = %d", stats.Dropped.Load())
	}
}

func TestAsyncDropsInsteadOfBlockingWhenQueueIsFull(t *testing.T) {
	inner := &slowRecorder{}
	stats := metrics.New()
	sink := NewAsync(inner, 1, slog.New(slog.NewTextHandler(io.Discard, nil)), stats)

	// Filling the single slot plus overflowing is fine: Record must never block,
	// because the YouTrack delivery path is synchronous on the app side.
	for i := 0; i < 200; i++ {
		sink.Record(&Record{Kind: "webhook", Event: "issueUpdated"})
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if stats.Dropped.Load() == 0 {
		t.Log("queue kept up; overflow path not exercised")
	}
	if stats.Persisted.Load()+stats.Dropped.Load() != 200 {
		t.Fatalf("persisted %d + dropped %d != 200", stats.Persisted.Load(), stats.Dropped.Load())
	}
}

func TestAsyncReportsWriteFailures(t *testing.T) {
	inner := &slowRecorder{fail: true}
	stats := metrics.New()
	sink := NewAsync(inner, 4, slog.New(slog.NewTextHandler(io.Discard, nil)), stats)

	sink.Record(&Record{Kind: "webhook"})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if stats.PersistErrors.Load() != 1 {
		t.Fatalf("persist errors = %d", stats.PersistErrors.Load())
	}
	if stats.Persisted.Load() != 0 {
		t.Fatalf("persisted = %d", stats.Persisted.Load())
	}
}
