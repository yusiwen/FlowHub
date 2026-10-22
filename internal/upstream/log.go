package upstream

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yusiwen/flowhub/internal/dedupe"
)

// LogOptions configures the event log.
type LogOptions struct {
	// Dir is the service's data directory; the log lives in <Dir>/events.
	Dir string
	// QueueSize bounds the events waiting for the writer. Record never blocks; a
	// full queue drops and counts, exactly like the audit queue.
	QueueSize int
	// DedupeTTL is how long (upstream, dedupe_key) is remembered. Zero means 24h.
	DedupeTTL time.Duration
	// Retention is how long daily files are kept. Zero means 30 days.
	Retention time.Duration
	Logger    *slog.Logger
	// Now is injectable for tests.
	Now func() time.Time
}

// Log is an append-only, per-day event store.
//
// One writer goroutine owns the file, so events keep their queue order. The
// dedupe cache is checked on the writer, which makes two identical deliveries
// collapse even when they arrive before the first is written.
type Log struct {
	opts LogOptions
	dir  string
	ch   chan Event

	pending sync.WaitGroup
	wg      sync.WaitGroup

	sendMu sync.Mutex
	closed bool

	dedupe *dedupe.Cache

	mu      sync.Mutex
	file    *os.File
	fileDay string

	subMu       sync.Mutex
	subscribers []func(Event)

	recorded    atomic.Int64
	dropped     atomic.Int64
	duplicates  atomic.Int64
	invalid     atomic.Int64
	writeErrors atomic.Int64
	lastEvent   atomic.Int64

	errorMu   sync.Mutex
	lastError string
}

// NewLog creates the event directory and starts the writer.
func NewLog(opts LogOptions) (*Log, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, fmt.Errorf("upstream: an event log needs a data directory")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 256
	}
	if opts.DedupeTTL <= 0 {
		opts.DedupeTTL = 24 * time.Hour
	}
	if opts.Retention <= 0 {
		opts.Retention = 30 * 24 * time.Hour
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	dir := filepath.Join(opts.Dir, "events")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("upstream: create %s: %w", dir, err)
	}
	log := &Log{
		opts:   opts,
		dir:    dir,
		ch:     make(chan Event, opts.QueueSize),
		dedupe: dedupe.New(opts.DedupeTTL),
	}
	log.prune()
	log.wg.Add(1)
	go log.loop()
	return log, nil
}

// Dir is the directory the daily files live in.
func (l *Log) Dir() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// Path is today's file, whether or not an event has been written yet.
func (l *Log) Path() string {
	if l == nil {
		return ""
	}
	return filepath.Join(l.dir, l.opts.Now().UTC().Format("2006-01-02")+".jsonl")
}

// Subscribe registers a listener for appended events. The listener runs on the
// writer goroutine and must not block; the flow engine submits to its own queue.
// Events written before the call are not replayed.
func (l *Log) Subscribe(listener func(Event)) {
	if l == nil || listener == nil {
		return
	}
	l.subMu.Lock()
	l.subscribers = append(l.subscribers, listener)
	l.subMu.Unlock()
}

// Record enqueues one event. It validates the envelope, assigns an id and a
// received time when absent, and never blocks: a full queue drops and counts.
func (l *Log) Record(event Event) {
	if l == nil {
		return
	}
	if err := event.Validate(); err != nil {
		l.invalid.Add(1)
		l.setError(err.Error())
		return
	}
	// The id and the received time are assigned on the writer: Record runs after the
	// webhook response, but it still must not call crypto/rand or any other I/O.
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	if l.closed {
		l.dropped.Add(1)
		return
	}
	l.pending.Add(1)
	select {
	case l.ch <- event:
	default:
		l.pending.Done()
		l.dropped.Add(1)
		l.setError("event queue is full")
	}
}

