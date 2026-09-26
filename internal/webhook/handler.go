// Package webhook implements the public YouTrack webhook entry point.
//
// It owns the transport: the three entry locks, the body limit, the replay
// window, the local idempotency cache, the audit record and the redaction. What a
// delivery *means* belongs to the event source (internal/source), which decodes
// the body and supplies its own audit facts. The receiver is deliberately a pure
// receiver: it never calls YouTrack, opencode or any other network service on the
// request path, because the published Webhook Triggers app delivers synchronously
// with a 5s timeout and no retry.
package webhook

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/dedupe"
	"github.com/yusiwen/flowhub/internal/metrics"
	"github.com/yusiwen/flowhub/internal/source"
	"github.com/yusiwen/flowhub/internal/store"
)

// Rejection reasons. They are stable identifiers: they end up in the audit log
// and in the /healthz counters, so they are never renamed casually.
const (
	ReasonBadURLKey             = "bad_url_key"
	ReasonBadHeaderToken        = "bad_header_token"
	ReasonSourceNotAllowed      = "source_not_allowed"
	ReasonMethodNotAllowed      = "method_not_allowed"
	ReasonBodyTooLarge          = "body_too_large"
	ReasonBodyReadError         = "body_read_error"
	ReasonUnexpectedContentType = "unexpected_content_type"
	ReasonInvalidJSON           = "invalid_json"
	ReasonUnknownEvent          = "unknown_event"
	ReasonUnparseableTimestamp  = "unparseable_timestamp"
	ReasonOutsideReplayWindow   = "outside_replay_window"
	ReasonDuplicate             = "duplicate"
)

// AllReasons lists every stable rejection label, in evaluation order.
var AllReasons = []string{
	ReasonMethodNotAllowed,
	ReasonBadURLKey,
	ReasonBadHeaderToken,
	ReasonSourceNotAllowed,
	ReasonBodyTooLarge,
	ReasonBodyReadError,
	ReasonUnexpectedContentType,
	ReasonInvalidJSON,
	ReasonUnknownEvent,
	ReasonUnparseableTimestamp,
	ReasonOutsideReplayWindow,
	ReasonDuplicate,
}

// IsKnownReason reports whether reason is one of the stable rejection labels.
func IsKnownReason(reason string) bool {
	for _, candidate := range AllReasons {
		if candidate == reason {
			return true
		}
	}
	return false
}

// Options is the receiver half of the configuration.
type Options struct {
	// HookPath is the base path the handler is mounted on, without a trailing
	// slash. It is what makes "<base>/<key>" recognisable, so the key can be
	// redacted by position rather than by matching the configured value.
	HookPath string
	// HookKey is the URL embedded secret (lock 1); empty disables the lock.
	HookKey string
	// TokenHeader is the header name carrying the shared token (lock 2).
	TokenHeader string
	// Token is the expected header value; empty disables the lock.
	Token string
	// AllowedSources restricts accepted source IPs; empty disables the lock.
	AllowedSources []*net.IPNet
	// MaxBodyBytes caps the request body.
	MaxBodyBytes int64
	// ReplayWindow rejects payloads older or newer than this. Zero disables it.
	ReplayWindow time.Duration
	// LogHeaders records the received headers (names and values, with sensitive
	// values masked) plus a token fingerprint, in the audit log. It is what makes
	// the real wire format — including anything nginx adds — inspectable.
	LogHeaders bool
	// Dispatcher, when set, is handed every accepted delivery after it has been
	// audited. It is the phase-2 bridge to opencode and it must not block: the
	// publisher waits for our response.
	Dispatcher Dispatcher
	// Source decodes accepted deliveries. It is the seam ADR 0001 added: the
	// receiver owns the transport locks, and the adapter owns what a delivery from
	// *its* tracker means. Required.
	Source source.Source
}

// Dispatcher receives accepted deliveries for asynchronous work.
//
// The interface lives here rather than in the dispatch package so the webhook
// pipeline keeps depending on nothing but its own types.
type Dispatcher interface {
	Dispatch(rec *store.Record)
}

// AuditSink is the destination for audit records. Implementations must not
// block: this runs on the YouTrack delivery path.
type AuditSink interface {
	Record(rec *store.Record)
}

// Handler serves POST /<hook path>/<url key>.
type Handler struct {
	opts  Options
	sink  AuditSink
	dd    *dedupe.Cache
	log   *slog.Logger
	stats *metrics.Counters
	now   func() time.Time

	activeLocks []string
	// expectedTokenSHA is the fingerprint of the configured token, used to tell
	// "the app sent the real token" from "the app sent the literal secret".
	expectedTokenSHA string
}

