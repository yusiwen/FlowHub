//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpencode is a minimal v1 protocol server for failure-path tests. It posts a
// reply only when the mode says so; a silent first turn exercises the dispatcher's
// one nudge, and an always-silent turn must become failed, never awaiting_input.
type fakeOpencode struct {
	mu         sync.Mutex
	mode       string // "nudge" posts on prompt 2+, "silent" never posts
	prompts    map[string][]string
	nextID     int
	delay      time.Duration
	hold       chan struct{}
	failPrompt bool
}

func newFakeOpencode(t *testing.T) *fakeOpencode {
	t.Helper()
	return &fakeOpencode{mode: "silent", prompts: map[string][]string{}}
}

func (f *fakeOpencode) setMode(mode string) {
	f.mu.Lock()
	f.mode = mode
	f.mu.Unlock()
}

func (f *fakeOpencode) setDelay(delay time.Duration) {
	f.mu.Lock()
	f.delay = delay
	f.mu.Unlock()
}

func (f *fakeOpencode) setHold(hold chan struct{}) {
	f.mu.Lock()
	f.hold = hold
	f.mu.Unlock()
}

func (f *fakeOpencode) release() {
	f.mu.Lock()
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		default:
			close(hold)
		}
	}
}

func (f *fakeOpencode) setFailPrompt(fail bool) {
	f.mu.Lock()
	f.failPrompt = fail
	f.mu.Unlock()
}

func (f *fakeOpencode) start() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]any{"healthy": true, "version": "fake"})
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.nextID++
		id := "ses_fake_" + strconv.Itoa(f.nextID)
		f.mu.Unlock()
		writeFakeJSON(w, map[string]any{"id": id, "directory": "/tmp/fake-worktree"})
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/session/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		id, action := parts[0], parts[1]
		switch action {
		case "prompt_async":
			f.prompt(w, id, r)
		case "message":
			f.messages(w, id)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/session/status", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		status := map[string]any{}
		for id := range f.prompts {
			status[id] = map[string]any{"type": "idle"}
		}
		f.mu.Unlock()
		writeFakeJSON(w, status)
	})
	mux.HandleFunc("/permission", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, []any{})
	})
	return httptest.NewServer(mux)
}

func (f *fakeOpencode) prompt(w http.ResponseWriter, id string, r *http.Request) {
	var body struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	text := ""
	if len(body.Parts) > 0 {
		text = body.Parts[0].Text
	}
	f.mu.Lock()
	f.prompts[id] = append(f.prompts[id], text)
	delay, hold := f.delay, f.hold
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if hold != nil {
		<-hold
	}
	f.mu.Lock()
	fail := f.failPrompt
	f.mu.Unlock()
	if fail {
		http.Error(w, "fake prompt failure", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeOpencode) messages(w http.ResponseWriter, id string) {
	f.mu.Lock()
	count := len(f.prompts[id])
	mode := f.mode
	f.mu.Unlock()
	var out []any
	for i := 0; i < count; i++ {
		out = append(out, map[string]any{
			"info": map[string]any{"id": fmt.Sprintf("u-%s-%d", id, i), "role": "user", "time": map[string]any{"created": i}},
		})
		parts := []any{map[string]any{"type": "text", "text": "analysis done"}}
		reply := mode == "nudge" && i >= 1
		if reply {
			parts = append(parts, map[string]any{
				"type": "tool", "tool": "youtrack_add_issue_comment",
				"state": map[string]any{"status": "completed"},
			})
		}
		out = append(out, map[string]any{
			"info": map[string]any{
				"id": fmt.Sprintf("a-%s-%d", id, i), "role": "assistant", "finish": "stop",
				"cost": 0.001, "tokens": map[string]any{"input": 10, "output": 2},
				"time": map[string]any{"created": i, "completed": i + 1},
			},
			"parts": parts,
		})
	}
	writeFakeJSON(w, out)
}

func (f *fakeOpencode) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, prompts := range f.prompts {
		total += len(prompts)
	}
	return total
}

func writeFakeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
