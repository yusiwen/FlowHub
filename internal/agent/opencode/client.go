// Package opencode is the opencode implementation of the agent seam: it talks to
// an `opencode serve` instance — create a session bound to a directory, deliver
// one prompt, answer the permission requests that arrive while the turn runs, and
// report the result.
//
// opencode 2 removed the v1 HTTP surface outright: `/global/health` now answers
// the web app's HTML shell, and everything lives under `/api/*`. Both generations
// are implemented here, behind one Client, and the first call decides which one
// the server speaks. The v1 transport is client_v1.go, the v2 one client_v2.go,
// and the protocol-specific traps are documented on each.
//
// Runtime is the agent.Runtime this package offers; Client and Runner are its two
// halves. The phase policy and the session ruleset belong to this product's
// permission model and live here too, so the dispatcher never learns them
// (ADR 0001).
//
// The v1 protocol details encoded here were measured against opencode 1.18.31
// (opencode-headless-automation-and-permissions.md, plus a live trial on
// 2026-09-20); the v2 ones against 2.0.32 on 2026-10-21 (ADR 0012). Four of them
// are load bearing and easy to get wrong:
//
//   - a v1 session-scoped endpoint needs ?directory=, and the directory must be
//     canonical (opencode treats /tmp and /private/tmp as two projects). v2
//     carries the location in the session itself.
//   - a v1 server accepts a prompt on a busy session and silently swallows it; a
//     v2 server queues it durably. Neither makes it safe to prompt a busy session:
//     one task, one turn.
//   - v1 completion needs three signals (completed assistant message, status not
//     busy, no pending permission); v2 writes an `idle` message whose `outcome` is
//     the authoritative end of the turn.
//   - the permission vocabulary changed (`bash`→`shell`, `task`→`subagent`) and so
//     did the body a client replies with (`{decision}` instead of `{reply}`).
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
	"sync"
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

// Client is a thin client for one opencode server.
//
// "Stateless" except for one fact: which API generation the server speaks. That
// is discovered once, on the first call, and remembered for the life of the
// Client — a server does not change generation without a restart, and probing it
// for every request would double the request count of a turn.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client

	mu    sync.Mutex
	proto protocol
}

// protocol is the HTTP API generation a server speaks.
type protocol int

const (
	protocolUnknown protocol = iota
	// protocolV1 is opencode 1.x: /global/health, /session, /permission.
	protocolV1
	// protocolV2 is opencode 2.x: /api/info, /api/session, /api/permission.
	protocolV2
)

