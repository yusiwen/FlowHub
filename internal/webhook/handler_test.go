package webhook

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yusiwen/flowhub/internal/dedupe"
	"github.com/yusiwen/flowhub/internal/metrics"
	"github.com/yusiwen/flowhub/internal/store"
)

const (
	testKey   = "0123456789abcdef0123456789abcdef"
	testToken = "feedfacefeedfacefeedfacefeedface"
)

type captureSink struct {
	mu      sync.Mutex
	records []*store.Record
}

func (s *captureSink) Record(rec *store.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
}

func (s *captureSink) last(t *testing.T) *store.Record {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		t.Fatal("no audit record captured")
	}
	return s.records[len(s.records)-1]
}

type harness struct {
	sink    *captureSink
	stats   *metrics.Counters
	mux     *http.ServeMux
	handler *Handler
}

func newHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	opts := Options{
		HookPath:     "/hooks/youtrack",
		HookKey:      testKey,
		TokenHeader:  "X-YouTrack-Token",
		Token:        testToken,
		MaxBodyBytes: 64 << 10,
		ReplayWindow: 10 * time.Minute,
	}
	if mutate != nil {
		mutate(&opts)
	}

	sink := &captureSink{}
	stats := metrics.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(opts, sink, dedupe.New(time.Hour), logger, stats)

	mux := http.NewServeMux()
	mux.Handle("/hooks/youtrack", handler)
	mux.Handle("/hooks/youtrack/{key}", handler)

	return &harness{sink: sink, stats: stats, mux: mux, handler: handler}
}

