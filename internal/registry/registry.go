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

// DefaultSource is the source a row written before the source seam is attributed
// to.
//
// It is a fact about history rather than a preference of this package: every row in
// an existing registry was produced by the only adapter FlowHub had, so reading such
// a row as YouTrack is what it meant when it was written. The field is added to the
// row the next time it is written, so the file migrates as it is used rather than
// being rewritten.
const DefaultSource = "youtrack"

// Task is one tracker item being worked on.
type Task struct {
	// Source names the adapter this task belongs to, e.g. "youtrack". Together with
	// Key it is the task's identity: two trackers may well have a project with the
	// same key, and a task must never be continued on the wrong one.
	Source string `json:"source,omitempty"`
	Key    string `json:"task_key"`
	Repo   string `json:"repo"`
	// Runtime is the agent host this task is bound to. A session cannot move
	// between hosts, so the binding is for the task's life: a task whose runtime is
	// gone is refused rather than re-homed. Empty means "not yet chosen".
	Runtime  string `json:"runtime,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	// BaseCommit is the commit the task started from, resolved once through the
	// origin when the task was created and never re-resolved. That is what keeps a
	// long-running task's patches reviewable against a fixed base even if the origin
	// moves, and what makes two hosts agree on where the task began.
	BaseCommit string `json:"base_commit,omitempty"`
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
	// its replies through an MCP server as the same tracker user as the human, so
	// identity cannot separate them: a comment that repeats this text is
	// recognised as our own by content instead.
	LastReply string `json:"last_reply,omitempty"`
	// LastAction is the action of the most recent turn, so a human's permit can
	// continue where the agent was stopped instead of guessing from the plan state.
	LastAction string `json:"last_action,omitempty"`
	// Refusals are the permission requests the most recent turn was refused for.
	// They are replaced every turn because they are what a bare `/opencode permit`
	// authorises: the commands the human has just read about, and nothing else.
	Refusals []Refusal `json:"refusals,omitempty"`
	// Grant is the authorisation a human gave by name, applying to this task's later
	// turns until the task is done or the permit is revoked. Nil means none.
	Grant     *Grant    `json:"grant,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Refusal is one permission request the arbiter refused, kept so that a human's
// authorisation is exactly the command they read in the agent's comment rather than
// something the automation inferred.
type Refusal struct {
	Command string `json:"command"`
	Reason  string `json:"reason,omitempty"`
}

// Grant is the shell authorisation a human gave one task by commenting the permit
// phrase. It is a list of *literal* command segments: authorising `rtk git status`
// does not authorise `rtk git status --porcelain`, and it never authorises anything
// the runtime's deny list refuses.
type Grant struct {
	Commands []string  `json:"commands"`
	By       string    `json:"by,omitempty"`
	At       time.Time `json:"at"`
}

// Registry is a concurrency-safe, file-backed task table.
//
// Tasks are indexed by (source, key), so a YouTrack `TEST-17` and a Gitea
// `owner/repo#42` — or a Gitea project that also happens to be called TEST — are
// different tasks. The index key never appears in the file: the file keeps `source`
// and `task_key` as separate fields, which is what makes a row readable and the
// migration from a pre-seam row trivial.
type Registry struct {
	path  string
	mu    sync.Mutex
	tasks map[string]*Task
}

// indexKey is the map key for one task. The separator cannot appear in a source name
// or a task key, so no pair can collide with another.
func indexKey(source, key string) string {
	return strings.ToLower(strings.TrimSpace(source)) + "\x00" + key
}

// Qualified renders the task's identity the way an operator writes it, for a log or
// the control API. Falls back to the bare key for a row with no source.
func (t Task) Qualified() string {
	if strings.TrimSpace(t.Source) == "" {
		return t.Key
	}
	return t.Source + ":" + t.Key
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
func (r *Registry) Get(source, key string) (Task, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[indexKey(source, key)]
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
	if strings.TrimSpace(task.Source) == "" {
		return errors.New("registry: task source is required")
	}
	if strings.TrimSpace(task.Key) == "" {
		return errors.New("registry: task key is required")
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	task.UpdatedAt = time.Now().UTC()
	stored := task
	r.tasks[indexKey(task.Source, task.Key)] = &stored
	return r.append(task)
}

// Update applies mutate to the stored task and saves the result. It is the only
// way to change a task, so every write goes through one code path.
func (r *Registry) Update(source, key string, mutate func(*Task)) (Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[indexKey(source, key)]
	if !ok {
		return Task{}, fmt.Errorf("%w: %s:%s", ErrTaskNotFound, source, key)
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
func (r *Registry) Ensure(source, key string, seed func(*Task)) (Task, bool, error) {
	if task, ok := r.Get(source, key); ok {
		return task, false, nil
	}
	task := Task{Source: source, Key: key, State: StateAnalyzing, Plan: PlanNone}
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
		if strings.TrimSpace(task.Source) == "" {
			// A row written before the source seam existed. Reading it as YouTrack is
			// what it meant when it was written; the field appears the next time the
			// task is written, so the file migrates as it is used rather than needing
			// a rewrite.
			task.Source = DefaultSource
		}
		stored := task
		r.tasks[indexKey(task.Source, task.Key)] = &stored
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("registry: scan %s: %w", r.path, err)
	}
	return nil
}
