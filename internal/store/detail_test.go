package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func detailRecord() *Record {
	skew := int64(1234)
	return &Record{
		Kind:      "webhook",
		Time:      time.Date(2026, 9, 20, 1, 2, 3, 0, time.UTC),
		Accepted:  true,
		Method:    "POST",
		Path:      "/hooks/youtrack/***",
		RemoteIP:  "10.8.0.2",
		RealIP:    "203.0.113.7",
		UserAgent: "YouTrack",
		Locks:     Locks{URLKey: true, HeaderToken: true},
		ActiveLocks: []string{
			"url_key", "header_token",
		},
		HandledMS:        1,
		Headers:          map[string][]string{"X-YouTrack-Token": {"<masked len=64 sha256:81e4c7ecf857>"}, "Content-Type": {"application/json"}},
		HeaderNames:      []string{"Content-Type", "X-YouTrack-Token"},
		TokenHeader:      &TokenHeaderInfo{Name: "X-YouTrack-Token", Present: true, ValueLen: 64, SHA256Prefix: "81e4c7ecf857", MatchesConfigured: true},
		Event:            "issueUpdated",
		KnownEvent:       true,
		IssueID:          "2-123",
		IssueIDForm:      "database",
		ProjectKey:       "SP",
		Summary:          "Fix login",
		PrimaryActor:     "jane.doe",
		ActorLogins:      []string{"jane.doe"},
		PayloadTimestamp: "2026-09-20T01:02:03.000Z",
		TimestampSkewMS:  &skew,
		PayloadKeys:      []string{"event", "id", "project"},
		PayloadSchema:    []string{"event: string", "id: string", "project.key: string"},
		ChangedFields:    []string{"State"},
		DedupeKey:        strings.Repeat("ab", 32),
		RawBody:          `{"event":"issueUpdated","id":"2-123","project":{"key":"SP"}}`,
	}
}

func TestDetailWritesReadableBlock(t *testing.T) {
	dir := t.TempDir()
	detail, err := OpenDetail(dir, 64<<10)
	if err != nil {
		t.Fatalf("OpenDetail: %v", err)
	}
	defer detail.Close()

	detail.now = func() time.Time { return time.Date(2026, 9, 20, 1, 2, 3, 0, time.UTC) }
	if err := detail.Record(detailRecord()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	path := detail.Path()
	if filepath.Base(path) != "payload-2026-09-20.log" {
		t.Fatalf("payload log = %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("payload log perm = %o, want 600", perm)
	}

	content := readAll(t, path)
	for _, want := range []string{
		"ACCEPTED",
		"event=issueUpdated",
		"source    remote=10.8.0.2",
		"x-real-ip=203.0.113.7",
		"url_key=ok  header_token=ok  source_ip=-",
		"(configured: url_key,header_token)",
		"clock_skew=1234ms",
		"issue     id=2-123  form=database  project=SP  summary=Fix login",
		"numberInProject_present=false",
		"matches_configured=true",
		"schema    (3 paths)",
		"project.key: string",
		"payload\n  {\n    \"event\": \"issueUpdated\",",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("payload log is missing %q:\n%s", want, content)
		}
	}
}

func TestDetailMarksRejectedDeliveries(t *testing.T) {
	dir := t.TempDir()
	detail, err := OpenDetail(dir, 64<<10)
	if err != nil {
		t.Fatalf("OpenDetail: %v", err)
	}
	defer detail.Close()

	rec := detailRecord()
	rec.Accepted = false
	rec.Reason = "bad_header_token"
	rec.Locks = Locks{URLKey: true}
	if err := detail.Record(rec); err != nil {
		t.Fatalf("Record: %v", err)
	}

	content := readAll(t, detail.Path())
	if !strings.Contains(content, "REJECTED (bad_header_token)") {
		t.Fatalf("verdict missing:\n%s", content)
	}
	if !strings.Contains(content, "source_ip=-") {
		t.Fatalf("unsatisfied lock must be visible:\n%s", content)
	}
}

func TestDetailTruncatesHugeBodiesButSaysSo(t *testing.T) {
	dir := t.TempDir()
	detail, err := OpenDetail(dir, 64)
	if err != nil {
		t.Fatalf("OpenDetail: %v", err)
	}
	defer detail.Close()

	rec := detailRecord()
	rec.RawBody = `{"event":"issueUpdated","padding":"` + strings.Repeat("x", 500) + `"}`
	if err := detail.Record(rec); err != nil {
		t.Fatalf("Record: %v", err)
	}

	content := readAll(t, detail.Path())
	if !strings.Contains(content, "truncated at 64 bytes") {
		t.Fatalf("truncation is not reported:\n%s", content)
	}
	if !strings.Contains(content, "the full body is in the JSONL audit record") {
		t.Fatalf("truncation hint is missing:\n%s", content)
	}
}

func TestDetailKeepsMalformedBodiesForInspection(t *testing.T) {
	dir := t.TempDir()
	detail, err := OpenDetail(dir, 64<<10)
	if err != nil {
		t.Fatalf("OpenDetail: %v", err)
	}
	defer detail.Close()

	rec := detailRecord()
	rec.Accepted = false
	rec.Reason = "invalid_json"
	rec.RawBody = `{"oops"`
	if err := detail.Record(rec); err != nil {
		t.Fatalf("Record: %v", err)
	}

	content := readAll(t, detail.Path())
	if !strings.Contains(content, `{"oops"`) || !strings.Contains(content, "(not valid JSON)") {
		t.Fatalf("malformed body must stay visible:\n%s", content)
	}
}

func TestMultiWritesToEverySinkAndReportsFailures(t *testing.T) {
	first := &slowRecorder{}
	second := &slowRecorder{}
	sink := NewMulti(first, second)

	if err := sink.Record(&Record{Kind: "webhook"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(first.records) != 1 || len(second.records) != 1 {
		t.Fatalf("first=%d second=%d, want both 1", len(first.records), len(second.records))
	}

	failing := NewMulti(&slowRecorder{fail: true}, second)
	if err := failing.Record(&Record{Kind: "webhook"}); err == nil {
		t.Fatal("a failing sink must surface an error")
	}
	if len(second.records) != 2 {
		t.Fatalf("second sink was skipped after a failure: %d", len(second.records))
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
