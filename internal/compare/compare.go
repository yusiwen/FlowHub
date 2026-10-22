// Package compare joins dispatch's decision and the flow's shadow decision for
// the same event. It is the migration instrument: a run is not evidence until the
// two orchestrators agreed on it, or the difference is written down.
package compare

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Decision is the decision one orchestrator made for one event.
type Decision struct {
	EventID  string    `json:"event_id"`
	Source   string    `json:"source"`
	IssueID  string    `json:"issue_id"`
	Project  string    `json:"project,omitempty"`
	Action   string    `json:"action"`
	Reason   string    `json:"reason,omitempty"`
	Routable bool      `json:"routable"`
	At       time.Time `json:"at"`
}

// Comparison is the joined result.
type Comparison struct {
	EventID     string    `json:"event_id"`
	Dispatch    Decision  `json:"dispatch"`
	Flow        Decision  `json:"flow"`
	Match       bool      `json:"match"`
	Differences []string  `json:"differences,omitempty"`
	At          time.Time `json:"at"`
}

type pair struct {
	dispatch *Decision
	flow     *Decision
}

// Store keeps pending and completed comparisons. Completed comparisons are
// appended to <DataDir>/flow-compare.jsonl so a restart does not reset the
// cutover counters.
type Store struct {
	path string

	mu         sync.Mutex
	pending    map[string]*pair
	results    map[string]Comparison
	order      []string
	compared   int64
	mismatches int64
	file       *os.File
}

const maxPending = 4096
const maxResults = 4096

// Open creates the comparison file and loads the completed results.
func Open(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("compare: a data directory is required")
	}
	path := filepath.Join(dataDir, "flow-compare.jsonl")
	store := &Store{
		path:    path,
		pending: map[string]*pair{},
		results: map[string]Comparison{},
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("compare: open %s: %w", path, err)
	}
	store.file = file
	return store, nil
}

// Path is the append-only comparison file.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// ObserveDispatch records dispatch's side. It is called from the dispatch worker.
func (s *Store) ObserveDispatch(decision Decision) {
	s.observe(false, decision)
}

// ObserveFlow records the shadow flow's side.
func (s *Store) ObserveFlow(decision Decision) {
	s.observe(true, decision)
}

func (s *Store) observe(flow bool, decision Decision) {
	if s == nil || strings.TrimSpace(decision.EventID) == "" || strings.TrimSpace(decision.Action) == "" {
		return
	}
	if decision.At.IsZero() {
		decision.At = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.pending[decision.EventID]
	if !ok {
		if len(s.pending) >= maxPending {
			// An event whose other side never arrives must not grow without bound.
			for id := range s.pending {
				delete(s.pending, id)
				break
			}
		}
		entry = &pair{}
		s.pending[decision.EventID] = entry
	}
	if flow {
		entry.flow = &decision
	} else {
		entry.dispatch = &decision
	}
	if entry.dispatch == nil || entry.flow == nil {
		return
	}
	comparison := join(*entry.dispatch, *entry.flow)
	s.results[decision.EventID] = comparison
	s.order = append(s.order, decision.EventID)
	if len(s.order) > maxResults {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.results, oldest)
	}
	delete(s.pending, decision.EventID)
	s.compared++
	if !comparison.Match {
		s.mismatches++
	}
	if s.file != nil {
		if data, err := json.Marshal(comparison); err == nil {
			_, _ = s.file.Write(append(data, '\n'))
		}
	}
}

// Get returns the completed comparison for one event.
func (s *Store) Get(eventID string) (Comparison, bool) {
	if s == nil {
		return Comparison{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	comparison, ok := s.results[eventID]
	return comparison, ok
}

// Counts returns the completed comparisons and how many differed.
func (s *Store) Counts() (compared, mismatches int64) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compared, s.mismatches
}

// Pending returns how many events are waiting for the other side.
func (s *Store) Pending() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// Close closes the append file.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func (s *Store) load() error {
	file, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("compare: read %s: %w", s.path, err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		var comparison Comparison
		if err := json.Unmarshal(scanner.Bytes(), &comparison); err != nil || comparison.EventID == "" {
			continue
		}
		s.results[comparison.EventID] = comparison
		s.order = append(s.order, comparison.EventID)
		s.compared++
		if !comparison.Match {
			s.mismatches++
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("compare: read %s: %w", s.path, err)
	}
	if len(s.order) > maxResults {
		drop := len(s.order) - maxResults
		for _, id := range s.order[:drop] {
			delete(s.results, id)
		}
		s.order = s.order[drop:]
	}
	return nil
}

func join(dispatch, flow Decision) Comparison {
	comparison := Comparison{
		EventID: dispatch.EventID, Dispatch: dispatch, Flow: flow,
		At: time.Now().UTC(),
	}
	var differences []string
	if dispatch.Action != flow.Action {
		differences = append(differences, "action")
	}
	// Two orchestrators that both refused to route the same project agree, even
	// though each names its own routing table in the reason.
	routingAgreement := !dispatch.Routable && !flow.Routable &&
		dispatch.Action == "ignore" && flow.Action == "ignore"
	if !routingAgreement && !reasonMatches(dispatch.Reason, flow.Reason) {
		differences = append(differences, "reason")
	}
	comparison.Differences = differences
	comparison.Match = len(differences) == 0
	return comparison
}

func reasonMatches(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}
