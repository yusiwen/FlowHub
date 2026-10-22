package youtrack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/source"
	"github.com/yusiwen/flowhub/internal/upstream"
)

// Normalize implements upstream.Normalizer: it turns one decoded YouTrack
// delivery into the neutral envelope the flow engine consumes.
//
// The audit record keeps the source's own facts; this envelope keeps only what a
// flow may read. The payload stays attached for replay and for the rare case that
// needs the raw shape, but a flow should prefer subject, actor, changes and
// comment.
func (s *Source) Normalize(decoded source.Decoded, request *source.Request, received time.Time) (upstream.Event, error) {
	e := decoded.Event
	if e == nil {
		return upstream.Event{}, fmt.Errorf("youtrack: delivery has no event")
	}
	envelope := upstream.Event{
		Schema:     upstream.Schema,
		Upstream:   SourceName,
		Kind:       normalizedKind(e.Kind),
		OccurredAt: decoded.Occurred,
		ReceivedAt: received,
		Subject: upstream.Subject{
			Type:    "issue",
			Project: e.Subject.Project,
			Key:     e.Subject.Key,
			Title:   e.Subject.Title,
			URL:     e.Subject.URL,
		},
		Actor:     upstream.Actor{Login: e.Actor},
		Origin:    "upstream:" + SourceName,
		DedupeKey: normalizedDedupeKey(e, decoded),
	}
	if e.State != "" {
		field := e.StateField
		if field == "" {
			field = "state"
		}
		envelope.Changes = append(envelope.Changes, upstream.Change{Field: field, To: e.State})
	}
	if e.IsComment() {
		envelope.Comment = &upstream.Comment{Text: e.Comment, Author: e.Actor}
	}
	if request != nil {
		if len(request.Body) > 0 && json.Valid(request.Body) {
			envelope.Payload = json.RawMessage(request.Body)
		}
		if request.RemoteIP != "" {
			envelope.Trace = map[string]any{"remote_ip": request.RemoteIP}
		}
	}
	if err := envelope.Validate(); err != nil {
		return upstream.Event{}, fmt.Errorf("youtrack: normalize: %w", err)
	}
	return envelope, nil
}

func normalizedKind(kind event.Kind) string {
	switch kind {
	case event.KindCreated:
		return upstream.KindIssueCreated
	case event.KindCommented:
		return upstream.KindCommentAdded
	case event.KindStateSet, event.KindUpdated:
		return upstream.KindIssueUpdated
	case event.KindDeleted:
		return upstream.KindIssueDeleted
	default:
		return "issue.other"
	}
}

// normalizedDedupeKey is stable across ingress modes: it uses the parsed instant,
// not the source's spelling, and does not include the raw body. YouTrack comments
// have no id, so the issue, time, actor and text are the identity.
func normalizedDedupeKey(e *event.Event, decoded source.Decoded) string {
	occurred := decoded.TimestampRaw
	if !decoded.Occurred.IsZero() {
		occurred = decoded.Occurred.UTC().Format(time.RFC3339Nano)
	}
	parts := []string{
		SourceName,
		string(e.Kind),
		e.Subject.Key,
		occurred,
		e.Actor,
		e.Comment,
		e.State,
	}
	if !e.IsComment() && len(e.Raw) > 0 {
		// An issue update carries its changed content in the body, not in the
		// Event fields. Without this, two legitimate updates in the same second
		// (different descriptions, same key/instant) collapse into one event.
		// A retried webhook still has the same body and is still deduped.
		parts = append(parts, string(e.Raw))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}
