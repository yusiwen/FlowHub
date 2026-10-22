package upstream_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/upstream"
)

func logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func event(received time.Time) upstream.Event {
	return upstream.Event{
		Schema:     upstream.Schema,
		Upstream:   "youtrack",
		Kind:       upstream.KindCommentAdded,
		OccurredAt: received.Add(-time.Second),
		ReceivedAt: received,
		Subject: upstream.Subject{
			Type: "issue", Project: "TEST", Key: "TEST-12", Title: "Fix the widget",
		},
		Actor:     upstream.Actor{Login: "yusiwen"},
		Comment:   &upstream.Comment{Text: "/opencode start", Author: "yusiwen"},
		Payload:   json.RawMessage(`{"id":"TEST-12"}`),
		DedupeKey: "sha256:abc",
		Origin:    "upstream:youtrack",
	}
}

func TestLogAppendsOneEventAndAssignsAnID(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	log, err := upstream.NewLog(upstream.LogOptions{
		Dir: dir, Logger: logger(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	defer func() { _ = log.Close() }()

	log.Record(event(now))
	log.Flush()

	path := filepath.Join(log.Dir(), now.Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read event file: %v", err)
	}
	var got upstream.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if !strings.HasPrefix(got.ID, "evt_") {
		t.Fatalf("id = %q, want an evt_ prefix", got.ID)
	}
	if got.Subject.Key != "TEST-12" || got.Kind != upstream.KindCommentAdded {
		t.Fatalf("unexpected event: %+v", got)
	}
	if got.ReceivedAt.IsZero() {
		t.Fatal("received_at was not set")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat event file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("event file mode = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(log.Dir())
	if err != nil {
		t.Fatalf("stat event dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("event dir mode = %o, want 700", dirInfo.Mode().Perm())
	}

	snapshot := log.Snapshot()
	if snapshot["recorded"] != int64(1) || snapshot["dropped"] != int64(0) {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot["last_event_at"] == "" {
		t.Fatalf("snapshot has no last event: %+v", snapshot)
	}
}

func TestLogDropsDuplicateEvents(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	log, err := upstream.NewLog(upstream.LogOptions{Dir: dir, Logger: logger()})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	defer func() { _ = log.Close() }()

	log.Record(event(now))
	log.Record(event(now))
	log.Flush()

	path := filepath.Join(log.Dir(), now.Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read event file: %v", err)
	}
	if lines := len(strings.Split(strings.TrimSpace(string(data)), "\n")); lines != 1 {
		t.Fatalf("duplicate event was written: %q", data)
	}
	snapshot := log.Snapshot()
	if snapshot["recorded"] != int64(1) || snapshot["duplicates"] != int64(1) {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestLogRotatesByReceivedDay(t *testing.T) {
	dir := t.TempDir()
	first := time.Date(2026, 10, 21, 23, 59, 0, 0, time.UTC)
	second := first.Add(2 * time.Minute)
	log, err := upstream.NewLog(upstream.LogOptions{Dir: dir, Logger: logger()})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	defer func() { _ = log.Close() }()

	log.Record(event(first))
	next := event(second)
	next.DedupeKey = "sha256:def"
	log.Record(next)
	log.Flush()

	for _, day := range []string{"2026-10-21", "2026-10-22"} {
		path := filepath.Join(log.Dir(), day+".jsonl")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s: %v", path, err)
		}
	}
}

func TestLogRejectsAnInvalidEvent(t *testing.T) {
	dir := t.TempDir()
	log, err := upstream.NewLog(upstream.LogOptions{Dir: dir, Logger: logger()})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	defer func() { _ = log.Close() }()

	log.Record(upstream.Event{})
	log.Flush()
	snapshot := log.Snapshot()
	if snapshot["invalid"] != int64(1) {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestNewIDIsUnique(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	first := upstream.NewID(now)
	second := upstream.NewID(now)
	if !strings.HasPrefix(first, "evt_") || !strings.HasPrefix(second, "evt_") {
		t.Fatalf("ids = %q %q", first, second)
	}
	if first == second {
		t.Fatalf("ids are identical: %q", first)
	}
}

func TestLogPrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	eventsDir := filepath.Join(dir, "events")
	if err := os.MkdirAll(eventsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := filepath.Join(eventsDir, "2020-01-01.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	recent := filepath.Join(eventsDir, "2026-10-21.jsonl")
	if err := os.WriteFile(recent, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write recent file: %v", err)
	}
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	log, err := upstream.NewLog(upstream.LogOptions{
		Dir: dir, Logger: logger(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	defer func() { _ = log.Close() }()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old file was not pruned: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent file was pruned: %v", err)
	}
}

func TestLogNotifiesSubscribers(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	log, err := upstream.NewLog(upstream.LogOptions{Dir: dir, Logger: logger()})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	defer func() { _ = log.Close() }()
	notified := make(chan upstream.Event, 2)
	log.Subscribe(func(event upstream.Event) { notified <- event })

	log.Record(event(now))
	log.Flush()
	select {
	case event := <-notified:
		if event.Subject.Key != "TEST-12" || event.ID == "" {
			t.Fatalf("unexpected event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber was not called")
	}

	log.Record(event(now))
	log.Flush()
	select {
	case event := <-notified:
		t.Fatalf("duplicate notified: %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAppendWritesBeforeItReturns(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	log, err := upstream.NewLog(upstream.LogOptions{
		Dir: dir, Logger: logger(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	defer func() { _ = log.Close() }()

	if _, err := log.Append(event(now)); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "events", "2026-10-21.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != 1 {
		t.Fatalf("lines = %d, body = %s", lines, data)
	}
	var stored upstream.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stored.ID == "" || stored.ReceivedAt.IsZero() {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestAppendAbsorbsADuplicate(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	log, err := upstream.NewLog(upstream.LogOptions{
		Dir: dir, Logger: logger(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	defer func() { _ = log.Close() }()

	if _, err := log.Append(event(now)); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := log.Append(event(now)); err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "events", "2026-10-21.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != 1 {
		t.Fatalf("lines = %d, body = %s", lines, data)
	}
	if got := log.Snapshot()["duplicates"]; got != int64(1) {
		t.Fatalf("duplicates = %v", got)
	}
}

func TestAFailedAppendReleasesItsDedupeKey(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	log, err := upstream.NewLog(upstream.LogOptions{
		Dir: dir, Logger: logger(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	defer func() { _ = log.Close() }()
	eventsDir := filepath.Join(dir, "events")
	// A directory at the day-file path makes OpenFile fail deterministically, also
	// for a test process running as root.
	if err := os.Mkdir(filepath.Join(eventsDir, "2026-10-21.jsonl"), 0o700); err != nil {
		t.Fatalf("block day file: %v", err)
	}
	if _, err := log.Append(event(now)); err == nil {
		t.Fatal("expected the append to fail while the day-file path is a directory")
	}
	if err := os.Remove(filepath.Join(eventsDir, "2026-10-21.jsonl")); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if _, err := log.Append(event(now)); err != nil {
		t.Fatalf("the retry must not be absorbed as a duplicate: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(eventsDir, "2026-10-21.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != 1 {
		t.Fatalf("lines = %d, body = %s", lines, data)
	}
}

func TestAppendAfterCloseIsRefused(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	log, err := upstream.NewLog(upstream.LogOptions{
		Dir: dir, Logger: logger(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := log.Append(event(now)); err == nil {
		t.Fatal("append after close must fail")
	}
}

func TestAppendNotifiesBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	log, err := upstream.NewLog(upstream.LogOptions{
		Dir: dir, Logger: logger(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new log: %v", err)
	}
	defer func() { _ = log.Close() }()
	notified := make(chan upstream.Event, 1)
	log.Subscribe(func(event upstream.Event) { notified <- event })
	if _, err := log.Append(event(now)); err != nil {
		t.Fatalf("append: %v", err)
	}
	select {
	case event := <-notified:
		if event.ID == "" || event.Subject.Key != "TEST-12" {
			t.Fatalf("unexpected event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("the subscriber was not called before Append returned")
	}
}