// Flush waits until every event enqueued before the call has been handled. It is
// for tests and shutdown, not for the delivery path.
func (l *Log) Flush() {
	if l == nil {
		return
	}
	l.pending.Wait()
}

// Close stops the writer after draining the queue and closes the file. It is
// idempotent.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.sendMu.Lock()
	if l.closed {
		l.sendMu.Unlock()
		return nil
	}
	l.closed = true
	close(l.ch)
	l.sendMu.Unlock()

	l.wg.Wait()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Snapshot is the /healthz projection.
func (l *Log) Snapshot() map[string]any {
	if l == nil {
		return map[string]any{"enabled": false}
	}
	last := ""
	if nanos := l.lastEvent.Load(); nanos != 0 {
		last = time.Unix(0, nanos).UTC().Format(time.RFC3339Nano)
	}
	l.errorMu.Lock()
	lastError := l.lastError
	l.errorMu.Unlock()
	return map[string]any{
		"enabled":       true,
		"dir":           l.dir,
		"path":          l.Path(),
		"recorded":      l.recorded.Load(),
		"dropped":       l.dropped.Load(),
		"duplicates":    l.duplicates.Load(),
		"invalid":       l.invalid.Load(),
		"write_errors":  l.writeErrors.Load(),
		"last_event_at": last,
		"last_error":    lastError,
	}
}

func (l *Log) loop() {
	defer l.wg.Done()
	for event := range l.ch {
		l.write(event)
		l.pending.Done()
	}
}

func (l *Log) write(event Event) {
	if event.ID == "" {
		event.ID = NewID(l.opts.Now())
	}
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = l.opts.Now()
	}
	key := event.Upstream + "\x00" + event.DedupeKey
	if l.dedupe.CheckAndAdd(key) {
		l.duplicates.Add(1)
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		l.writeErrors.Add(1)
		l.setError(fmt.Sprintf("marshal event: %v", err))
		return
	}

	day := event.ReceivedAt.UTC().Format("2006-01-02")
	written := false
	l.mu.Lock()
	defer func() {
		l.mu.Unlock()
		if written {
			l.notify(event)
		}
	}()
	if l.file == nil || l.fileDay != day {
		if l.file != nil {
			_ = l.file.Close()
			l.file = nil
		}
		l.prune()
		path := filepath.Join(l.dir, day+".jsonl")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			l.writeErrors.Add(1)
			l.setError(fmt.Sprintf("open %s: %v", path, err))
			return
		}
		l.file = file
		l.fileDay = day
	}
	if _, err := l.file.Write(append(data, '\n')); err != nil {
		l.writeErrors.Add(1)
		l.setError(fmt.Sprintf("write %s: %v", l.file.Name(), err))
		_ = l.file.Close()
		l.file = nil
		return
	}
	l.recorded.Add(1)
	l.lastEvent.Store(event.ReceivedAt.UnixNano())
	l.setError("")
	written = true
}

func (l *Log) notify(event Event) {
	l.subMu.Lock()
	subscribers := append([]func(Event){}, l.subscribers...)
	l.subMu.Unlock()
	for _, listener := range subscribers {
		listener(event)
	}
}

// prune removes daily files older than the retention window. It runs at start-up
// and whenever the writer opens a new day; a failed removal is reported but does
// not stop the log.
func (l *Log) prune() {
	l.dedupe.Sweep()
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		l.setError(fmt.Sprintf("read event directory: %v", err))
		return
	}
	cutoff := l.opts.Now().UTC().Add(-l.opts.Retention)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(entry.Name(), ".jsonl"))
		if err != nil {
			continue
		}
		if day.Before(cutoff) {
			if err := os.Remove(filepath.Join(l.dir, entry.Name())); err != nil {
				l.setError(fmt.Sprintf("remove %s: %v", entry.Name(), err))
			}
		}
	}
}

func (l *Log) setError(message string) {
	l.errorMu.Lock()
	l.lastError = message
	l.errorMu.Unlock()
}
