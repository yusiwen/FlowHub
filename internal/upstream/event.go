// Package upstream is the seam between an event source and the flow engine.
//
// An adapter decodes one source system's delivery into the neutral Event
// envelope; the log appends accepted events to a per-day JSONL file; the flow
// engine consumes events from the log. The envelope is deliberately independent of
// the tracker: the YouTrack adapter owns the payload shape, and nothing downstream
// of this package parses it again.
//
// The model is documented in docs/flow-model.md.
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Schema is the envelope version.
const Schema = 1

// Kind values for the builtin YouTrack flow. A source may use a different
// vocabulary; the flow matches strings.
const (
	KindIssueCreated    = "issue.created"
	KindIssueUpdated    = "issue.updated"
	KindIssueDeleted    = "issue.deleted"
	KindCommentAdded    = "comment.added"
	KindAttachmentAdded = "attachment.added"
)

// Subject is what the event is about.
type Subject struct {
	Type    string `json:"type"`
	Project string `json:"project,omitempty"`
	Key     string `json:"key"`
	Title   string `json:"title,omitempty"`
	URL     string `json:"url,omitempty"`
}

// Actor is the person whose action caused the event.
type Actor struct {
	Login   string `json:"login,omitempty"`
	Display string `json:"display,omitempty"`
}

// Change is one normalised field transition.
type Change struct {
	Field string `json:"field"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

// Comment is the comment text for a comment event.
type Comment struct {
	Text   string `json:"text"`
	Author string `json:"author,omitempty"`
}

// Event is the neutral envelope every ingress produces.
type Event struct {
	ID         string          `json:"id"`
	Schema     int             `json:"schema"`
	Upstream   string          `json:"upstream"`
	Kind       string          `json:"kind"`
	OccurredAt time.Time       `json:"occurred_at"`
	ReceivedAt time.Time       `json:"received_at"`
	Subject    Subject         `json:"subject"`
	Actor      Actor           `json:"actor"`
	Changes    []Change        `json:"changes,omitempty"`
	Comment    *Comment        `json:"comment,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	DedupeKey  string          `json:"dedupe_key"`
	Origin     string          `json:"origin"`
	Trace      map[string]any  `json:"trace,omitempty"`
}

// Validate reports whether an adapter produced a usable event. A failure is an
// adapter bug, not a hostile delivery: the webhook has already decided the
// delivery is acceptable.
func (e *Event) Validate() error {
	if e == nil {
		return fmt.Errorf("event is nil")
	}
	if e.Schema != Schema {
		return fmt.Errorf("event schema is %d, want %d", e.Schema, Schema)
	}
	if strings.TrimSpace(e.Upstream) == "" {
		return fmt.Errorf("event has no upstream")
	}
	if strings.TrimSpace(e.Kind) == "" {
		return fmt.Errorf("event has no kind")
	}
	if strings.TrimSpace(e.Subject.Key) == "" {
		return fmt.Errorf("event has no subject key")
	}
	if strings.TrimSpace(e.DedupeKey) == "" {
		return fmt.Errorf("event has no dedupe key")
	}
	if strings.TrimSpace(e.Origin) == "" {
		return fmt.Errorf("event has no origin")
	}
	if len(e.Payload) > 0 && !json.Valid(e.Payload) {
		return fmt.Errorf("event payload is not valid JSON")
	}
	return nil
}

// NewID returns a time-sortable event id. The random suffix keeps two events in
// the same nanosecond distinct without a database.
func NewID(now time.Time) string {
	var random [6]byte
	_, _ = rand.Read(random[:])
	return fmt.Sprintf("evt_%d_%s", now.UTC().UnixNano(), hex.EncodeToString(random[:]))
}

// Sink accepts events asynchronously. It is kept for tests and for callers that
// do not own the delivery path; the webhook uses DurableSink.
type Sink interface {
	Record(Event)
}

// DurableSink appends an event before the delivery is answered. Append must not
// return nil unless the event is on disk; no fsync is required, but a failed write
// must be reported so the webhook can answer 503 instead of claiming durability.
type DurableSink interface {
	Append(Event) error
}
