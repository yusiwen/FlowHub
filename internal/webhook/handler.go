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
		Path:         h.redact(r.URL.Path),
		Query:        h.redact(r.URL.RawQuery),
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

	payload, keys, err := Parse(body)
	if err != nil {
		h.respond(w, rec, ReasonInvalidJSON, http.StatusAccepted)
		return
	}
	h.describe(rec, payload, keys, body)

	if !rec.KnownEvent {
		h.respond(w, rec, ReasonUnknownEvent, http.StatusAccepted)
		return
	}

	if h.opts.ReplayWindow > 0 {
		ts, err := ParseTimestamp(payload.Timestamp)
		if err != nil {
			h.respond(w, rec, ReasonUnparseableTimestamp, http.StatusAccepted)
			return
		}
		skew := start.Sub(ts)
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

	dedupeKey := DedupeKey(payload.Event, payload.ID, payload.Timestamp, rec.BodySHA256)
	rec.DedupeKey = dedupeKey
	if h.dd.CheckAndAdd(dedupeKey) {
		h.respond(w, rec, ReasonDuplicate, http.StatusAccepted)
		return
	}

	h.respond(w, rec, "", http.StatusAccepted)
}

// describe fills the payload derived fields of the record. The schema report is
// derived from the raw body so the real payload can be diffed against the
// documented one without a second look at the wire.
func (h *Handler) describe(rec *store.Record, payload *Payload, keys []string, body []byte) {
	rec.Event = payload.Event
	rec.KnownEvent = KnownEvent(payload.Event)
	rec.IssueID = payload.ID
	rec.IssueIDForm = IssueIDForm(payload.ID)
	rec.ProjectKey = payload.ProjectKey()
	rec.Summary = payload.Summary
	rec.PrimaryActor = payload.PrimaryActor()
	rec.ActorLogins = payload.ActorLogins()
	rec.PayloadTimestamp = payload.Timestamp
	rec.PayloadKeys = keys
	rec.HasNumberInProject = payload.NumberInProject != nil
	rec.ChangedFields = payload.ChangedFieldNames()
	rec.CommentIDs = payload.CommentIDs()
	// A schema error cannot happen here: Parse already validated the body.
	rec.PayloadSchema, _ = Schema(body)
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
func (h *Handler) redact(value string) string {
	if value == "" || h.opts.HookKey == "" {
		return value
	}
	if !strings.Contains(value, h.opts.HookKey) {
		return value
	}
	return strings.ReplaceAll(value, h.opts.HookKey, "***")
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
