// Package registry remembers which opencode session belongs to which task, and
// how far the task has come.
//
// opencode generates session IDs and does not let a caller choose one, so this
// table is the only way to keep "one task, one session" (design document §8).
// It is append-only JSONL: every Put writes a full snapshot, and opening the file
// replays them last-write-wins. That is crash safe without a database, greppable
// by hand, and consistent with the audit store.
package registry

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is where a task is in its lifecycle.
type State string

const (
	// StateAnalyzing: the read-only analysis turn is running.
	StateAnalyzing State = "analyzing"
	// StateAwaitingInput: analysis is posted and a human must answer or approve.
	StateAwaitingInput State = "awaiting_input"
	// StatePlanning: asked to execute, but there is no plan yet, so a planning
	// turn is running.
	StatePlanning State = "planning"
	// StateExecuting: the plan is approved and the execution turn is running.
	StateExecuting State = "executing"
	// StateDone: the last turn finished and nothing is pending.
	StateDone State = "done"
	// StateFailed: the last turn failed; a human decides what happens next.
	StateFailed State = "failed"
)

// PlanState tracks whether the task has a plan to execute.
type PlanState string

const (
	// PlanNone: no plan has been produced yet.
	PlanNone PlanState = "none"
	// PlanDraft: a plan exists but a human has not approved it.
	PlanDraft PlanState = "draft"
	// PlanConfirmed: a human approved the plan; execution may proceed.
	PlanConfirmed PlanState = "confirmed"
)

// Task is one YouTrack issue being worked on.
type Task struct {
	Key      string `json:"task_key"`
	Repo     string `json:"repo"`
	Worktree string `json:"worktree,omitempty"`
	// SessionID is the opencode session this task runs in. Empty until the first
	// turn has created one.
	SessionID string    `json:"session_id,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	State     State     `json:"state"`
	Plan      PlanState `json:"plan_state"`
	// Turns counts the prompts delivered for this task; it is the cheap guard
	// against a runaway loop.
	Turns int     `json:"turns"`
	Cost  float64 `json:"cost"`
	// LastReply is the last assistant text we saw for this task. The agent posts
	// its replies through an MCP server as the same YouTrack user as the human, so
	// identity cannot separate them: a comment that repeats this text is
	// recognised as our own by content instead.
	LastReply string    `json:"last_reply,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Registry is a concurrency-safe, file-backed task table.
type Registry struct {
	path  string
	mu    sync.Mutex
	tasks map[string]*Task
}

// ErrTaskNotFound is returned by Update for an unknown task.
var ErrTaskNotFound = errors.New("registry: task not found")

// Open loads the registry, creating the file's directory when needed. A missing
// file is not an error: it simply means no task has run yet.
func Open(path string) (*Registry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("registry: path is required")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("registry: create %s: %w", dir, err)
		}
	}
	r := &Registry{path: path, tasks: map[string]*Task{}}
	if err := r.replay(); err != nil {
		return nil, err
	}
	return r, nil
}

// Path returns the backing file.
func (r *Registry) Path() string { return r.path }

// Len returns the number of known tasks.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tasks)
}

// Get returns a copy of one task.
func (r *Registry) Get(key string) (Task, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[key]
	if !ok {
		return Task{}, false
	}
	return *task, true
}

// List returns every task, newest first.
func (r *Registry) List() []Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Task, 0, len(r.tasks))
	for _, task := range r.tasks {
		out = append(out, *task)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Put creates or REPLACES a task with the given snapshot, stamping the
// timestamps. It is a whole-record write: a field left empty in the argument is
// cleared. Use Update for a partial change, which is what almost every caller
// wants.
func (r *Registry) Put(task Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(task.Key) == "" {
		return errors.New("registry: task key is required")
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	task.UpdatedAt = time.Now().UTC()
	stored := task
	r.tasks[task.Key] = &stored
	return r.append(task)
}

// Update applies mutate to the stored task and saves the result. It is the only
// way to change a task, so every write goes through one code path.
func (r *Registry) Update(key string, mutate func(*Task)) (Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[key]
	if !ok {
		return Task{}, fmt.Errorf("%w: %s", ErrTaskNotFound, key)
	}
	mutate(task)
	task.UpdatedAt = time.Now().UTC()
	stored := *task
	if err := r.append(stored); err != nil {
		return Task{}, err
	}
	return stored, nil
}

// Ensure returns the task, creating it with the given seed when it is unknown.
func (r *Registry) Ensure(key string, seed func(*Task)) (Task, bool, error) {
	if task, ok := r.Get(key); ok {
		return task, false, nil
	}
	task := Task{Key: key, State: StateAnalyzing, Plan: PlanNone}
	if seed != nil {
		seed(&task)
	}
	return task, true, r.Put(task)
}

func (r *Registry) append(task Task) error {
	line, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("registry: encode task %s: %w", task.Key, err)
	}
	file, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("registry: open %s: %w", r.path, err)
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("registry: write %s: %w", r.path, err)
	}
	return file.Sync()
}

// replay rebuilds the in-memory table. A malformed line is an error rather than
// a silent skip: forgetting a session would make FlowHub start a second one for
// the same task, which is exactly what this table exists to prevent.
func (r *Registry) replay() error {
	file, err := os.Open(r.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("registry: read %s: %w", r.path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var task Task
		if err := json.Unmarshal([]byte(raw), &task); err != nil {
			return fmt.Errorf("registry: %s line %d is not valid JSON: %w", r.path, line, err)
		}
		if task.Key == "" {
			return fmt.Errorf("registry: %s line %d has no task_key", r.path, line)
		}
		stored := task
		r.tasks[task.Key] = &stored
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("registry: scan %s: %w", r.path, err)
	}
	return nil
}
