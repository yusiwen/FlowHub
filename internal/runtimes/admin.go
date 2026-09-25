package runtimes

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server is the control API: how a host enrols, how it reports in, and how an
// operator inspects or removes runtimes.
//
// It is deliberately a separate listener from the webhook entry. That entry
// answers 202 to everything and explains nothing; this one must return real
// errors, because the caller is a program (init) or an operator fixing a machine.
type Server struct {
	Inventory  *Inventory
	AdminToken string
	// Prober proves the control plane can reach a host before activating it.
	Prober Prober
	// BoundTasks reports the non-terminal tasks bound to a runtime, so removing a
	// host that is still in use can be refused. It may be nil.
	BoundTasks BoundTasksFunc
	Log        *slog.Logger
	// Now is injectable for tests.
	Now func() time.Time
}

// Handler builds the control API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /control/v1/runtimes/invite", s.admin(s.handleInvite))
	mux.HandleFunc("POST /control/v1/runtimes/register", s.handleRegister)
	mux.HandleFunc("GET /control/v1/runtimes", s.admin(s.handleList))
	mux.HandleFunc("GET /control/v1/runtimes/{name}", s.admin(s.handleShow))
	mux.HandleFunc("DELETE /control/v1/runtimes/{name}", s.admin(s.handleRemove))
	mux.HandleFunc("POST /control/v1/runtimes/{name}/rotate", s.admin(s.handleRotate))
	mux.HandleFunc("POST /control/v1/runtimes/{name}/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /control/v1/health", s.handleHealth)
	return mux
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// admin wraps a handler that only the operator may call.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AdminToken == "" {
			writeError(w, http.StatusServiceUnavailable, "the control API has no admin token configured")
			return
		}
		if !bearerMatches(r, s.AdminToken) {
			s.log().Warn("control API: rejected admin call", "path", r.URL.Path, "remote", r.RemoteAddr)
			writeError(w, http.StatusUnauthorized, "admin token required")
			return
		}
		next(w, r)
	}
}

// handleInvite reserves a name. It is an admin action, and the token it returns is
// the whole point of enrolment being admin-initiated: registering a host is
// handing a machine work to execute, so it starts here.
func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	if s.Inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "no runtime inventory is configured")
		return
	}
	var request struct {
		Name       string   `json:"name"`
		Projects   []string `json:"projects"`
		TTLSeconds int      `json:"ttl_seconds"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ttl := DefaultInviteTTL
	if request.TTLSeconds > 0 {
		ttl = time.Duration(request.TTLSeconds) * time.Second
	}
	token, runtime, err := s.Inventory.Invite(request.Name, request.Projects, ttl)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.log().Info("control API: runtime invited",
		"name", runtime.Name, "projects", strings.Join(request.Projects, ","), "expires", runtime.Invite.ExpiresAt)
	writeJSON(w, http.StatusCreated, map[string]any{
		"runtime": runtime,
		"token":   token,
		"note":    "one-time token; it is not stored and cannot be shown again",
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Claim
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.Inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "no runtime inventory is configured")
		return
	}

	runtime, secret, err := s.Inventory.Register(r.Context(), request.Claim, request.Token, s.Prober, s.now())
	if err != nil {
		// The host is told exactly what is wrong: this is a program talking, and a
		// vague failure here costs an operator an hour.
		s.log().Warn("control API: registration failed", "name", request.Name, "error", err)
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	s.log().Info("control API: runtime registered",
		"name", runtime.Name, "agent", runtime.Agent, "agent_version", runtime.AgentVersion,
		"advertise", runtime.Advertise)
	writeJSON(w, http.StatusCreated, map[string]any{
		"runtime": runtime,
		"secret":  secret,
		"note":    "store this secret: it is not recoverable, and it authenticates this host's heartbeats",
	})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if s.Inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "no runtime inventory is configured")
		return
	}
	name := r.PathValue("name")
	var request struct {
		Report map[string]any `json:"report,omitempty"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	secret := bearerToken(r)
	runtime, err := s.Inventory.Heartbeat(name, secret, request.Report, s.now())
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtime": runtime})
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	if s.Inventory == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runtimes": []Runtime{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtimes": s.Inventory.List()})
}

func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	if s.Inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "no runtime inventory is configured")
		return
	}
	runtime, ok := s.Inventory.Get(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("runtime %s is not registered", r.PathValue("name")))
		return
	}
	body := map[string]any{"runtime": runtime}
	if s.BoundTasks != nil {
		body["bound_tasks"] = s.BoundTasks(runtime.Name)
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	if s.Inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "no runtime inventory is configured")
		return
	}
	force := false
	if raw := strings.TrimSpace(r.URL.Query().Get("force")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "force must be true or false")
			return
		}
		force = parsed
	}
	runtime, tasks, err := s.Inventory.Remove(r.PathValue("name"), force, s.BoundTasks)
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "is not registered") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "bound_tasks": tasks})
		return
	}
	s.log().Info("control API: runtime removed", "name", runtime.Name, "force", force, "bound_tasks", len(tasks))
	writeJSON(w, http.StatusOK, map[string]any{"runtime": runtime, "bound_tasks": tasks})
}