// post sends a delivery; callers override RemoteAddr with withRemoteAddr.
func (h *harness) post(t *testing.T, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-YouTrack-Token", token)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func (h *harness) postFrom(t *testing.T, addr, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hooks/youtrack/"+testKey, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-YouTrack-Token", testToken)
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func keyedPath() string {
	return "/hooks/youtrack/" + testKey
}

func freshTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func issueUpdatedBody(timestamp string) string {
	return `{"event":"issueUpdated","timestamp":"` + timestamp + `","id":"2-123","summary":"Fix login",` +
		`"project":{"key":"SP","name":"Sample Project","shortName":"SP"},"description":"body text",` +
		`"updated":1732708800000,"updatedBy":{"login":"jane.doe","fullName":"Jane Doe","email":"jane@example.com"},` +
		`"changedFields":[{"name":"State","oldValue":{"name":"Open","presentation":"Open"},` +
		`"value":{"name":"In Progress","presentation":"In Progress"}}]}`
}

func commentAddedBody(timestamp string) string {
	return `{"event":"commentAdded","timestamp":"` + timestamp + `","id":"2-123","summary":"Fix login",` +
		`"project":{"key":"SP","name":"Sample Project","shortName":"SP"},` +
		`"comments":[{"id":"4-14","text":"/opencode please look","textPreview":"/opencode pl…",` +
		`"created":1732708800000,"updated":null,` +
		`"author":{"login":"john.doe","fullName":"John Doe","email":"john@example.com"}}]}`
}

// assertRejected checks the invariant that a failed delivery is indistinguishable
// from an accepted one: 202 with an empty body and no explanatory header. The
// expected reason is asserted separately against the audit record by each test.
func assertRejected(t *testing.T, rec *httptest.ResponseRecorder, wantReason string) {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
	for _, header := range []string{"X-FlowHub-Reason", "X-FlowHub-Error"} {
		if got := rec.Header().Get(header); got != "" {
			t.Fatalf("rejection detail leaked in %s: %q", header, got)
		}
	}
	if wantReason != "" && !IsKnownReason(wantReason) {
		t.Fatalf("test uses an unknown reason label %q", wantReason)
	}
}

func TestAcceptsValidDelivery(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.post(t, keyedPath(), testToken, issueUpdatedBody(freshTimestamp()))

	assertRejected(t, resp, "")
	got := h.sink.last(t)
	if !got.Accepted {
		t.Fatalf("accepted = false, reason = %q", got.Reason)
	}
	if !got.Locks.URLKey || !got.Locks.HeaderToken {
		t.Fatalf("locks = %+v, want the two configured locks satisfied", got.Locks)
	}
	if got.Locks.SourceIP {
		t.Fatalf("source lock is not configured but reported satisfied: %+v", got.Locks)
	}
	if strings.Join(got.ActiveLocks, ",") != "url_key,header_token" {
		t.Fatalf("active locks = %v", got.ActiveLocks)
	}
	if got.Event != "issueUpdated" || got.IssueID != "2-123" || got.ProjectKey != "SP" {
		t.Fatalf("payload facts = %s/%s/%s", got.Event, got.IssueID, got.ProjectKey)
	}
	if got.IssueIDForm != "database" {
		t.Fatalf("issue id form = %q, want database", got.IssueIDForm)
	}
	if got.PrimaryActor != "jane.doe" {
		t.Fatalf("primary actor = %q", got.PrimaryActor)
	}
	if len(got.ChangedFields) != 1 || got.ChangedFields[0] != "State" {
		t.Fatalf("changed fields = %v", got.ChangedFields)
	}
	if got.DedupeKey == "" || got.BodySHA256 == "" {
		t.Fatalf("dedupe/body hash missing: %q %q", got.DedupeKey, got.BodySHA256)
	}
	if got.RawBody == "" {
		t.Fatal("raw body must be audited")
	}
	if h.stats.Accepted.Load() != 1 {
		t.Fatalf("accepted counter = %d", h.stats.Accepted.Load())
	}
	if got.HasNumberInProject {
		t.Fatal("the released app does not send numberInProject; this fixture must not either")
	}
}

func TestLockURLKeyRejectsWrongAndMissingKey(t *testing.T) {
	for name, target := range map[string]string{
		"wrong key":   "/hooks/youtrack/deadbeefdeadbeef",
		"missing key": "/hooks/youtrack",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			resp := h.post(t, target, testToken, issueUpdatedBody(freshTimestamp()))
			assertRejected(t, resp, ReasonBadURLKey)
			if got := h.sink.last(t); got.Reason != ReasonBadURLKey || got.Accepted {
				t.Fatalf("record = %+v", got)
			}
		})
	}
}

func TestURLKeyAcceptedFromQuery(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.post(t, "/hooks/youtrack?k="+testKey, testToken, issueUpdatedBody(freshTimestamp()))
	assertRejected(t, resp, "")
	got := h.sink.last(t)
	if !got.Accepted {
		t.Fatalf("rejected: %s", got.Reason)
	}
	// The key travels in the query string and must not survive into the audit log.
	if strings.Contains(got.Query, testKey) {
		t.Fatalf("query was not redacted: %q", got.Query)
	}
}

func TestURLKeyLockCanBeDisabled(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.HookKey = "" })
	resp := h.post(t, "/hooks/youtrack", testToken, issueUpdatedBody(freshTimestamp()))
	assertRejected(t, resp, "")
	if got := h.sink.last(t); !got.Accepted || got.Locks.URLKey {
		t.Fatalf("record = %+v", got)
	}
}

func TestLockHeaderToken(t *testing.T) {
	t.Run("wrong token", func(t *testing.T) {
		h := newHarness(t, nil)
		resp := h.post(t, keyedPath(), "not-the-token", issueUpdatedBody(freshTimestamp()))
		assertRejected(t, resp, ReasonBadHeaderToken)
		if got := h.sink.last(t); got.Reason != ReasonBadHeaderToken || got.Locks.URLKey != true {
			t.Fatalf("record = %+v", got)
		}
	})
	t.Run("missing token", func(t *testing.T) {
		h := newHarness(t, nil)
		resp := h.post(t, keyedPath(), "", issueUpdatedBody(freshTimestamp()))
		assertRejected(t, resp, ReasonBadHeaderToken)
	})
	t.Run("token header name is configurable", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.TokenHeader = "X-Custom-Token" })
		req := httptest.NewRequest(http.MethodPost, keyedPath(), strings.NewReader(issueUpdatedBody(freshTimestamp())))
		req.Header.Set("X-Custom-Token", testToken)
		resp := httptest.NewRecorder()
		h.mux.ServeHTTP(resp, req)
		assertRejected(t, resp, "")
		if got := h.sink.last(t); !got.Accepted {
			t.Fatalf("rejected: %s", got.Reason)
		}
	})
}

