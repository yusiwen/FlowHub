package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postRaw sends a delivery with extra headers and a custom remote address.
func postRaw(t *testing.T, h *harness, token, body string, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, keyedPath(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-YouTrack-Token", token)
	}
	for name, value := range extra {
		req.Header.Set(name, value)
	}
	resp := httptest.NewRecorder()
	h.mux.ServeHTTP(resp, req)
	return resp
}

func TestReceivedHeadersAreRecordedWithSecretsMasked(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.LogHeaders = true })
	const authorization = "Bearer super-secret-value"

	resp := postRaw(t, h, testToken, issueUpdatedBody(freshTimestamp()), map[string]string{
		"X-Real-IP":       "203.0.113.7",
		"X-Forwarded-For": "203.0.113.7, 10.8.0.1",
		"Authorization":   authorization,
		"User-Agent":      "YouTrack",
	})
	assertRejected(t, resp, "")

	got := h.sink.last(t)
	if !got.Accepted {
		t.Fatalf("rejected: %s", got.Reason)
	}
	if got.RealIP != "203.0.113.7" {
		t.Fatalf("x-real-ip = %q", got.RealIP)
	}
	if got.ForwardedFor != "203.0.113.7, 10.8.0.1" {
		t.Fatalf("x-forwarded-for = %q", got.ForwardedFor)
	}
	if got.UserAgent != "YouTrack" {
		t.Fatalf("user agent = %q", got.UserAgent)
	}

	// The real wire format stays readable...
	if values := got.Headers["X-Real-Ip"]; len(values) != 1 || values[0] != "203.0.113.7" {
		t.Fatalf("x-real-ip header = %v", got.Headers["X-Real-Ip"])
	}
	if len(got.HeaderNames) == 0 {
		t.Fatal("header names must be recorded")
	}

	// ...but no credential may survive into the record, in any field.
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testToken, authorization, "super-secret-value"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("secret %q leaked into the audit record: %s", secret, encoded)
		}
	}
	// net/http canonicalises header names, so "X-YouTrack-Token" is stored as
	// "X-Youtrack-Token"; the configured spelling is kept in token_header.name.
	for _, name := range []string{http.CanonicalHeaderKey("X-YouTrack-Token"), "Authorization"} {
		values := got.Headers[name]
		if len(values) == 0 || !strings.Contains(values[0], "<masked len=") {
			t.Fatalf("%s was not masked: %v (recorded keys: %v)", name, values, got.HeaderNames)
		}
	}
	if got.TokenHeader == nil || got.TokenHeader.Name != "X-YouTrack-Token" {
		t.Fatalf("configured token header name lost: %+v", got.TokenHeader)
	}
}

func TestTokenHeaderMatchesConfiguredFingerprint(t *testing.T) {
	t.Run("real token matches", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.LogHeaders = true })
		assertRejected(t, h.post(t, keyedPath(), testToken, issueUpdatedBody(freshTimestamp())), "")
		info := h.sink.last(t).TokenHeader
		if info == nil || !info.MatchesConfigured || info.LooksLikeLiteralSecret {
			t.Fatalf("token info = %+v", info)
		}
	})

	t.Run("literal secret is flagged and does not match", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.LogHeaders = true })
		assertRejected(t, h.post(t, keyedPath(), "secret", issueUpdatedBody(freshTimestamp())), "")
		info := h.sink.last(t).TokenHeader
		if info == nil || info.MatchesConfigured || !info.LooksLikeLiteralSecret {
			t.Fatalf("token info = %+v", info)
		}
		if info.ValueLen != len("secret") {
			t.Fatalf("length = %d", info.ValueLen)
		}
	})

	t.Run("wrong token does not match", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.LogHeaders = true })
		assertRejected(t, h.post(t, keyedPath(), "another-value", issueUpdatedBody(freshTimestamp())), "")
		info := h.sink.last(t).TokenHeader
		if info == nil || info.MatchesConfigured {
			t.Fatalf("token info = %+v", info)
		}
	})

	t.Run("absent token header", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.LogHeaders = true })
		assertRejected(t, h.post(t, keyedPath(), "", issueUpdatedBody(freshTimestamp())), "")
		info := h.sink.last(t).TokenHeader
		if info == nil || info.Present || info.MatchesConfigured {
			t.Fatalf("token info = %+v", info)
		}
	})
}

func TestHeaderLoggingIsOptional(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.LogHeaders = false })
	assertRejected(t, h.post(t, keyedPath(), testToken, issueUpdatedBody(freshTimestamp())), "")

	got := h.sink.last(t)
	if got.Headers != nil || got.HeaderNames != nil || got.TokenHeader != nil {
		t.Fatalf("headers were recorded although logging is off: %+v", got.Headers)
	}
}

func TestPayloadSchemaIsRecorded(t *testing.T) {
	h := newHarness(t, nil)
	assertRejected(t, h.post(t, keyedPath(), testToken, issueUpdatedBody(freshTimestamp())), "")

	got := h.sink.last(t)
	schema := strings.Join(got.PayloadSchema, "\n")
	for _, want := range []string{
		"changedFields: array",
		"changedFields[].name: string",
		"changedFields[].oldValue: object",
		"project: object",
		"updated: number",
		"updatedBy.login: string",
	} {
		if !strings.Contains(schema, want) {
			t.Fatalf("schema is missing %q:\n%s", want, schema)
		}
	}
	// The documented-but-absent field must be visibly absent from the real payload.
	if strings.Contains(schema, "numberInProject") {
		t.Fatalf("fixture must not contain numberInProject:\n%s", schema)
	}
}

func TestSchemaIsRecordedForCommentEvents(t *testing.T) {
	h := newHarness(t, nil)
	assertRejected(t, h.post(t, keyedPath(), testToken, commentAddedBody(freshTimestamp())), "")

	got := h.sink.last(t)
	schema := strings.Join(got.PayloadSchema, "\n")
	for _, want := range []string{
		"comments: array",
		"comments[].author.email: string",
		"comments[].text: string",
		"comments[].updated: null",
	} {
		if !strings.Contains(schema, want) {
			t.Fatalf("schema is missing %q:\n%s", want, schema)
		}
	}
}
