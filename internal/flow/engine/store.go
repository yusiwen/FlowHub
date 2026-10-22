package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Store is the on-disk run journal: one directory per run, an atomically replaced
// meta.json and an append-only journal.jsonl.
type Store struct {
	dir string

	mu  sync.Mutex
	seq map[string]int
}

// NewStore creates <dataDir>/runs.
func NewStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("engine: a data directory is required")
	}
	dir := filepath.Join(dataDir, "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("engine: create %s: %w", dir, err)
	}
	return &Store{dir: dir, seq: map[string]int{}}, nil
}

// Dir is the runs directory.
func (s *Store) Dir() string { return s.dir }

// Create writes the initial meta and an empty journal.
func (s *Store) Create(run *Run) error {
	runDir := filepath.Join(s.dir, run.ID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return fmt.Errorf("engine: create run %s: %w", run.ID, err)
	}
	if err := s.writeMeta(run); err != nil {
		return err
	}
	journal := filepath.Join(runDir, "journal.jsonl")
	file, err := os.OpenFile(journal, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("engine: create journal %s: %w", run.ID, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("engine: close journal %s: %w", run.ID, err)
	}
	s.mu.Lock()
	s.seq[run.ID] = 0
	s.mu.Unlock()
	return nil
}

// Save replaces the run meta atomically.
func (s *Store) Save(run *Run) error {
	return s.writeMeta(run)
}

// Append writes one journal event.
func (s *Store) Append(runID, kind string, fields map[string]any) error {
	s.mu.Lock()
	s.seq[runID]++
	seq := s.seq[runID]
	s.mu.Unlock()

	entry := make(map[string]any, len(fields)+3)
	for key, value := range fields {
		entry[key] = value
	}
	entry["seq"] = seq
	entry["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	entry["type"] = kind
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("engine: marshal journal event: %w", err)
	}
	path := filepath.Join(s.dir, runID, "journal.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("engine: open journal %s: %w", runID, err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("engine: write journal %s: %w", runID, err)
	}
	return nil
}

// Load reads one run's meta.
func (s *Store) Load(runID string) (*Run, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, runID, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("engine: read run %s: %w", runID, err)
	}
	var run Run
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("engine: decode run %s: %w", runID, err)
	}
	if run.Steps == nil {
		run.Steps = map[string]*Step{}
	}
	return &run, nil
}

// List reads every run meta, newest first.
func (s *Store) List() ([]*Run, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("engine: read %s: %w", s.dir, err)
	}
	runs := make([]*Run, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		run, err := s.Load(entry.Name())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(a, b int) bool {
		return runs[a].StartedAt.After(runs[b].StartedAt)
	})
	return runs, nil
}

// Recover marks runs that were in flight when the process stopped. A shadow run
// cannot be resumed safely: its definition and event are known, but the point of
// interruption is exactly what needs a human to look at it.
func (s *Store) Recover() (int, error) {
	runs, err := s.List()
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, run := range runs {
		switch run.State {
		case StateQueued, StateRunning, StateWaiting:
		default:
			continue
		}
		now := time.Now().UTC()
		for _, step := range run.Steps {
			if step.State != StepRunning {
				continue
			}
			step.State = StepInterrupted
			step.Error = "interrupted by a restart; inspect before retrying"
			step.FinishedAt = &now
			if err := s.Append(run.ID, "step.interrupted", map[string]any{"step": step.ID}); err != nil {
				return recovered, err
			}
		}
		run.State = StateNeedsAttention
		run.Error = "interrupted by a restart; inspect before retrying"
		run.FinishedAt = &now
		if err := s.Save(run); err != nil {
			return recovered, err
		}
		if err := s.Append(run.ID, "run.interrupted", map[string]any{"reason": run.Error}); err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

func (s *Store) writeMeta(run *Run) error {
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("engine: marshal run %s: %w", run.ID, err)
	}
	runDir := filepath.Join(s.dir, run.ID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return fmt.Errorf("engine: create run %s: %w", run.ID, err)
	}
	temp := filepath.Join(runDir, "meta.json.tmp")
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return fmt.Errorf("engine: write run %s: %w", run.ID, err)
	}
	if err := os.Rename(temp, filepath.Join(runDir, "meta.json")); err != nil {
		return fmt.Errorf("engine: publish run %s: %w", run.ID, err)
	}
	return nil
}
