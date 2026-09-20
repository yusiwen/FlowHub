package store

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/yusiwen/flowhub/internal/metrics"
)

// Async decouples the HTTP handler from the audit writer.
//
// The webhook must be acknowledged in well under 200ms because the published app
// blocks the YouTrack UI on a synchronous POST with a 5s timeout. Record is
// therefore non-blocking: when the queue is full the record is dropped and
// counted instead of slowing the response down.
type Async struct {
	inner Recorder
	log   *slog.Logger
	stats *metrics.Counters

	ch    chan *Record
	depth atomic.Int64
	wg    sync.WaitGroup
	state atomic.Int32 // 0 = open, 1 = draining, 2 = closed
}

const (
	stateOpen int32 = iota
	stateDraining
	stateClosed
)

// NewAsync starts the writer goroutine. size is the queue depth.
func NewAsync(inner Recorder, size int, log *slog.Logger, stats *metrics.Counters) *Async {
	a := &Async{
		inner: inner,
		log:   log,
		stats: stats,
		ch:    make(chan *Record, size),
	}
	a.wg.Add(1)
	go a.loop()
	return a
}

// Record enqueues one audit record without blocking.
func (a *Async) Record(rec *Record) {
	if a.state.Load() != stateOpen {
		a.stats.Dropped.Add(1)
		return
	}
	select {
	case a.ch <- rec:
		a.depth.Add(1)
	default:
		a.stats.Dropped.Add(1)
		a.log.Error("audit queue full, dropping record",
			"event", rec.Event,
			"accepted", rec.Accepted,
			"reason", rec.Reason)
	}
}

// QueueDepth returns the number of records waiting to be written.
func (a *Async) QueueDepth() int64 {
	return a.depth.Load()
}

// Close stops accepting records, drains the queue and closes the inner recorder.
func (a *Async) Close(ctx context.Context) error {
	if !a.state.CompareAndSwap(stateOpen, stateDraining) {
		return nil
	}
	close(a.ch)

	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		a.state.Store(stateClosed)
		return a.inner.Close()
	case <-ctx.Done():
		a.state.Store(stateClosed)
		return ctx.Err()
	}
}

func (a *Async) loop() {
	defer a.wg.Done()
	for rec := range a.ch {
		a.depth.Add(-1)
		if err := a.inner.Record(rec); err != nil {
			a.stats.PersistErrors.Add(1)
			a.log.Error("audit write failed", "error", err, "event", rec.Event)
			continue
		}
		a.stats.Persisted.Add(1)
	}
}