func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request) {
	if s.Inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "no runtime inventory is configured")
		return
	}
	secret, err := s.Inventory.Rotate(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.log().Info("control API: runtime secret rotated", "name", r.PathValue("name"))
	writeJSON(w, http.StatusOK, map[string]any{
		"secret": secret,
		"note":   "store this secret: the previous one no longer works",
	})
}

// handleHealth is the only unauthenticated route: a liveness probe for whatever
// supervises the control plane. It says nothing an attacker can use.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	counts := map[string]int{}
	if s.Inventory != nil {
		counts = s.Inventory.Counts()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "runtimes": counts})
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// bearerMatches compares the request's bearer token with an expected value in
// constant time.
func bearerMatches(r *http.Request, expected string) bool {
	presented := bearerToken(r)
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// maxControlBody bounds a control request. The capability report is the largest
// thing that legitimately arrives, and it is a few kilobytes.
const maxControlBody = 1 << 20

func decodeJSON(r *http.Request, target any) error {
	if r.Body == nil {
		return errors.New("a request body is required")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxControlBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("the request body is not valid JSON: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

// Client talks to a control plane's API. It is what the CLI on the service host
// and `flowhub runtime init` on a worker use.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient builds a client with a bounded HTTP timeout.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// InviteResponse is what an operator gets back, and what a worker needs.
type InviteResponse struct {
	Runtime Runtime `json:"runtime"`
	Token   string  `json:"token"`
}

// Invite asks the control plane to reserve a runtime name.
func (c *Client) Invite(ctx context.Context, name string, projects []string, ttl time.Duration) (InviteResponse, error) {
	body := map[string]any{"name": name, "projects": projects}
	if ttl > 0 {
		body["ttl_seconds"] = int(ttl.Seconds())
	}
	var response struct {
		Runtime Runtime `json:"runtime"`
		Token   string  `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, "/control/v1/runtimes/invite", body, &response); err != nil {
		return InviteResponse{}, err
	}
	return InviteResponse{Runtime: response.Runtime, Token: response.Token}, nil
}

// List returns every runtime the control plane knows.
func (c *Client) List(ctx context.Context) ([]Runtime, error) {
	var response struct {
		Runtimes []Runtime `json:"runtimes"`
	}
	if err := c.do(ctx, http.MethodGet, "/control/v1/runtimes", nil, &response); err != nil {
		return nil, err
	}
	return response.Runtimes, nil
}

// Show returns one runtime and the tasks bound to it.
func (c *Client) Show(ctx context.Context, name string) (Runtime, []string, error) {
	var response struct {
		Runtime    Runtime  `json:"runtime"`
		BoundTasks []string `json:"bound_tasks"`
	}
	if err := c.do(ctx, http.MethodGet, "/control/v1/runtimes/"+name, nil, &response); err != nil {
		return Runtime{}, nil, err
	}
	return response.Runtime, response.BoundTasks, nil
}

// Remove revokes a runtime.
func (c *Client) Remove(ctx context.Context, name string, force bool) ([]string, error) {
	path := "/control/v1/runtimes/" + name
	if force {
		path += "?force=true"
	}
	var response struct {
		BoundTasks []string `json:"bound_tasks"`
	}
	if err := c.do(ctx, http.MethodDelete, path, nil, &response); err != nil {
		return nil, err
	}
	return response.BoundTasks, nil
}

// Rotate issues a new secret for a runtime.
func (c *Client) Rotate(ctx context.Context, name string) (string, error) {
	var response struct {
		Secret string `json:"secret"`
	}
	if err := c.do(ctx, http.MethodPost, "/control/v1/runtimes/"+name+"/rotate", map[string]any{}, &response); err != nil {
		return "", err
	}
	return response.Secret, nil
}

// Register submits a capability claim with an invite token.
func (c *Client) Register(ctx context.Context, token string, claim Claim) (Runtime, string, error) {
	body := struct {
		Claim
		Token string `json:"token"`
	}{Claim: claim, Token: token}

	var response struct {
		Runtime Runtime `json:"runtime"`
		Secret  string  `json:"secret"`
	}
	if err := c.do(ctx, http.MethodPost, "/control/v1/runtimes/register", body, &response); err != nil {
		return Runtime{}, "", err
	}
	return response.Runtime, response.Secret, nil
}

// Heartbeat reports in.
func (c *Client) Heartbeat(ctx context.Context, name, secret string, report map[string]any) error {
	previous := c.Token
	c.Token = secret
	defer func() { c.Token = previous }()
	return c.do(ctx, http.MethodPost, "/control/v1/runtimes/"+name+"/heartbeat", map[string]any{"report": report}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	if c.BaseURL == "" {
		return errors.New("no control plane address is configured (set FLOWHUB_ADMIN_ADDR, or pass --server)")
	}
	var reader *strings.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(encoded))
	} else {
		reader = strings.NewReader("")
	}

	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("cannot reach the control plane at %s: %w", c.BaseURL, err)
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&failure)
		if failure.Error != "" {
			return fmt.Errorf("%s", failure.Error)
		}
		return fmt.Errorf("the control plane answered %s", response.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}