func TestLockSourceIP(t *testing.T) {
	networks, err := parseTestNetworks("203.0.113.0/24")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.AllowedSources = networks })

	resp := h.postFrom(t, "192.0.2.1:5000", issueUpdatedBody(freshTimestamp()))
	assertRejected(t, resp, ReasonSourceNotAllowed)
	if got := h.sink.last(t); got.RemoteIP != "192.0.2.1" {
		t.Fatalf("remote ip = %q", got.RemoteIP)
	}

	resp = h.postFrom(t, "203.0.113.9:5000", issueUpdatedBody(freshTimestamp()))
	assertRejected(t, resp, "")
	if got := h.sink.last(t); !got.Accepted || !got.Locks.SourceIP {
		t.Fatalf("record = %+v", got)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newHarness(t, nil)
	req := httptest.NewRequest(http.MethodGet, keyedPath(), nil)
	resp := httptest.NewRecorder()
	h.mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.Code)
	}
	if got := h.sink.last(t); got.Reason != ReasonMethodNotAllowed {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestInvalidJSON(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.post(t, keyedPath(), testToken, "{not json")
	assertRejected(t, resp, ReasonInvalidJSON)
	got := h.sink.last(t)
	if got.Reason != ReasonInvalidJSON {
		t.Fatalf("reason = %q", got.Reason)
	}
	if got.BodySHA256 == "" {
		t.Fatal("body hash must be recorded even for malformed bodies")
	}
}

func TestUnknownEventIsDropped(t *testing.T) {
	h := newHarness(t, nil)
	body := `{"event":"issueRenamed","timestamp":"` + freshTimestamp() + `","id":"2-123"}`
	resp := h.post(t, keyedPath(), testToken, body)
	assertRejected(t, resp, ReasonUnknownEvent)
	got := h.sink.last(t)
	if got.Reason != ReasonUnknownEvent || got.KnownEvent {
		t.Fatalf("record = %+v", got)
	}
}

func TestReplayWindow(t *testing.T) {
	for name, offset := range map[string]time.Duration{
		"stale":   -30 * time.Minute,
		"future":  +30 * time.Minute,
		"in band": -time.Minute,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			ts := time.Now().UTC().Add(offset).Format(time.RFC3339Nano)
			resp := h.post(t, keyedPath(), testToken, issueUpdatedBody(ts))
			assertRejected(t, resp, "")
			got := h.sink.last(t)
			if offset == -time.Minute {
				if !got.Accepted {
					t.Fatalf("rejected: %s", got.Reason)
				}
				if got.TimestampSkewMS == nil {
					t.Fatal("skew must be recorded for accepted deliveries too")
				}
				return
			}
			if got.Reason != ReasonOutsideReplayWindow {
				t.Fatalf("reason = %q, want %s", got.Reason, ReasonOutsideReplayWindow)
			}
		})
	}
}

func TestReplayWindowCanBeDisabled(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ReplayWindow = 0 })
	ts := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano)
	resp := h.post(t, keyedPath(), testToken, issueUpdatedBody(ts))
	assertRejected(t, resp, "")
	if got := h.sink.last(t); !got.Accepted {
		t.Fatalf("rejected: %s", got.Reason)
	}
}