// New builds a receiver handler.
func New(opts Options, sink AuditSink, dd *dedupe.Cache, log *slog.Logger, stats *metrics.Counters) *Handler {
	h := &Handler{opts: opts, sink: sink, dd: dd, log: log, stats: stats, now: time.Now}
	if opts.HookKey != "" {
		h.activeLocks = append(h.activeLocks, "url_key")
	}
	if opts.Token != "" {
		h.activeLocks = append(h.activeLocks, "header_token")
		h.expectedTokenSHA = shortSHA256(opts.Token)
	}
	if len(opts.AllowedSources) > 0 {
		h.activeLocks = append(h.activeLocks, "source_ip")
	}
	return h
}

// ServeHTTP implements the delivery pipeline:
//
//	three entry locks -> body limits -> content type -> JSON -> known event ->
//	replay window -> idempotency -> audit + 202
//
// Every failure returns the same empty response body as a success, so a scanner
// learns nothing from the status code. The reason is only written to the local
// audit log.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := h.now()
	h.stats.Received.Add(1)

	rec := &store.Record{
		Kind:         "webhook",
		Time:         start.UTC(),
		ActiveLocks:  h.activeLocks,
		Method:       r.Method,
		Path:         h.redactPath(r.URL.Path),
		Query:        h.redactQuery(r.URL.RawQuery),
		RemoteIP:     remoteIP(r),
		ForwardedFor: r.Header.Get("X-Forwarded-For"),
		RealIP:       r.Header.Get("X-Real-IP"),
		UserAgent:    r.UserAgent(),
		ContentType:  r.Header.Get("Content-Type"),
	}
	if h.opts.LogHeaders {
		rec.HeaderNames = headerNames(r.Header)
		rec.Headers = maskHeaders(r.Header, h.opts.TokenHeader)
		rec.TokenHeader = tokenFingerprint(r.Header, h.opts.TokenHeader, h.expectedTokenSHA)
	}

	if r.Method != http.MethodPost {
		h.respond(w, rec, ReasonMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	// Lock 1: URL embedded key.
	key := r.PathValue("key")
	if key == "" {
		key = r.URL.Query().Get("k")
	}
	if h.opts.HookKey != "" {
		if !constantTimeEqual(key, h.opts.HookKey) {
			h.respond(w, rec, ReasonBadURLKey, http.StatusAccepted)
			return
		}
		rec.Locks.URLKey = true
	}

	// Lock 2: shared token header.
	if h.opts.Token != "" {
		if !constantTimeEqual(r.Header.Get(h.opts.TokenHeader), h.opts.Token) {
			h.respond(w, rec, ReasonBadHeaderToken, http.StatusAccepted)
			return
		}
		rec.Locks.HeaderToken = true
	}

	// Lock 3: source address.
	if len(h.opts.AllowedSources) > 0 {
		ip := net.ParseIP(rec.RemoteIP)
		if ip == nil || !ipInAny(ip, h.opts.AllowedSources) {
			h.respond(w, rec, ReasonSourceNotAllowed, http.StatusAccepted)
			return
		}
		rec.Locks.SourceIP = true
	}

	body, err := h.readBody(w, r)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			h.respond(w, rec, ReasonBodyTooLarge, http.StatusRequestEntityTooLarge)
			return
		}
		h.log.Warn("reading webhook body failed", "error", err, "remote_ip", rec.RemoteIP)
		h.respond(w, rec, ReasonBodyReadError, http.StatusAccepted)
		return
	}
	rec.BodyBytes = len(body)
	rec.BodySHA256 = sha256Hex(body)
	rec.RawBody = string(body)

	if ct := mediaType(rec.ContentType); ct != "" && ct != "application/json" {
		h.respond(w, rec, ReasonUnexpectedContentType, http.StatusAccepted)
		return
	}

	decoded, err := h.opts.Source.Parse(&source.Request{
		Method: rec.Method, Path: rec.Path, Query: rec.Query,
		Headers: r.Header, RemoteIP: rec.RemoteIP, UserAgent: rec.UserAgent,
		Body: body,
	})
	if err != nil {
		h.respond(w, rec, ReasonInvalidJSON, http.StatusAccepted)
		return
	}
	h.describe(rec, decoded)

	if !rec.KnownEvent {
		h.respond(w, rec, ReasonUnknownEvent, http.StatusAccepted)
		return
	}

	if h.opts.ReplayWindow > 0 {
		if decoded.Occurred.IsZero() {
			// Only the adapter knows the timestamp formats, so an unparseable one
			// arrives as a zero time rather than as an error.
			h.respond(w, rec, ReasonUnparseableTimestamp, http.StatusAccepted)
			return
		}
		skew := start.Sub(decoded.Occurred)
		skewMS := skew.Milliseconds()
		rec.TimestampSkewMS = &skewMS
		if skew < 0 {
			skew = -skew
		}
		if skew > h.opts.ReplayWindow {
			h.respond(w, rec, ReasonOutsideReplayWindow, http.StatusAccepted)
			return
		}
	}

	dedupeKey := DedupeKey(rec.Event, rec.IssueID, rec.PayloadTimestamp, rec.BodySHA256)
	rec.DedupeKey = dedupeKey
	if h.dd.CheckAndAdd(dedupeKey) {
		h.respond(w, rec, ReasonDuplicate, http.StatusAccepted)
		return
	}

	h.respond(w, rec, "", http.StatusAccepted)
}

