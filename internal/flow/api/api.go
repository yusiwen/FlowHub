// Package api serves the read-only /hub/v1 surface of the FlowHub service.
//
// Every endpoint requires FLOWHUB_HUB_TOKEN. The token is deliberately separate
// from the control API's admin token: the desktop may read flow definitions and runs
// without being able to invite, remove or rotate a runtime. A missing token disables
// the whole surface rather than exposing it unauthenticated.
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/yusiwen/flowhub/internal/compare"
	"github.com/yusiwen/flowhub/internal/flow"
	"github.com/yusiwen/flowhub/internal/flow/engine"
	"github.com/yusiwen/flowhub/internal/flow/mode"
)

// Options configures the hub API.
type Options struct {
	Token      string
	Flow       *flow.Compiled
	Definition []byte
	Modes      *mode.Store
	Runs       *engine.Store
	// OnAvailable reports whether this process has the task service wired. `on`
	// is refused without it: skipping dispatch but not running turns would lose work.
	OnAvailable bool
	// Cancel stops an active run. Nil disables the endpoint with 503 rather than
	// pretending a run was canceled.
	Cancel func(runID string) (*engine.Run, error)
	// Compare joins a shadow run with dispatch's decision for the same event.
	Compare *compare.Store
	Logger  *slog.Logger
}

// Handler is the /hub/v1 mux with the bearer check in front.
type Handler struct {
	opts     Options
	mux      *http.ServeMux
	tokenSHA [32]byte
}

// New builds the API. A missing token is allowed; every request then gets 503.
func New(opts Options) (*Handler, error) {
	if opts.Flow == nil {
		return nil, fmt.Errorf("flow api: a compiled flow is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	handler := &Handler{opts: opts, mux: http.NewServeMux()}
	if opts.Token != "" {
		handler.tokenSHA = sha256.Sum256([]byte(opts.Token))
	}
	handler.mux.HandleFunc("GET /hub/v1/ping", handler.ping)
	handler.mux.HandleFunc("GET /hub/v1/flows", handler.flows)
	handler.mux.HandleFunc("GET /hub/v1/flows/{id}", handler.flow)
	handler.mux.HandleFunc("POST /hub/v1/flows/{id}/deployment", handler.deploy)
	handler.mux.HandleFunc("GET /hub/v1/runs", handler.runs)
	handler.mux.HandleFunc("GET /hub/v1/runs/{id}", handler.run)
	handler.mux.HandleFunc("POST /hub/v1/runs/{id}/cancel", handler.cancel)
	handler.mux.HandleFunc("GET /hub/v1/runs/{id}/events", handler.events)
	return handler, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.opts.Token == "" {
		writeError(w, http.StatusServiceUnavailable, "the hub API is disabled; set FLOWHUB_HUB_TOKEN to enable /hub/v1")
		return
	}
	if !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, "a hub token is required")
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) authorized(r *http.Request) bool {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	presented := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if presented == "" {
		return false
	}
	sum := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(sum[:], h.tokenSHA[:]) == 1
}

func (h *Handler) ping(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) flows(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"flows": []any{h.flowSummary(h.lastRun())},
	})
}

func (h *Handler) flow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id != h.opts.Flow.Definition.ID {
		writeError(w, http.StatusNotFound, "unknown flow")
		return
	}
	entry := h.entry()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           h.opts.Flow.Definition.ID,
		"name":         h.opts.Flow.Definition.Name,
		"builtin":      h.opts.Flow.Definition.Builtin,
		"upstream":     h.opts.Flow.Definition.Upstream,
		"revision":     h.revision(),
		"mode":         entry.Mode,
		"projects":     entry.Projects,
		"updated_at":   entry.UpdatedAt,
		"updated_by":   entry.UpdatedBy,
		"on_available": h.opts.OnAvailable,
		"definition":   json.RawMessage(h.opts.Definition),
	})
}

type deploymentRequest struct {
	Mode     string   `json:"mode"`
	Projects []string `json:"projects"`
}