func TestUnparseableTimestamp(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.post(t, keyedPath(), testToken, issueUpdatedBody("yesterday"))
	assertRejected(t, resp, ReasonUnparseableTimestamp)
}

func TestDuplicateDeliveryIsRemembered(t *testing.T) {
	h := newHarness(t, nil)
	body := issueUpdatedBody(freshTimestamp())

	assertRejected(t, h.post(t, keyedPath(), testToken, body), "")
	first := h.sink.last(t)

	resp := h.post(t, keyedPath(), testToken, body)
	assertRejected(t, resp, ReasonDuplicate)
	second := h.sink.last(t)

	if !second.Duplicate || second.Accepted {
		t.Fatalf("second delivery = %+v", second)
	}
	if first.DedupeKey != second.DedupeKey {
		t.Fatalf("dedupe key changed: %q vs %q", first.DedupeKey, second.DedupeKey)
	}
	if h.stats.Duplicates.Load() != 1 {
		t.Fatalf("duplicates counter = %d", h.stats.Duplicates.Load())
	}
}

func TestBodyTooLarge(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxBodyBytes = 64 })
	resp := h.post(t, keyedPath(), testToken, issueUpdatedBody(freshTimestamp()))
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.Code)
	}
	if got := h.sink.last(t); got.Reason != ReasonBodyTooLarge {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestTokenFingerprintAnswersTheLiteralSecretQuestion(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.LogHeaders = true })
	resp := h.post(t, keyedPath(), "secret", issueUpdatedBody(freshTimestamp()))
	assertRejected(t, resp, ReasonBadHeaderToken)

	got := h.sink.last(t)
	if got.TokenHeader == nil {
		t.Fatal("token fingerprint missing")
	}
	if !got.TokenHeader.LooksLikeLiteralSecret {
		t.Fatal("literal \"secret\" must be flagged")
	}
	if got.TokenHeader.ValueLen != len("secret") {
		t.Fatalf("token length = %d", got.TokenHeader.ValueLen)
	}
	if strings.Contains(got.TokenHeader.SHA256Prefix, "secret") {
		t.Fatal("fingerprint must not contain the value")
	}
	if len(got.HeaderNames) == 0 {
		t.Fatal("header names must be recorded when header logging is on")
	}
}

func TestRawTokenNeverReachesTheAuditLog(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.LogHeaders = true })
	assertRejected(t, h.post(t, keyedPath(), testToken, issueUpdatedBody(freshTimestamp())), "")

	got := h.sink.last(t)
	if got.TokenHeader == nil || got.TokenHeader.SHA256Prefix == testToken {
		t.Fatal("token value leaked into the audit record")
	}
	if strings.Contains(got.Path, testKey) {
		t.Fatalf("url key leaked into path: %q", got.Path)
	}
	if strings.Contains(got.Path, "***") == false {
		t.Fatalf("path should be redacted: %q", got.Path)
	}
}

func TestNumberInProjectProbe(t *testing.T) {
	h := newHarness(t, nil)

	without := `{"event":"issueCreated","timestamp":"` + freshTimestamp() + `","id":"2-9","summary":"s",` +
		`"project":{"key":"SP","name":"Sample","shortName":"SP"},"description":"d","created":1,` +
		`"reporter":{"login":"jane.doe"}}`
	assertRejected(t, h.post(t, keyedPath(), testToken, without), "")
	got := h.sink.last(t)
	if got.HasNumberInProject {
		t.Fatal("numberInProject must be reported absent")
	}
	if strings.Join(got.PayloadKeys, ",") != "created,description,event,id,project,reporter,summary,timestamp" {
		t.Fatalf("payload keys = %v", got.PayloadKeys)
	}

	with := `{"event":"issueCreated","timestamp":"` + freshTimestamp() + `","id":"2-10",` +
		`"numberInProject":10,"unknownNewField":true}`
	assertRejected(t, h.post(t, keyedPath(), testToken, with), "")
	got = h.sink.last(t)
	if !got.HasNumberInProject || !got.KnownEvent {
		t.Fatalf("record = %+v", got)
	}
	// Unknown fields must not break parsing: the app and its docs disagree.
	if !strings.Contains(strings.Join(got.PayloadKeys, ","), "unknownNewField") {
		t.Fatalf("payload keys = %v", got.PayloadKeys)
	}
}

