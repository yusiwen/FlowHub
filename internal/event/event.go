// Package event is the neutral intermediate representation that every event source
// decodes into and that the rest of the core consumes.
//
// The point of the type is that nothing downstream has to know which tracker the
// event came from: routing, the trigger policy, the audit decisions and the reply
// checks are all written against this, and a source's own payload stays inside its
// adapter. ADR 0001 chose the in-process seam over plugins, so this is a Go type
// rather than a wire format — but it is deliberately data-only: no methods that do
// I/O, no vendor names, no behaviour a source could use to reach around the
// dispatcher.
package event

import (
	"fmt"
	"strings"
	"time"
)

// Kind is what happened, in the source's terms reduced to the handful of shapes
// FlowHub acts on.
//
// `updated` and `other` exist so an adapter never has to lie: a source that cannot
// classify an event says `other` and the policy ignores it, which is what happens
// to YouTrack's work-item and attachment events today.
type Kind string

const (
	// KindCreated: the subject itself was created.
	KindCreated Kind = "created"
	// KindCommented: a comment was added or edited. Comment carries the text.
	KindCommented Kind = "commented"
	// KindStateSet: a workflow state was set. State carries the new value.
	KindStateSet Kind = "state_set"
	// KindUpdated: something else about the subject changed.
	KindUpdated Kind = "updated"
	// KindDeleted: the subject was deleted.
	KindDeleted Kind = "deleted"
	// KindOther: the source knows the event but FlowHub has no action for it.
	KindOther Kind = "other"
)

// Attachment is one file attached to the subject. The URL is whatever the source
// offers; the dispatcher constrains where it may be downloaded to, not the source.
type Attachment struct {
	Name  string
	URL   string
	MIME  string
	Bytes int64
}

// Subject is what the event is about.
type Subject struct {
	// Project is the routing key as the source spells it. It is matched against
	// the routing table, never guessed from Key.
	Project string
	// Key is the stable task key, e.g. "TEST-17". One task, one worktree, one
	// session is keyed on this.
	Key string
	// Title and Body are the human text, copied from the event for the prompt.
	// The prompt says out loud that they can be stale: the live subject is the
	// source of truth.
	Title string
	Body  string
	// URL is a human link to the subject, for the prompt and the reply.
	URL string
	// Attachments is what the event carried, if anything.
	Attachments []Attachment
}

// Event is one decoded delivery.
type Event struct {
	// Source names the adapter that produced this, e.g. "youtrack".
	Source string
	Kind   Kind
	// Subject is what the event is about. Key is required: everything downstream
	// is keyed on it.
	Subject Subject
	// Actor is the login whose action caused the event, when the source knows it.
	// The no-actor events exist (a deletion), and the per-project allowlist is
	// written so that they can never pass it.
	Actor string
	// State is the new workflow state, already normalised by the adapter, and
	// StateField names the field that carried it for the log. The policy compares
	// State against the values that mean "start implementing"; it never has to
	// know which field the tracker uses.
	State      string
	StateField string
	// Comment is the comment text for KindCommented, and empty otherwise.
	Comment string
	// Occurred is when the source says it happened. It is not trusted for
	// ordering, only for the replay window and the audit record.
	Occurred time.Time
	// Raw is the body as it arrived, kept for the audit. Nothing re-parses it
	// except the dispatcher, which needs the same event again.
	Raw []byte
}

// Validate reports whether the event is usable. An adapter produces this, so a
// failure means the adapter is broken rather than the input being hostile — which
// is why the dispatcher logs it loudly instead of rejecting quietly.
func (e *Event) Validate() error {
	if e == nil {
		return fmt.Errorf("event is nil")
	}
	if strings.TrimSpace(e.Source) == "" {
		return fmt.Errorf("event has no source")
	}
	if strings.TrimSpace(e.Subject.Key) == "" {
		return fmt.Errorf("event has no subject key")
	}
	if strings.TrimSpace(string(e.Kind)) == "" {
		return fmt.Errorf("event has no kind")
	}
	return nil
}

// IsComment reports whether the event is a comment, which is the shape the trigger
// phrase is looked for in.
func (e *Event) IsComment() bool { return e != nil && e.Kind == KindCommented }
