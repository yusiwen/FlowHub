package youtrack_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/rules"
	"github.com/yusiwen/flowhub/internal/source"
	"github.com/yusiwen/flowhub/internal/source/youtrack"
	"github.com/yusiwen/flowhub/internal/upstream"
)

const commentBody = `{"event":"commentAdded","timestamp":"2026-10-21T09:00:00.000Z","id":"2-123",` +
	`"summary":"Fix login","project":{"key":"SP","name":"Sample Project","shortName":"SP"},` +
	`"comments":[{"id":"4-14","text":"/opencode start","created":1732708800000,` +
	`"author":{"login":"john.doe","fullName":"John Doe"}}]}`

const updatedBody = `{"event":"issueUpdated","timestamp":"2026-10-21T09:05:00.000Z","id":"2-123",` +
	`"summary":"Fix login","project":{"key":"SP","name":"Sample Project","shortName":"SP"},` +
	`"updatedBy":{"login":"jane.doe"},` +
	`"changedFields":[{"name":"State","oldValue":{"name":"Open"},` +
	`"value":{"name":"In Progress"}}]}`

const createdBody = `{"event":"issueCreated","timestamp":"2026-10-21T09:10:00.000Z","id":"2-124",` +
	`"summary":"New issue","project":{"key":"SP","shortName":"SP"},` +
	`"reporter":{"login":"john.doe"}}`

func normalize(t *testing.T, body string) upstream.Event {
	t.Helper()
	adapter := youtrack.New(rules.Policy{}, "")
	request := &source.Request{Body: []byte(body), RemoteIP: "127.0.0.1"}
	decoded, err := adapter.Parse(request)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	event, err := adapter.Normalize(decoded, request, time.Date(2026, 10, 21, 9, 0, 1, 0, time.UTC))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return event
}

func TestNormalizeComment(t *testing.T) {
	event := normalize(t, commentBody)
	if event.Kind != upstream.KindCommentAdded {
		t.Fatalf("kind = %q", event.Kind)
	}
	if event.Subject.Key != "2-123" || event.Subject.Project != "SP" {
		t.Fatalf("subject = %+v", event.Subject)
	}
	if event.Comment == nil || event.Comment.Text != "/opencode start" || event.Comment.Author != "john.doe" {
		t.Fatalf("comment = %+v", event.Comment)
	}
	if event.Origin != "upstream:youtrack" || !strings.HasPrefix(event.DedupeKey, "sha256:") {
		t.Fatalf("origin/dedupe = %q %q", event.Origin, event.DedupeKey)
	}
	if !event.OccurredAt.Equal(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("occurred = %s", event.OccurredAt)
	}
	if event.Trace["remote_ip"] != "127.0.0.1" {
		t.Fatalf("trace = %+v", event.Trace)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
}

func TestNormalizeDedupeIsStableAcrossWireChanges(t *testing.T) {
	first := normalize(t, commentBody)
	changed := strings.Replace(commentBody, `"summary":"Fix login"`, `"summary":"Fix login","description":"different wire field"`, 1)
	second := normalize(t, changed)
	if first.DedupeKey != second.DedupeKey {
		t.Fatalf("dedupe changed with an unrelated field: %q != %q", first.DedupeKey, second.DedupeKey)
	}
}

func TestNormalizeStateChange(t *testing.T) {
	event := normalize(t, updatedBody)
	if event.Kind != upstream.KindIssueUpdated {
		t.Fatalf("kind = %q", event.Kind)
	}
	if len(event.Changes) != 1 || event.Changes[0].Field != "State" || event.Changes[0].To != "In Progress" {
		t.Fatalf("changes = %+v", event.Changes)
	}
	if event.Actor.Login != "jane.doe" {
		t.Fatalf("actor = %+v", event.Actor)
	}
}

func TestNormalizeCreated(t *testing.T) {
	event := normalize(t, createdBody)
	if event.Kind != upstream.KindIssueCreated {
		t.Fatalf("kind = %q", event.Kind)
	}
	if event.Subject.Key != "2-124" {
		t.Fatalf("subject = %+v", event.Subject)
	}
}

func TestIssueUpdateDedupeIncludesChangedContent(t *testing.T) {
	first := normalize(t, updatedBody)
	changed := strings.Replace(updatedBody, `"value":{"name":"In Progress"}}]}`, `"value":{"name":"In Progress"},"description":"second"}]}`, 1)
	if changed == updatedBody {
		t.Fatal("test body did not change")
	}
	second := normalize(t, changed)
	if first.DedupeKey == second.DedupeKey {
		t.Fatalf("different issue updates share a dedupe key: %q", first.DedupeKey)
	}
}