func TestCommentAddedFacts(t *testing.T) {
	h := newHarness(t, nil)
	assertRejected(t, h.post(t, keyedPath(), testToken, commentAddedBody(freshTimestamp())), "")

	got := h.sink.last(t)
	if !got.Accepted || got.Event != "commentAdded" {
		t.Fatalf("record = %+v", got)
	}
	if got.PrimaryActor != "john.doe" {
		t.Fatalf("primary actor = %q, want the comment author", got.PrimaryActor)
	}
	if len(got.CommentIDs) != 1 || got.CommentIDs[0] != "4-14" {
		t.Fatalf("comment ids = %v", got.CommentIDs)
	}
	if !strings.Contains(got.RawBody, "/opencode") {
		t.Fatal("raw comment text must be audited")
	}
}

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

func TestCountersReflectOutcomes(t *testing.T) {
	h := newHarness(t, nil)
	body := issueUpdatedBody(freshTimestamp())
	assertRejected(t, h.post(t, keyedPath(), testToken, body), "")
	assertRejected(t, h.post(t, keyedPath(), testToken, body), "")
	assertRejected(t, h.post(t, keyedPath(), "bad", issueUpdatedBody(freshTimestamp())), "")

	snapshot := h.stats.Snapshot()
	if snapshot["received"].(int64) != 3 {
		t.Fatalf("received = %v", snapshot["received"])
	}
	if snapshot["accepted"].(int64) != 1 {
		t.Fatalf("accepted = %v", snapshot["accepted"])
	}
	if snapshot["rejected"].(int64) != 2 {
		t.Fatalf("rejected = %v", snapshot["rejected"])
	}
	reasons := snapshot["reject_reasons"].(map[string]int64)
	if reasons[ReasonDuplicate] != 1 || reasons[ReasonBadHeaderToken] != 1 {
		t.Fatalf("reject reasons = %v", reasons)
	}
}

func parseTestNetworks(spec string) ([]*net.IPNet, error) {
	_, network, err := net.ParseCIDR(spec)
	if err != nil {
		return nil, fmt.Errorf("bad test network %q: %w", spec, err)
	}
	return []*net.IPNet{network}, nil
}

// captureDispatcher records what the phase-2 bridge was handed.
type captureDispatcher struct {
	mu     sync.Mutex
	queued []*store.Record
}

func (c *captureDispatcher) Dispatch(rec *store.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queued = append(c.queued, rec)
}

func (c *captureDispatcher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queued)
}

func TestDispatcherSeesAcceptedDeliveriesOnly(t *testing.T) {
	bridge := &captureDispatcher{}
	h := newHarness(t, func(o *Options) { o.Dispatcher = bridge })

	// Accepted, then the same bytes again (duplicate), then a bad token.
	body := issueUpdatedBody(freshTimestamp())
	assertRejected(t, h.post(t, keyedPath(), testToken, body), "")
	assertRejected(t, h.post(t, keyedPath(), testToken, body), "")
	assertRejected(t, h.post(t, keyedPath(), "bad", issueUpdatedBody(freshTimestamp())), "")

	if got := bridge.count(); got != 1 {
		t.Fatalf("dispatch calls = %d, want 1 (only the accepted delivery)", got)
	}
	// The dispatched record must be the same one that was audited, and it must
	// already carry the derived fields the rules need.
	var audited *store.Record
	h.sink.mu.Lock()
	for _, rec := range h.sink.records {
		if rec.Accepted {
			audited = rec
		}
	}
	h.sink.mu.Unlock()
	if audited == nil {
		t.Fatal("no accepted record was audited")
	}
	if bridge.queued[0] != audited {
		t.Fatal("the dispatched record is not the audited record")
	}
	if audited.IssueID != "2-123" || audited.RawBody == "" || audited.Event != "issueUpdated" {
		t.Fatalf("dispatched record is incomplete: %+v", audited)
	}
}

