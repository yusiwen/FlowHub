// Package source is the seam between "something happened in a tracker" and the
// rest of FlowHub.
//
// ADR 0001 chose in-process Go interfaces over plugins: a source is a package that
// decodes one tracker's deliveries into internal/event and supplies the policy,
// the prompt and the tool allowlist that belong to that tracker. The receiver
// keeps the transport locks, because every source deserves the same ones, and the
// dispatcher keeps the shell policy, because a source must not be able to widen
// what an unattended turn may execute.
package source

import (
	"net/http"
	"time"

	"github.com/yusiwen/flowhub/internal/event"
	"github.com/yusiwen/flowhub/internal/rules"
)

// Request is one delivery as the receiver saw it, after the entry locks passed.
type Request struct {
	Method    string
	Path      string
	Query     string
	Headers   http.Header
	RemoteIP  string
	UserAgent string
	Body      []byte
}

// ToolPolicy is data, not behaviour: the dispatcher performs the checks, so a
// source cannot smuggle logic into them.
type ToolPolicy struct {
	// Allowed lists the tools the agent may call without asking. Everything else
	// stays gated: the session ruleset asks and the arbiter decides.
	Allowed []string
	// Reply lists the tool calls that mean "this turn replied". A completed call to
	// any of them is what the dispatcher reports as the turn's answer.
	Reply []string
	// Download constrains attachment downloads: which hosts may be reached, and
	// where inside the worktree the file may be written.
	Download DownloadPolicy
}

// DownloadPolicy is the part of an attachment URL and path the arbiter enforces.
type DownloadPolicy struct {
	Hosts  []string
	Prefix string
}

// Decoded is one delivery decoded.
//
// It is deliberately wider than the neutral event: the receiver's audit record
// still carries source-shaped fields (ADR 0001 defers a generic audit schema until
// a second source exists), so the adapter hands those over as facts instead of
// pretending they are neutral.
type Decoded struct {
	Event *event.Event
	// Name is the source's own name for the event, kept for the audit record.
	Name string
	// Known reports whether Name is an event this source can act on. The receiver
	// rejects an unknown name before anything else happens.
	Known bool
	// Occurred is the event time as the source parsed it. The zero time means the
	// source could not read one: only the adapter knows the tracker's formats, and
	// the receiver turns a zero into its own rejection reason.
	Occurred time.Time
	// TimestampRaw is the timestamp exactly as it arrived. The audit record and the
	// idempotency key keep the source's own spelling, so re-rendering it as RFC 3339
	// would change the record shape and the dedupe key for an epoch-millis payload.
	TimestampRaw string
	// ActorLogins is every actor the event names, not only the primary one.
	ActorLogins []string
	// IssueIDForm is the source's own spelling of the key (YouTrack sends both a
	// readable and a database form), for the audit record.
	IssueIDForm string
	// NumberInProjectPresent records whether the payload carried that field at all,
	// which is how the "is it really absent?" question stays answerable.
	NumberInProjectPresent bool
	// ChangedFields is the names of the fields the event changed.
	ChangedFields []string
	// CommentIDs is the ids of the comments the event carried.
	CommentIDs []string
	// PayloadKeys is the top-level key set, and PayloadSchema the flat path: type
	// report. Both are payload-analysis aids: they are how the real wire format is
	// compared against the documented one.
	PayloadKeys   []string
	PayloadSchema []string
}

// Source is one tracker adapter.
type Source interface {
	// Name is the source's own name, e.g. "youtrack".
	Name() string
	// Parse verifies whatever the source can verify and decodes the delivery. The
	// three entry locks are the receiver's, not this: a source signature (a Gitea
	// HMAC, say) is the only thing Parse is responsible for.
	Parse(req *Request) (Decoded, error)
	// Policy is the trigger policy for this source: the phrase that means "start",
	// the state values that mean it, the markers that identify FlowHub's own
	// replies and the turn budget.
	Policy() rules.Policy
	// Tools is the tool allowlist and the reply-tool names for this source.
	Tools() ToolPolicy
	// Prompt writes one turn's instruction. It is the source's because the text
	// names the tracker's tools and its sign-off contract.
	Prompt(action rules.Action, e *event.Event, ctx rules.PromptContext) string
}
