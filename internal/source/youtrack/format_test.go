package youtrack

import (
	"strings"
	"testing"
)

func TestIssueIDForm(t *testing.T) {
	cases := map[string]string{
		"2-123":  "database",
		"SP-123": "readable",
		"":       "",
		"weird":  "unknown",
	}
	for id, want := range cases {
		if got := IssueIDForm(id); got != want {
			t.Errorf("IssueIDForm(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestParseTimestampFormats(t *testing.T) {
	if _, err := ParseTimestamp("2026-09-19T12:00:00.000Z"); err != nil {
		t.Fatalf("RFC3339 with millis: %v", err)
	}
	if _, err := ParseTimestamp("2026-09-19T12:00:00Z"); err != nil {
		t.Fatalf("RFC3339: %v", err)
	}
	ms, err := ParseTimestamp("1732708800000")
	if err != nil {
		t.Fatalf("epoch millis: %v", err)
	}
	if ms.UnixMilli() != 1732708800000 {
		t.Fatalf("epoch millis parsed as %d", ms.UnixMilli())
	}
	if _, err := ParseTimestamp("not-a-time"); err == nil {
		t.Fatal("garbage timestamp must fail")
	}
}

func TestKnownEventsCoverThePublishedApp(t *testing.T) {
	want := []string{
		"commentAdded", "commentDeleted", "commentUpdated",
		"issueAttachmentAdded", "issueAttachmentDeleted",
		"issueCreated", "issueDeleted", "issueUpdated",
		"workItemAdded", "workItemDeleted", "workItemUpdated",
	}
	got := KnownEventNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("known events = %v, want %v", got, want)
	}
}