// deploy writes the deployment state. `on` is intentionally refused until the
// task service can execute task.turn: accepting it now would make dispatch skip a
// project the flow cannot run.
func (h *Handler) deploy(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != h.opts.Flow.Definition.ID {
		writeError(w, http.StatusNotFound, "unknown flow")
		return
	}
	if h.opts.Modes == nil {
		writeError(w, http.StatusServiceUnavailable, "flow deployment state is unavailable")
		return
	}
	var request deploymentRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid deployment body: "+err.Error())
		return
	}
	request.Mode = strings.TrimSpace(request.Mode)
	switch request.Mode {
	case mode.Off, mode.Shadow:
	case mode.On:
		if !h.opts.OnAvailable {
			writeError(w, http.StatusConflict, "on needs FLOWHUB_DISPATCH=1 and the task service; use off or shadow")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "mode must be off or shadow")
		return
	}
	projects, err := cleanProjects(request.Projects)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor := strings.TrimSpace(os.Getenv("FLOWHUB_OPERATOR"))
	if actor == "" {
		actor = strings.TrimSpace(os.Getenv("USER"))
	}
	if actor == "" {
		actor = "operator"
	}
	if err := h.opts.Modes.Set(h.opts.Flow.Definition.ID, request.Mode, projects, actor); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	entry := h.entry()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": h.opts.Flow.Definition.ID, "mode": entry.Mode,
		"projects": entry.Projects, "updated_at": entry.UpdatedAt, "updated_by": entry.UpdatedBy,
	})
}

func cleanProjects(values []string) ([]string, error) {
	cleaned := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("project keys must not be empty")
		}
		if len(value) > 64 {
			return nil, fmt.Errorf("project key %q is longer than 64 characters", value)
		}
		for _, r := range value {
			if !(r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return nil, fmt.Errorf("project key %q contains an unsupported character", value)
			}
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		cleaned = append(cleaned, value)
	}
	return cleaned, nil
}