func TestNilDispatcherIsAllowed(t *testing.T) {
	h := newHarness(t, nil)
	assertRejected(t, h.post(t, keyedPath(), testToken, issueUpdatedBody(freshTimestamp())), "")
	if h.sink.last(t).Accepted != true {
		t.Fatal("the delivery was not accepted")
	}
}

// TestRedactionNeverEchoesAnUnrecognisedKey is a regression test for a real
// leak: redaction used to replace only the *configured* key, so a receiver
// started with a rotated or freshly generated key wrote the app's live key into
// the audit log in plaintext — which is precisely the delivery that gets
// audited (the one whose key did not match). Measured on this deployment on
// 2026-09-21: a probe instance recovered the running app's key from its own
// data/webhook-*.jsonl.
func TestRedactionNeverEchoesAnUnrecognisedKey(t *testing.T) {
	// 64 hex characters, the shape `openssl rand -hex 32` produces. Synthetic:
	// never paste a real key into a test, which is how one ends up in git.
	const liveKey = "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
	h := newHarness(t, func(o *Options) { o.HookKey = testKey })

	assertRejected(t, h.post(t, "/hooks/youtrack/"+liveKey, testToken, issueUpdatedBody(freshTimestamp())),
		ReasonBadURLKey)
	got := h.sink.last(t)
	if strings.Contains(got.Path, liveKey) {
		t.Fatalf("the app's key leaked into the audit path: %q", got.Path)
	}
	if got.Path != "/hooks/youtrack/***" {
		t.Fatalf("path = %q, want /hooks/youtrack/***", got.Path)
	}
	// The same request with the key in the query string.
	assertRejected(t, h.post(t, "/hooks/youtrack?k="+liveKey, testToken, issueUpdatedBody(freshTimestamp())),
		ReasonBadURLKey)
	got = h.sink.last(t)
	if strings.Contains(got.Query, liveKey) || got.Query != "k=***" {
		t.Fatalf("query = %q, want k=***", got.Query)
	}
}

func TestRedactionCoversTheEdgesOfTheRouteShape(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.HookKey = testKey })
	cases := map[string]string{
		"/hooks/youtrack/" + testKey: "/hooks/youtrack/***",
		"/hooks/youtrack":            "/hooks/youtrack",
		// A trailing slash with no key carries no secret, so none is invented.
		"/hooks/youtrack/":                      "/hooks/youtrack/",
		"/hooks/youtrack/" + testKey + "/extra": "/hooks/youtrack/***/extra",
		"/something/else/" + testKey:            "/something/else/***",
		"/" + testKey:                           "/***",
		"/":                                     "/",
		"":                                      "",
	}
	for path, want := range cases {
		if got := h.handler.redactPath(path); got != want {
			t.Errorf("redactPath(%q) = %q, want %q", path, got, want)
		}
	}
	if got := h.handler.redactQuery("k=" + testKey + "&other=1"); got != "k=***&other=1" {
		t.Errorf("redactQuery = %q", got)
	}
	if got := h.handler.redactQuery("other=1"); got != "other=1" {
		t.Errorf("redactQuery = %q", got)
	}

	// A receiver with the key lock disabled still must not store the key the app
	// sends: the lock being off is a configuration choice, not a licence to leak.
	off := newHarness(t, func(o *Options) { o.HookKey = "" })
	if got := off.handler.redactPath("/hooks/youtrack/" + testKey); got != "/hooks/youtrack/***" {
		t.Fatalf("with the lock off, path = %q", got)
	}
}
