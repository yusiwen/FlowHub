package engine

import (
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/upstream"
)

// State is a run's lifecycle position.
type State string

const (
	StateQueued         State = "queued"
	StateRunning        State = "running"
	StateWaiting        State = "waiting"
	StateNeedsAttention State = "needs_attention"
	StateSucceeded      State = "succeeded"
	StateFailed         State = "failed"
	StateCanceled       State = "canceled"
)

// StepState is one step's lifecycle position.
type StepState string

const (
	StepPending     StepState = "pending"
	StepRunning     StepState = "running"
	StepWaiting     StepState = "waiting"
	StepSucceeded   StepState = "succeeded"
	StepFailed      StepState = "failed"
	StepSkipped     StepState = "skipped"
	StepInterrupted StepState = "interrupted"
)

// Step is one node's execution in a run.
type Step struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	State      StepState      `json:"state"`
	Attempt    int            `json:"attempt"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	Output     map[string]any `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// Run is one execution with the definition and event it started from.
type Run struct {
	ID         string           `json:"id"`
	Flow       string           `json:"flow"`
	Revision   string           `json:"revision"`
	Mode       string           `json:"mode"`
	State      State            `json:"state"`
	Trigger    string           `json:"trigger"`
	Event      upstream.Event   `json:"event"`
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt *time.Time       `json:"finished_at,omitempty"`
	Steps      map[string]*Step `json:"steps"`
	Error      string           `json:"error,omitempty"`
}

func newRunID(now time.Time) string {
	return "run_" + strings.TrimPrefix(upstream.NewID(now), "evt_")
}

func (r *Run) finish(state State, now time.Time) {
	r.State = state
	r.FinishedAt = &now
}

func (s *Step) finish(state StepState, now time.Time) {
	s.State = state
	s.FinishedAt = &now
}
