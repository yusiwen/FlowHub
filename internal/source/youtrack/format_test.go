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

// TestIssueIDPrefixIsTheAdaptersConvention pins where the "TEST-11 -> TEST" rule
// lives. The router refuses to guess a project from an issue ID, so the adapter that
// knows the convention answers for itself — and a payload whose project object is
// missing entirely still routes.
func TestIssueIDPrefixIsTheAdaptersConvention(t *testing.T) {
	cases := map[string]string{
		"TEST-11":   "TEST",
		"2-123":     "2",
		"SP-123":    "SP",
		"no-dash":   "no",
		"-leading":  "",
		"":          "",
		"trailing-": "trailing",
	}
	for id, want := range cases {
		if got := issueIDPrefix(id); got != want {
			t.Errorf("issueIDPrefix(%q) = %q, want %q", id, got, want)
		}
	}

	// A payload with no project object at all still yields the project, from the ID.
	payload, _, err := Parse([]byte(`{"event":"issueCreated","id":"TEST-12"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := payload.ProjectKey(); got != "TEST" {
		t.Fatalf("ProjectKey with no project object = %q, want TEST", got)
	}
	// The payload's own project wins over the prefix when the two disagree.
	payload, _, err = Parse([]byte(`{"event":"issueCreated","id":"TEST-12","project":{"key":"OTHER"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := payload.ProjectKey(); got != "OTHER" {
		t.Fatalf("ProjectKey = %q, want the payload's own key", got)
	}
	// ShortName is the last resort before the prefix.
	payload, _, err = Parse([]byte(`{"event":"issueCreated","id":"TEST-12","project":{"shortName":"SHORT"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := payload.ProjectKey(); got != "SHORT" {
		t.Fatalf("ProjectKey = %q, want the short name", got)
	}
}
