// Package opencode talks to a local `opencode serve` instance over its v1 HTTP
// API: create a session bound to a directory, deliver one prompt, answer the
// permission requests that arrive while the turn runs, and report the result.
//
// The protocol details encoded here were measured against opencode 1.18.31
// (opencode-headless-automation-and-permissions.md, plus a live trial on
// 2026-09-20). Four of them are load bearing and easy to get wrong:
//
//   - every session-scoped endpoint needs ?directory=. Without it the server
//     falls back to its own default location and returns empty results, which
//     looks exactly like "nothing is pending".
//   - the directory must be the canonical path: opencode treats /tmp and
//     /private/tmp as two different projects.
//   - v1 only. The v2 /api/* endpoints append the prompt to the event log but
//     never execute it without a runner (the web UI is that runner).
//   - a pending permission keeps the session busy, so "busy" alone never means
//     "still working" and "idle" alone never means "finished".
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is where `opencode serve` listens by default.
const DefaultBaseURL = "http://127.0.0.1:4096"

// Options configures a Client.
type Options struct {
	// BaseURL is the server root, e.g. http://127.0.0.1:4096.
	BaseURL string
	// Username and Password enable HTTP Basic Auth, which the server turns on
	// when OPENCODE_SERVER_PASSWORD is set. Production deployments must set it;
	// the local development instance usually has no auth.
	Username string
	Password string
	// Timeout bounds one HTTP request. The task deadline is separate: a turn can
	// legitimately run for minutes while each request stays fast.
	Timeout time.Duration
}

// Client is a thin, stateless HTTP client for one opencode server.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

// New builds a client.
func New(opts Options) *Client {
	base := strings.TrimSuffix(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL:  base,
		username: opts.Username,
		password: opts.Password,
		http:     &http.Client{Timeout: timeout},
	}
}

// BaseURL returns the configured server root.
func (c *Client) BaseURL() string { return c.baseURL }

// Health reports whether the server is up and which version it runs.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var health Health
	err := c.do(ctx, http.MethodGet, "/global/health", nil, nil, &health)
	return health, err
}

// CreateSession binds a new session to directory. The session-level ruleset in
// req.Permission is the first line of defence: it overrides the project and
// global configuration, and a "deny" on a wildcard pattern removes the tool
// entirely instead of asking.
func (c *Client) CreateSession(ctx context.Context, directory string, req CreateSessionRequest) (Session, error) {
	var session Session
	query := url.Values{"directory": {directory}}
	err := c.do(ctx, http.MethodPost, "/session", query, req, &session)
	return session, err
}

// PromptAsync delivers one user turn and returns as soon as it is admitted
// (204). It does not wait for the model.
func (c *Client) PromptAsync(ctx context.Context, directory, sessionID string, req PromptRequest) error {
	query := url.Values{"directory": {directory}}
	return c.do(ctx, http.MethodPost, "/session/"+sessionID+"/prompt_async", query, req, nil)
}

// Session returns the session record (cost and tokens are aggregated there).
func (c *Client) Session(ctx context.Context, directory, sessionID string) (Session, error) {
	var session Session
	query := url.Values{"directory": {directory}}
	err := c.do(ctx, http.MethodGet, "/session/"+sessionID, query, nil, &session)
	return session, err
}

// SessionStatuses returns the status of every session in directory. Only busy
// and retry mean "running"; anything else (including an absent entry) is idle.
func (c *Client) SessionStatuses(ctx context.Context, directory string) (map[string]Status, error) {
	var statuses map[string]Status
	query := url.Values{"directory": {directory}}
	err := c.do(ctx, http.MethodGet, "/session/status", query, nil, &statuses)
	return statuses, err
}

// Messages returns the full message list for a session, oldest first.
func (c *Client) Messages(ctx context.Context, directory, sessionID string) ([]Message, error) {
	var messages []Message
	query := url.Values{"directory": {directory}}
	err := c.do(ctx, http.MethodGet, "/session/"+sessionID+"/message", query, nil, &messages)
	return messages, err
}

// Permissions lists every pending permission request in directory, across all
// sessions: the caller must filter by session ID.
func (c *Client) Permissions(ctx context.Context, directory string) ([]PermissionRequest, error) {
	var requests []PermissionRequest
	query := url.Values{"directory": {directory}}
	err := c.do(ctx, http.MethodGet, "/permission", query, nil, &requests)
	return requests, err
}

// ReplyPermission answers one request. The reply endpoint is keyed by request ID
// only — it carries no session ID — so a caller running several sessions must
// keep its own request-to-task mapping.
func (c *Client) ReplyPermission(ctx context.Context, directory, requestID string, reply Reply, message string) error {
	body := ReplyRequest{Reply: reply, Message: message}
	query := url.Values{"directory": {directory}}
	return c.do(ctx, http.MethodPost, "/permission/"+requestID+"/reply", query, body, nil)
}

// do performs one request, decoding a JSON response into out when it is non-nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.password != "" {
		request.SetBasicAuth(c.username, c.password)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return &HTTPError{
			Method:     method,
			Path:       path,
			StatusCode: response.StatusCode,
			Body:       strings.TrimSpace(string(payload)),
		}
	}
	if out == nil || len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// HTTPError is a non-2xx response from opencode.
type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, truncate(e.Body, 300))
}

// IsUnauthorized reports whether err is a 401, which means the server requires
// OPENCODE_SERVER_PASSWORD and the client did not supply it.
func IsUnauthorized(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusUnauthorized
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
