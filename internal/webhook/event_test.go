package webhook

import (
	"strings"
	"sync"
	"testing"

	"github.com/yusiwen/flowhub/internal/upstream"
)

type captureEvents struct {
	mu     sync.Mutex
	events []upstream.Event
}

func (c *captureEvents) Record(event upstream.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *captureEvents) all() []upstream.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]upstream.Event(nil), c.events...)
}

func TestAcceptedDeliveryRecordsOneEvent(t *testing.T) {
	sink := &captureEvents{}
	h := newHarness(t, func(opts *Options) { opts.Events = sink })
	resp := h.post(t, keyedPath(), testToken, issueUpdatedBody(freshTimestamp()))
	assertRejected(t, resp, "")

	events := sink.all()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Kind != upstream.KindIssueUpdated {
		t.Fatalf("kind = %q", event.Kind)
	}
	if event.Subject.Key != "2-123" || event.Subject.Project != "SP" {
		t.Fatalf("subject = %+v", event.Subject)
	}
	if event.Origin != "upstream:youtrack" || !strings.HasPrefix(event.DedupeKey, "sha256:") {
		t.Fatalf("origin/dedupe = %q %q", event.Origin, event.DedupeKey)
	}
	if event.ReceivedAt.IsZero() || event.ID != "" {
		t.Fatalf("received/id = %s %q (the log assigns the id)", event.ReceivedAt, event.ID)
	}
	if len(event.Changes) != 1 || event.Changes[0].Field != "State" || event.Changes[0].To != "In Progress" {
		t.Fatalf("changes = %+v", event.Changes)
	}
}

func TestCommentDeliveryRecordsTheComment(t *testing.T) {
	sink := &captureEvents{}
	h := newHarness(t, func(opts *Options) { opts.Events = sink })
	h.post(t, keyedPath(), testToken, commentAddedBody(freshTimestamp()))

	events := sink.all()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Kind != upstream.KindCommentAdded {
		t.Fatalf("kind = %q", events[0].Kind)
	}
	if events[0].Comment == nil || events[0].Comment.Text != "/opencode please look" {
		t.Fatalf("comment = %+v", events[0].Comment)
	}
}

func TestRejectedDeliveryRecordsNoEvent(t *testing.T) {
	sink := &captureEvents{}
	h := newHarness(t, func(opts *Options) { opts.Events = sink })
	h.post(t, keyedPath(), "wrong-token", issueUpdatedBody(freshTimestamp()))
	if events := sink.all(); len(events) != 0 {
		t.Fatalf("rejected delivery recorded %d events", len(events))
	}
}

func TestDuplicateDeliveryRecordsOneEvent(t *testing.T) {
	sink := &captureEvents{}
	h := newHarness(t, func(opts *Options) { opts.Events = sink })
	body := issueUpdatedBody(freshTimestamp())
	h.post(t, keyedPath(), testToken, body)
	h.post(t, keyedPath(), testToken, body)
	if events := sink.all(); len(events) != 1 {
		t.Fatalf("duplicate delivery recorded %d events", len(events))
	}
}