func (p protocol) String() string {
	switch p {
	case protocolV1:
		return "v1"
	case protocolV2:
		return "v2"
	default:
		return "unknown"
	}
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

// Protocol reports which HTTP API the server speaks, probing it once.
//
// The order is deliberate: /api/info is tried first because a v2 server answers
// /global/health with the web app's HTML page and status 200, so probing the v1
// endpoint first would classify every v2 server as "v1, answering nonsense".
// A 401 is reported as an authentication failure rather than falling through:
// the server is alive and refusing this client, and the second probe would only
// repeat the same 401.
func (c *Client) Protocol(ctx context.Context) (protocol, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.proto != protocolUnknown {
		return c.proto, nil
	}

	var info struct {
		Version string `json:"version"`
	}
	v2Err := c.do(ctx, http.MethodGet, "/api/info", nil, nil, &info)
	if v2Err == nil && strings.TrimSpace(info.Version) != "" {
		c.proto = protocolV2
		return c.proto, nil
	}
	if IsUnauthorized(v2Err) {
		return protocolUnknown, fmt.Errorf("opencode: %s rejected the credentials on the v2 /api/info probe: %w", c.baseURL, v2Err)
	}

	var health Health
	v1Err := c.do(ctx, http.MethodGet, "/global/health", nil, nil, &health)
	if v1Err == nil && (health.Healthy || strings.TrimSpace(health.Version) != "") {
		c.proto = protocolV1
		return c.proto, nil
	}
	if IsUnauthorized(v1Err) {
		return protocolUnknown, fmt.Errorf("opencode: %s rejected the credentials on the v1 /global/health probe: %w", c.baseURL, v1Err)
	}

	return protocolUnknown, fmt.Errorf(
		"opencode: %s speaks neither API: /api/info (opencode 2) failed: %v; /global/health (opencode 1) failed: %v; a 200 with an HTML body on /global/health means an opencode 2 server whose v1 routes are gone",
		c.baseURL, v2Err, v1Err)
}

// isV2 reports whether the server speaks the v2 API.
func (c *Client) isV2(ctx context.Context) (bool, error) {
	proto, err := c.Protocol(ctx)
	if err != nil {
		return false, err
	}
	return proto == protocolV2, nil
}

// Health reports whether the server is up and which version it runs.
func (c *Client) Health(ctx context.Context) (Health, error) {
	v2, err := c.isV2(ctx)
	if err != nil {
		return Health{}, err
	}
	if v2 {
		return c.healthV2(ctx)
	}
	return c.healthV1(ctx)
}

// Agents lists the agents a server resolves names against.
//
// The list is what makes "the host answers, but does it have the agent?" a
// question with an answer. It is also built once per server *process*, which is
// why an agent file repaired on disk keeps answering with its old settings — the
// model included — until the server restarts.
func (c *Client) Agents(ctx context.Context) ([]AgentInfo, error) {
	v2, err := c.isV2(ctx)
	if err != nil {
		return nil, err
	}
	if v2 {
		return c.agentsV2(ctx)
	}
	return c.agentsV1(ctx)
}

// Catalogue lists the models each configured provider offers.
func (c *Client) Catalogue(ctx context.Context) (map[string][]string, error) {
	v2, err := c.isV2(ctx)
	if err != nil {
		return nil, err
	}
	if v2 {
		return c.catalogueV2(ctx)
	}
	return c.catalogueV1(ctx)
}

// CreateSession binds a new session to directory. req.Permission (v1) or
// req.Permissions (v2) is the session ruleset, whichever the server speaks.
func (c *Client) CreateSession(ctx context.Context, directory string, req CreateSessionRequest) (Session, error) {
	v2, err := c.isV2(ctx)
	if err != nil {
		return Session{}, err
	}
	if v2 {
		return c.createSessionV2(ctx, directory, req)
	}
	return c.createSessionV1(ctx, directory, req)
}

// PromptAsync delivers one user turn and returns as soon as it is admitted. It
// does not wait for the model.
func (c *Client) PromptAsync(ctx context.Context, directory, sessionID string, req PromptRequest) error {
	v2, err := c.isV2(ctx)
	if err != nil {
		return err
	}
	if v2 {
		return c.promptAsyncV2(ctx, sessionID, req)
	}
	return c.promptAsyncV1(ctx, directory, sessionID, req)
}

// Session returns the session record (cost and tokens are aggregated there).
func (c *Client) Session(ctx context.Context, directory, sessionID string) (Session, error) {
	v2, err := c.isV2(ctx)
	if err != nil {
		return Session{}, err
	}
	if v2 {
		return c.sessionV2(ctx, sessionID)
	}
	return c.sessionV1(ctx, directory, sessionID)
}

// SessionStatuses returns the status of one session. Only busy and retry mean
// "running"; anything else (including an absent entry) is idle.
func (c *Client) SessionStatuses(ctx context.Context, directory, sessionID string) (Status, error) {
	v2, err := c.isV2(ctx)
	if err != nil {
		return Status{}, err
	}
	if v2 {
		return c.sessionStatusV2(ctx, sessionID)
	}
	return c.sessionStatusV1(ctx, directory, sessionID)
}

// Messages returns the full message list for a session, oldest first.
func (c *Client) Messages(ctx context.Context, directory, sessionID string) ([]Message, error) {
	v2, err := c.isV2(ctx)
	if err != nil {
		return nil, err
	}
	if v2 {
		return c.messagesV2(ctx, sessionID)
	}
	return c.messagesV1(ctx, directory, sessionID)
}

// Permissions lists the pending permission requests of one session.
func (c *Client) Permissions(ctx context.Context, directory, sessionID string) ([]PermissionRequest, error) {
	v2, err := c.isV2(ctx)
	if err != nil {
		return nil, err
	}
	if v2 {
		return c.permissionsV2(ctx, sessionID)
	}
	return c.permissionsV1(ctx, directory, sessionID)
}

// ReplyPermission answers one request. The session is needed by v2, whose reply
// route is session-scoped; v1 keys the reply by request ID alone and ignores it.
func (c *Client) ReplyPermission(ctx context.Context, directory, sessionID, requestID string, reply Reply, message string) error {
	v2, err := c.isV2(ctx)
	if err != nil {
		return err
	}
	if v2 {
		return c.replyPermissionV2(ctx, sessionID, requestID, reply, message)
	}
	return c.replyPermissionV1(ctx, directory, requestID, reply, message)
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

// IsNotFound reports whether err is a 404. For a permission reply that means the
// request no longer exists — an answer, not a failure: the server resolves a
// permission itself when its turn ends, and a request can also be answered twice.
// For a prompt it means the session the registry recorded does not exist on this
// server (a v1 session after the server was upgraded to v2).
func IsNotFound(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