func (h *Handler) runs(w http.ResponseWriter, r *http.Request) {
	if h.opts.Runs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runs": []any{}, "next_cursor": ""})
		return
	}
	list, err := h.opts.Runs.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	flowID := r.URL.Query().Get("flow")
	state := r.URL.Query().Get("state")
	limit := queryInt(r, "limit", 50, 1, 200)
	cursor := r.URL.Query().Get("cursor")

	filtered := make([]*engine.Run, 0, len(list))
	for _, run := range list {
		if flowID != "" && run.Flow != flowID {
			continue
		}
		if state != "" && string(run.State) != state {
			continue
		}
		filtered = append(filtered, run)
	}
	if cursor != "" {
		index := -1
		for at, run := range filtered {
			if run.ID == cursor {
				index = at
				break
			}
		}
		if index >= 0 {
			filtered = filtered[index+1:]
		}
	}
	next := ""
	if len(filtered) > limit {
		filtered = filtered[:limit]
		next = filtered[len(filtered)-1].ID
	}
	summaries := make([]any, 0, len(filtered))
	for _, run := range filtered {
		summaries = append(summaries, h.summarize(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": summaries, "next_cursor": next})
}

func (h *Handler) run(w http.ResponseWriter, r *http.Request) {
	if h.opts.Runs == nil {
		writeError(w, http.StatusNotFound, "unknown run")
		return
	}
	run, err := h.opts.Runs.Load(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "unknown run")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	response := map[string]any{"run": run}
	if h.opts.Compare != nil {
		if comparison, ok := h.opts.Compare.Get(run.Event.ID); ok {
			response["comparison"] = comparison
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// cancel stops an active run. A terminal run is 409, never a silent no-op: the
// operator asked for a state change that cannot happen. A run that is not active
// in this process is also 409, because a different process cannot stop it.
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	if h.opts.Cancel == nil {
		writeError(w, http.StatusServiceUnavailable, "run cancellation is unavailable in this process")
		return
	}
	run, err := h.opts.Cancel(r.PathValue("id"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"run": run})
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, "unknown run")
	case errors.Is(err, engine.ErrRunNotActive), errors.Is(err, engine.ErrRunNotCancelable):
		state := ""
		if run != nil {
			state = string(run.State)
		}
		writeError(w, http.StatusConflict, "run is not cancelable"+stateSuffix(state))
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func stateSuffix(state string) string {
	if state == "" {
		return ""
	}
	return " (state " + state + ")"
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	if h.opts.Runs == nil {
		writeError(w, http.StatusNotFound, "unknown run")
		return
	}
	after := queryInt(r, "after", 0, 0, 1<<30)
	limit := queryInt(r, "limit", 200, 1, 500)
	events, err := h.opts.Runs.Events(r.PathValue("id"), after, limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "unknown run")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := after
	if len(events) > 0 {
		if seq, ok := events[len(events)-1]["seq"].(float64); ok {
			next = int(seq)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next": next})
}

func (h *Handler) flowSummary(last *engine.Run) map[string]any {
	triggers := make([]any, 0, len(h.opts.Flow.Triggers))
	for _, trigger := range h.opts.Flow.Triggers {
		triggers = append(triggers, map[string]any{
			"id": trigger.Trigger.ID, "event": trigger.Trigger.Event,
			"exclusive": trigger.Trigger.Exclusive, "priority": trigger.Trigger.Priority,
		})
	}
	entry := h.entry()
	summary := map[string]any{
		"id":           h.opts.Flow.Definition.ID,
		"name":         h.opts.Flow.Definition.Name,
		"builtin":      h.opts.Flow.Definition.Builtin,
		"upstream":     h.opts.Flow.Definition.Upstream,
		"revision":     h.revision(),
		"mode":         entry.Mode,
		"projects":     entry.Projects,
		"updated_at":   entry.UpdatedAt,
		"updated_by":   entry.UpdatedBy,
		"on_available": h.opts.OnAvailable,
		"triggers":     triggers,
		"last_run":     nil,
	}
	if last != nil {
		summary["last_run"] = h.summarize(last)
	}
	return summary
}

func (h *Handler) lastRun() *engine.Run {
	if h.opts.Runs == nil {
		return nil
	}
	list, err := h.opts.Runs.List()
	if err != nil || len(list) == 0 {
		return nil
	}
	return list[0]
}

func (h *Handler) entry() mode.Entry {
	entry := mode.Entry{Mode: mode.Off}
	if h.opts.Modes == nil {
		return entry
	}
	if current, ok := h.opts.Modes.Entry(h.opts.Flow.Definition.ID); ok {
		return current
	}
	return entry
}

func (h *Handler) revision() string {
	sum := sha256.Sum256(h.opts.Definition)
	return fmt.Sprintf("%x", sum[:])
}

func (h *Handler) summarize(run *engine.Run) map[string]any {
	summary := runSummary(run)
	if h.opts.Compare != nil {
		if comparison, ok := h.opts.Compare.Get(run.Event.ID); ok {
			summary["compared"] = true
			summary["match"] = comparison.Match
			summary["differences"] = comparison.Differences
		}
	}
	return summary
}

func runSummary(run *engine.Run) map[string]any {
	action := ""
	if step := run.Steps["evaluate"]; step != nil {
		if value, ok := step.Output["action"].(string); ok {
			action = value
		}
	}
	duration := int64(0)
	if run.FinishedAt != nil {
		duration = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	}
	return map[string]any{
		"id":          run.ID,
		"flow":        run.Flow,
		"state":       run.State,
		"mode":        run.Mode,
		"trigger":     run.Trigger,
		"event_kind":  run.Event.Kind,
		"action":      action,
		"task":        run.Event.Subject.Key,
		"started_at":  run.StartedAt,
		"finished_at": run.FinishedAt,
		"duration_ms": duration,
		"error":       run.Error,
	}
}

func queryInt(r *http.Request, name string, fallback, min, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
