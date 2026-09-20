// Package metrics holds the process-wide counters exposed by /healthz.
//
// The receiver must answer in a few milliseconds, so counting is lock-free
// except for the per-reason rejection breakdown, which is only touched on the
// (rare) rejection path.
package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Counters aggregates receiver activity since process start.
type Counters struct {
	// Received counts every request that reached the handler.
	Received atomic.Int64
	// Accepted counts deliveries that passed every lock, the replay window and
	// deduplication.
	Accepted atomic.Int64
	// Rejected counts deliveries dropped for any reason (including duplicates).
	Rejected atomic.Int64
	// Duplicates counts deliveries dropped by the idempotency cache.
	Duplicates atomic.Int64
	// Persisted counts audit records successfully written to the store.
	Persisted atomic.Int64
	// PersistErrors counts audit writes that failed.
	PersistErrors atomic.Int64
	// Dropped counts audit records discarded because the queue was full or the
	// process was shutting down.
	Dropped atomic.Int64

	mu      sync.Mutex
	reasons map[string]int64
}

// New returns ready-to-use counters.
func New() *Counters {
	return &Counters{reasons: make(map[string]int64)}
}

// Reject records one rejected delivery under a stable reason label.
func (c *Counters) Reject(reason string) {
	c.Rejected.Add(1)
	c.mu.Lock()
	c.reasons[reason]++
	c.mu.Unlock()
}

// Snapshot renders the counters as a JSON friendly map.
func (c *Counters) Snapshot() map[string]any {
	c.mu.Lock()
	reasons := make(map[string]int64, len(c.reasons))
	for k, v := range c.reasons {
		reasons[k] = v
	}
	c.mu.Unlock()

	return map[string]any{
		"received":       c.Received.Load(),
		"accepted":       c.Accepted.Load(),
		"rejected":       c.Rejected.Load(),
		"duplicates":     c.Duplicates.Load(),
		"persisted":      c.Persisted.Load(),
		"persist_errors": c.PersistErrors.Load(),
		"dropped":        c.Dropped.Load(),
		"reject_reasons": reasons,
	}
}

// ReasonLabels returns the observed rejection reasons, sorted.
func (c *Counters) ReasonLabels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.reasons))
	for k := range c.reasons {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
