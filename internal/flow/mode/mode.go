// Package mode stores the deployment state of each flow: off, shadow or on.
//
// The definition is immutable; this is the operator's switch. It lives next to the
// runs rather than in the configuration file, so toggling a flow is a state write
// and never a configuration reload. The file is re-read when its mtime changes, so
// a CLI click is visible to the running service without a restart.
package mode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Version is the state-file format.
const Version = 1

// Modes.
const (
	Off    = "off"
	Shadow = "shadow"
	On     = "on"
)

// Entry is one flow's deployment.
type Entry struct {
	Mode string `json:"mode"`
	// Projects scopes an `on` deployment: empty means every routed project.
	// `shadow` and `off` ignore it, but it is kept so a later `on` keeps its
	// selected projects.
	Projects  []string  `json:"projects,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// State is the on-disk shape.
type State struct {
	Version int              `json:"version"`
	Flows   map[string]Entry `json:"flows"`
}

// Store reads and writes <dataDir>/flows/state.json.
type Store struct {
	path string

	mu        sync.Mutex
	state     State
	modTime   time.Time
	exists    bool
	lastError string
}

// Path is the state file for one data directory.
func Path(dataDir string) string {
	return filepath.Join(dataDir, "flows", "state.json")
}

// Open loads the state, creating the directory. A missing file means every flow
// is off.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("mode: a data directory is required")
	}
	dir := filepath.Dir(Path(dataDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mode: create %s: %w", dir, err)
	}
	store := &Store{path: Path(dataDir), state: State{Version: Version, Flows: map[string]Entry{}}}
	store.mu.Lock()
	store.reloadLocked()
	store.mu.Unlock()
	return store, nil
}

// Mode reports one flow's deployment, defaulting to off.
func (s *Store) Mode(id string) string {
	if s == nil {
		return Off
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	entry, ok := s.state.Flows[id]
	if !ok {
		return Off
	}
	switch entry.Mode {
	case Off, Shadow, On:
		return entry.Mode
	default:
		return Off
	}
}

// Entry reports one flow's deployment and whether it has a state row.
func (s *Store) Entry(id string) (Entry, bool) {
	if s == nil {
		return Entry{Mode: Off}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	entry, ok := s.state.Flows[id]
	if !ok {
		return Entry{Mode: Off}, false
	}
	entry.Projects = append([]string(nil), entry.Projects...)
	return entry, true
}

// Entries copies the current state.
func (s *Store) Entries() map[string]Entry {
	if s == nil {
		return map[string]Entry{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	out := make(map[string]Entry, len(s.state.Flows))
	for id, entry := range s.state.Flows {
		entry.Projects = append([]string(nil), entry.Projects...)
		out[id] = entry
	}
	return out
}

// Set writes one flow's mode and project scope atomically. The mode package
// accepts `on` so a future executable build can read a file written by a newer
// one; the CLI and the API refuse it until the task service exists.
func (s *Store) Set(id, mode string, projects []string, by string) error {
	if s == nil {
		return fmt.Errorf("mode: no store")
	}
	if id == "" {
		return fmt.Errorf("mode: a flow id is required")
	}
	switch mode {
	case Off, Shadow, On:
	default:
		return fmt.Errorf("mode: %q is not one of off, shadow, on", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	if s.state.Flows == nil {
		s.state.Flows = map[string]Entry{}
	}
	cleaned := make([]string, 0, len(projects))
	seen := map[string]bool{}
	for _, project := range projects {
		project = strings.TrimSpace(project)
		if project == "" || seen[project] {
			continue
		}
		seen[project] = true
		cleaned = append(cleaned, project)
	}
	s.state.Version = Version
	s.state.Flows[id] = Entry{Mode: mode, Projects: cleaned, UpdatedAt: time.Now().UTC(), UpdatedBy: by}
	return s.writeLocked()
}

// Error reports the last reload failure, if any. The running mode stays off when
// the file cannot be read; the health view says why.
func (s *Store) Error() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	return s.lastError
}

func (s *Store) reloadLocked() {
	info, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			if s.exists {
				s.state = State{Version: Version, Flows: map[string]Entry{}}
				s.exists = false
				s.modTime = time.Time{}
			}
			s.lastError = ""
			return
		}
		s.lastError = fmt.Sprintf("stat %s: %v", s.path, err)
		return
	}
	if s.exists && info.ModTime().Equal(s.modTime) {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		s.lastError = fmt.Sprintf("read %s: %v", s.path, err)
		return
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		s.lastError = fmt.Sprintf("decode %s: %v", s.path, err)
		return
	}
	if state.Flows == nil {
		state.Flows = map[string]Entry{}
	}
	s.state = state
	s.modTime = info.ModTime()
	s.exists = true
	s.lastError = ""
}

func (s *Store) writeLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("mode: marshal state: %w", err)
	}
	dir := filepath.Dir(s.path)
	temp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return fmt.Errorf("mode: write %s: %w", temp, err)
	}
	if err := os.Rename(temp, s.path); err != nil {
		return fmt.Errorf("mode: publish %s: %w", s.path, err)
	}
	info, err := os.Stat(s.path)
	if err == nil {
		s.modTime = info.ModTime()
		s.exists = true
	}
	s.lastError = ""
	return nil
}
