package dispatch

import (
	"context"
	"sync"
)

// taskID is a task's identity inside the scheduler: the source that produced it and
// its key. It is the unit that must never be in two turns at once, because a task
// owns exactly one session, and a prompt sent to a busy session is silently
// swallowed. The runtime is a capacity, not the lock (ADR 0003).
type taskID struct {
	source string
	key    string
}

// runtimeScheduler is one runtime's work: the deliveries routed to it, grouped by
// task, served by up to `max` workers.
//
// The state machine is the exclusion mechanism — nothing holds a lock across a turn,
// so a worker never blocks on another task's turn, which is what a per-task mutex
// taken after dequeueing would do:
//
//   - `pending` holds the deliveries waiting for their task's previous turn;
//   - `ready` lists the tasks with pending work and no turn running, oldest first;
//   - `listed` says which of them is already in `ready`, so a task appears once;
//   - `running` says which tasks a worker is serving right now.
//
// The invariant, asserted by the tests: a task is in `listed` at most once, in
// `running` at most once, never in both, and a running task may also have pending
// deliveries — which is exactly the case that must wait rather than be dropped.
type runtimeScheduler struct {
	name string
	// max is how many distinct tasks may run turns at the same time. It is fixed when
	// the scheduler is created: shrinking a live pool means deciding what happens to
	// the turns it already started, and that decision is not worth making today
	// (ADR 0003 §6).
	max int
	// limit bounds the deliveries this runtime accepts, across all its tasks. It is
	// the existing per-runtime queue bound: a per-task bound would let one task's
	// flood sit in memory indefinitely.
	limit int

	mu      sync.Mutex
	pending map[taskID][]routed
	ready   []taskID
	listed  map[taskID]bool
	running map[taskID]bool
	queued  int

	// wake carries one token per state change that might have made work available.
	// It is written with a non-blocking send, so a token may be dropped when the slot
	// is full — which is safe: a full slot means a token exists, and whoever consumes
	// it looks for work *after* consuming it, so the work that was signalled cannot be
	// stranded.
	wake chan struct{}
}

func newRuntimeScheduler(name string, max, limit int) *runtimeScheduler {
	if max < 1 {
		max = 1
	}
	return &runtimeScheduler{
		name:    name,
		max:     max,
		limit:   limit,
		pending: map[taskID][]routed{},
		listed:  map[taskID]bool{},
		running: map[taskID]bool{},
		wake:    make(chan struct{}, 1),
	}
}

// add accepts a delivery, or refuses it when this runtime already holds as many as
// it is allowed to. A refusal is the caller's signal to count a drop, which is what
// keeps a full host from growing without bound.
func (s *runtimeScheduler) add(id taskID, item routed) bool {
	s.mu.Lock()
	if s.limit > 0 && s.queued >= s.limit {
		s.mu.Unlock()
		return false
	}
	s.pending[id] = append(s.pending[id], item)
	s.queued++
	// A task whose turn is running is deliberately not listed: the delivery's turn
	// starts when that one finishes, and `finish` lists it again.
	if !s.running[id] && !s.listed[id] {
		s.ready = append(s.ready, id)
		s.listed[id] = true
	}
	s.mu.Unlock()
	s.signal()
	return true
}

// next returns the head delivery of the oldest task that is ready to run, or false
// once the context is cancelled. The task is marked running before the caller sees
// it, so a second worker cannot pick up the same task.
func (s *runtimeScheduler) next(ctx context.Context) (routed, bool) {
	for {
		if item, ok := s.take(); ok {
			return item, true
		}
		select {
		case <-ctx.Done():
			return routed{}, false
		case <-s.wake:
		}
	}
}

// take is the non-blocking half of next: it pops one delivery and marks its task as
// running. It reports false when nothing is ready, which is the caller's signal to
// wait for a token.
func (s *runtimeScheduler) take() (routed, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.ready) > 0 {
		id := s.ready[0]
		s.ready = s.ready[1:]
		delete(s.listed, id)
		items := s.pending[id]
		if len(items) == 0 {
			// Unreachable while the invariant holds; kept so a future edit that
			// breaks it skips the task instead of returning an empty delivery.
			continue
		}
		item := items[0]
		if len(items) == 1 {
			delete(s.pending, id)
		} else {
			s.pending[id] = items[1:]
		}
		s.queued--
		s.running[id] = true
		return item, true
	}
	return routed{}, false
}

// finish releases a task after its turn and reports whether the task's work has
// drained. A drained task is forgotten by the scheduler, which is also when the
// dispatcher may forget the routing decision it cached for that task.
func (s *runtimeScheduler) finish(id taskID) bool {
	s.mu.Lock()
	delete(s.running, id)
	remaining := len(s.pending[id])
	if remaining > 0 && !s.listed[id] {
		s.ready = append(s.ready, id)
		s.listed[id] = true
	}
	s.mu.Unlock()
	if remaining > 0 {
		s.signal()
	}
	return remaining == 0
}

// idle reports whether this task has nothing queued and no turn running here.
func (s *runtimeScheduler) idle(id taskID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.running[id] && len(s.pending[id]) == 0
}

// signal wakes one waiting worker if possible. It never blocks: the caller is a
// worker or the intake loop, neither of which may wait on the other.
func (s *runtimeScheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// busy reports the work committed to this runtime: deliveries accepted and not yet
// started, plus the tasks being served. `spread` ranks by it, so a delivery that has
// been accepted but not started still counts — that is what keeps a burst from
// landing entirely on the host that happens to sort first.
func (s *runtimeScheduler) busy() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queued + len(s.running)
}

// runningCount reports how many tasks this runtime is serving right now.
func (s *runtimeScheduler) runningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

// snapshot renders the scheduler for /healthz. `queued` counts deliveries accepted
// and not yet started, `running` counts tasks being served, `tasks` counts the
// distinct tasks involved — a task with a turn running *and* a delivery waiting is one
// task, not two — and the breadth travels with them so one call answers "is this host
// doing what it was configured to do".
func (s *runtimeScheduler) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	withWork := make(map[taskID]struct{}, len(s.running)+len(s.pending))
	for id := range s.running {
		withWork[id] = struct{}{}
	}
	for id, items := range s.pending {
		if len(items) > 0 {
			withWork[id] = struct{}{}
		}
	}
	return map[string]any{
		"queued":  s.queued,
		"running": len(s.running),
		"tasks":   len(withWork),
		// busy is the number the ranking reads: everything committed to this host.
		"busy":           s.queued + len(s.running),
		"max_concurrent": s.max,
	}
}