// describe fills the record from what the source decoded.
//
// The neutral part of the record comes from the event; the rest is the source's
// own audit material. ADR 0001 deliberately defers a generic audit schema until a
// second source exists, so this is the one place that knows a YouTrack-shaped
// field by name — and the shape it knows is the record's, unchanged.
func (h *Handler) describe(rec *store.Record, decoded source.Decoded) {
	e := decoded.Event
	if e != nil {
		rec.IssueID = e.Subject.Key
		rec.ProjectKey = e.Subject.Project
		rec.Summary = e.Subject.Title
		rec.PrimaryActor = e.Actor
	}
	rec.Event = decoded.Name
	rec.KnownEvent = decoded.Known
	rec.IssueIDForm = decoded.IssueIDForm
	rec.ActorLogins = decoded.ActorLogins
	// The audit record keeps the source's own spelling of the timestamp: it is what
	// makes a clock-skew or wire-format question answerable later, and it is part of
	// the idempotency key.
	rec.PayloadTimestamp = decoded.TimestampRaw
	rec.PayloadKeys = decoded.PayloadKeys
	rec.HasNumberInProject = decoded.NumberInProjectPresent
	rec.ChangedFields = decoded.ChangedFields
	rec.CommentIDs = decoded.CommentIDs
	rec.PayloadSchema = decoded.PayloadSchema
}

// respond finalizes the record, hands it to the (non-blocking) audit queue and
// writes the HTTP status.
func (h *Handler) respond(w http.ResponseWriter, rec *store.Record, reason string, status int) {
	rec.HandledMS = h.now().Sub(rec.Time).Milliseconds()
	if reason == "" {
		rec.Accepted = true
		h.stats.Accepted.Add(1)
	} else {
		rec.Reason = reason
		rec.Duplicate = reason == ReasonDuplicate
		if rec.Duplicate {
			h.stats.Duplicates.Add(1)
		}
		h.stats.Reject(reason)
	}

	h.sink.Record(rec)

	attrs := []any{
		"status", status,
		"accepted", rec.Accepted,
		"event", rec.Event,
		"issue_id", rec.IssueID,
		"project", rec.ProjectKey,
		"actor", rec.PrimaryActor,
		"remote_ip", rec.RemoteIP,
		"handled_ms", rec.HandledMS,
	}
	if !rec.Accepted {
		attrs = append(attrs, "reason", rec.Reason)
		h.log.Warn("webhook rejected", attrs...)
	} else {
		h.log.Info("webhook accepted", attrs...)
	}

	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)

	// Dispatch only after the answer is on the wire, so nothing in the agent
	// path can delay or fail the publisher. Rejected and duplicate deliveries
	// are never dispatched: a duplicate was already handled by the delivery
	// that created the key, and answering a rejection 202 is not a request for
	// work.
	if rec.Accepted && h.opts.Dispatcher != nil {
		h.opts.Dispatcher.Dispatch(rec)
	}
}

var errBodyTooLarge = errors.New("body too large")

func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.opts.MaxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errBodyTooLarge
		}
		return nil, err
	}
	return body, nil
}

// redact removes the URL embedded key from a path or query string before it
// reaches the log or the audit file.
// redactPath removes the URL embedded secret from a request path.
//
// It redacts by *shape*, never by matching the configured key. Matching the
// configured value was the original implementation and it leaked: the audit log
// is written for exactly the deliveries whose key did not match, so a receiver
// started with a rotated or freshly generated key wrote the app's real key to
// `data/webhook-*.jsonl` in plaintext. Measured 2026-09-21: a probe instance
// with its own random key recovered the live key of the running deployment from
// its own audit record. The route is `<hook path>/<key>`, so the position is
// enough.
func (h *Handler) redactPath(path string) string {
	if path == "" {
		return path
	}
	base := strings.TrimSuffix(h.opts.HookPath, "/")
	if base != "" && (path == base || strings.HasPrefix(path, base+"/")) {
		rest := strings.TrimPrefix(strings.TrimPrefix(path, base), "/")
		if rest == "" {
			// The base path itself carries no secret.
			return path
		}
		// Only the first segment after the base is the key; anything after it is
		// a suffix the app never sends, and keeping it makes a probe visible.
		tail := ""
		if cut := strings.Index(rest, "/"); cut >= 0 {
			tail = rest[cut:]
		}
		return base + "/***" + tail
	}
	// An unexpected shape: a scanner, or a deployment whose hook path moved.
	// Redacting the last segment costs nothing and still covers a key that
	// arrived through a path this handler does not recognise.
	// An empty HookPath (a caller that did not say where it is mounted) lands
	// here too. For the documented route that is still correct:
	// /hooks/youtrack/<key> becomes /hooks/youtrack/***.
	last := strings.LastIndex(path, "/")
	if last < 0 || last == len(path)-1 {
		return path
	}
	return path[:last+1] + "***"
}

