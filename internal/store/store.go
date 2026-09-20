// Package store persists the webhook audit trail.
//
// Phase 1 of FlowHub records every delivery (accepted or not) as one
// append-only JSON line, which keeps the receiver dependency free while the real
// payload/header/source-IP questions from the design documents are still open.
// A SQLite implementation can replace JSONL later: both satisfy Recorder.
package store

import (
	"time"
)

// Recorder is the sink for audit records.
type Recorder interface {
	// Record persists one audit record. Implementations must be safe for
	// concurrent use.
	Record(rec *Record) error
	// Close flushes and releases resources.
	Close() error
}

// Locks records which entry locks were satisfied by a delivery. Locks that are
// not configured are reported in Record.ActiveLocks and stay false here.
type Locks struct {
	URLKey      bool `json:"url_key"`
	HeaderToken bool `json:"header_token"`
	SourceIP    bool `json:"source_ip"`
}

// TokenHeaderInfo is the safe fingerprint of the shared token header. The raw
// value is deliberately never stored: the published app may send the literal
// string "secret" instead of the configured token, and that question is answered
// by ValueLen, SHA256Prefix and MatchesConfigured without keeping a secret at
// rest.
type TokenHeaderInfo struct {
	Name                   string `json:"name"`
	Present                bool   `json:"present"`
	ValueLen               int    `json:"value_len"`
	SHA256Prefix           string `json:"sha256_prefix,omitempty"`
	MatchesConfigured      bool   `json:"matches_configured"`
	LooksLikeLiteralSecret bool   `json:"looks_like_literal_secret"`
}

// Record is one audited webhook delivery.
type Record struct {
	Kind string `json:"kind"`

	// Time is when the handler started processing the delivery.
	Time time.Time `json:"ts"`

	// Accepted reports whether the delivery passed every stage.
	Accepted bool `json:"accepted"`

	// Reason is the stable rejection label; empty when Accepted.
	Reason string `json:"reason,omitempty"`

	// Duplicate marks a delivery dropped by the idempotency cache.
	Duplicate bool `json:"duplicate,omitempty"`

	// ActiveLocks lists the entry locks configured at receive time, so records
	// stay interpretable after the configuration changes.
	ActiveLocks []string `json:"active_locks"`
	Locks       Locks    `json:"locks"`

	// Request metadata. Path and Query are redacted: the URL embedded key is a
	// secret and nginx is configured not to log it either.
	Method       string `json:"method"`
	Path         string `json:"path"`
	Query        string `json:"query,omitempty"`
	RemoteIP     string `json:"remote_ip"`
	ForwardedFor string `json:"forwarded_for,omitempty"`
	RealIP       string `json:"x_real_ip,omitempty"`
	UserAgent    string `json:"user_agent,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	BodyBytes    int    `json:"body_bytes"`
	BodySHA256   string `json:"body_sha256,omitempty"`
	HandledMS    int64  `json:"handled_ms"`

	// HeaderNames, Headers and TokenHeader are filled when header logging is
	// enabled. Headers holds the received headers with sensitive values masked,
	// so the real wire format (including what nginx forwards) can be inspected
	// without writing a secret to disk.
	HeaderNames []string            `json:"header_names,omitempty"`
	Headers     map[string][]string `json:"headers,omitempty"`
	TokenHeader *TokenHeaderInfo    `json:"token_header,omitempty"`

	// Payload facts, derived leniently from the body.
	Event              string   `json:"event,omitempty"`
	KnownEvent         bool     `json:"known_event"`
	IssueID            string   `json:"issue_id,omitempty"`
	IssueIDForm        string   `json:"issue_id_form,omitempty"`
	ProjectKey         string   `json:"project_key,omitempty"`
	Summary            string   `json:"summary,omitempty"`
	PrimaryActor       string   `json:"primary_actor,omitempty"`
	ActorLogins        []string `json:"actor_logins,omitempty"`
	PayloadTimestamp   string   `json:"payload_timestamp,omitempty"`
	TimestampSkewMS    *int64   `json:"timestamp_skew_ms,omitempty"`
	PayloadKeys        []string `json:"payload_keys,omitempty"`
	PayloadSchema      []string `json:"payload_schema,omitempty"`
	HasNumberInProject bool     `json:"has_number_in_project"`
	ChangedFields      []string `json:"changed_fields,omitempty"`
	CommentIDs         []string `json:"comment_ids,omitempty"`
	DedupeKey          string   `json:"dedupe_key,omitempty"`

	// RawBody is the unmodified request body. The design documents require the
	// original payload for audit and replay; the size is bounded by
	// FLOWHUB_MAX_BODY_BYTES.
	RawBody string `json:"raw_body,omitempty"`
}