// redactQuery removes the URL embedded secret from a raw query string. The key
// travels as `k`, and the value is redacted whatever it is; see redactPath for
// why the configured value must not be the thing being matched.
func (h *Handler) redactQuery(query string) string {
	if query == "" {
		return query
	}
	parts := strings.Split(query, "&")
	for i, part := range parts {
		name, _, found := strings.Cut(part, "=")
		if !found || name != "k" {
			continue
		}
		parts[i] = name + "=***"
	}
	return strings.Join(parts, "&")
}

// DedupeKey fingerprints one delivery. The app sends no delivery id and never
// retries, so the key is derived from the event identity plus the exact body.
func DedupeKey(event, issueID, timestamp, bodySHA256 string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{event, issueID, timestamp, bodySHA256}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// constantTimeEqual compares two secrets without leaking their length through
// timing.
func constantTimeEqual(a, b string) bool {
	left := sha256.Sum256([]byte(a))
	right := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func ipInAny(ip net.IP, networks []*net.IPNet) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func mediaType(contentType string) string {
	if contentType == "" {
		return ""
	}
	value, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(value))
}

func headerNames(header http.Header) []string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// tokenFingerprint describes the token header without storing the secret. The
// released app may put the literal string "secret" on the wire, so the
// fingerprint records enough to answer that question durably:
//
//   - MatchesConfigured compares the received value against the configured token
//     without revealing either;
//   - LooksLikeLiteralSecret flags the known platform bug.
func tokenFingerprint(header http.Header, name, expectedSHA string) *store.TokenHeaderInfo {
	if name == "" {
		return nil
	}
	values, present := header[http.CanonicalHeaderKey(name)]
	info := &store.TokenHeaderInfo{Name: name, Present: present}
	if present && len(values) > 0 {
		value := values[0]
		info.ValueLen = len(value)
		info.SHA256Prefix = shortSHA256(value)
		info.LooksLikeLiteralSecret = value == "secret"
		info.MatchesConfigured = expectedSHA != "" && info.SHA256Prefix == expectedSHA
	}
	return info
}

// sensitiveHeaders are masked in the audit log even though only the token header
// is expected: a reverse proxy or future change should not be able to leak a
// credential into the log files through an unanticipated header.
var sensitiveHeaders = map[string]struct{}{
	"Authorization":       {},
	"Proxy-Authorization": {},
	"Cookie":              {},
	"Set-Cookie":          {},
}

// maskHeaders copies the request headers, replacing sensitive values with a
// non-reversible fingerprint. The received format stays visible (which headers
// nginx adds, what Content-Type and User-Agent look like) without writing a
// credential to disk.
func maskHeaders(header http.Header, tokenHeader string) map[string][]string {
	if len(header) == 0 {
		return nil
	}
	tokenName := ""
	if tokenHeader != "" {
		tokenName = http.CanonicalHeaderKey(tokenHeader)
	}

	masked := make(map[string][]string, len(header))
	for name, values := range header {
		canonical := http.CanonicalHeaderKey(name)
		_, sensitive := sensitiveHeaders[canonical]
		if canonical != tokenName && !sensitive {
			masked[canonical] = append([]string(nil), values...)
			continue
		}
		maskedValues := make([]string, 0, len(values))
		for _, value := range values {
			maskedValues = append(maskedValues, maskedValue(value))
		}
		masked[canonical] = maskedValues
	}
	return masked
}

// maskedValue renders a credential as a fingerprint that cannot be reversed.
func maskedValue(value string) string {
	return fmt.Sprintf("<masked len=%d sha256:%s>", len(value), shortSHA256(value))
}

// shortSHA256 is the 12 hex character fingerprint used to compare secrets without
// storing them.
func shortSHA256(value string) string {
	return sha256Hex([]byte(value))[:12]
}
