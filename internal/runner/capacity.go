package runner

import (
	"context"
	"sync"
)

// Capacity bounds how many turns may run on a runtime at once while reporting the
// in-flight count /healthz shows.
//
// The dispatcher's scheduler already decides how many workers a runtime gets; this
// is the shared gate behind them, so a flow run and a dispatch turn cannot exceed
// the same breadth. A limit of zero or less means "not set": the count is still
// kept, which is what the selection policy and /healthz read.
type Capacity struct {
	mu       sync.Mutex
	inFlight map[string]int
	limits   map[string]int
	waiters  map[string][]chan struct{}
}

// NewCapacity returns an empty gate.
func NewCapacity() *Capacity {
	return &Capacity{
		inFlight: map[string]int{},
		limits:   map[string]int{},
		waiters:  map[string][]chan struct{}{},
	}
}

// SetLimit sets the breadth of one runtime. A non-positive limit disables the
// bound but keeps counting.
func (c *Capacity) SetLimit(name string, limit int) {
	if c == nil || name == "" {
		return
	}
	c.mu.Lock()
	c.limits[name] = limit
	waiters := c.waiters[name]
	c.waiters[name] = nil
	c.mu.Unlock()
	// Every waiter re-checks the new limit; a raised limit frees a slot, a lowered
	// one sends them back to sleep.
	for _, waiter := range waiters {
		signal(waiter)
	}
}

// Acquire blocks until a turn slot on name is free, or ctx is done. The returned
// release is idempotent; a caller must always call it.
func (c *Capacity) Acquire(ctx context.Context, name string) (func(), error) {
	if c == nil || name == "" {
		return func() {}, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			// A waiter that was already signalled may have consumed the slot's wake-up;
			// pass it on so another waiter is not stranded.
			c.mu.Lock()
			c.wake(name)
			c.mu.Unlock()
			return nil, err
		}
		c.mu.Lock()
		limit := c.limits[name]
		if limit <= 0 || c.inFlight[name] < limit {
			c.inFlight[name]++
			c.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() { c.release(name) })
			}, nil
		}
		waiter := make(chan struct{}, 1)
		c.waiters[name] = append(c.waiters[name], waiter)
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.removeWaiter(name, waiter)
			c.wake(name)
			c.mu.Unlock()
			return nil, ctx.Err()
		case <-waiter:
		}
	}
}

// InFlight reports the turns currently holding a slot on name.
func (c *Capacity) InFlight(name string) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight[name]
}

// Snapshot copies the in-flight counters for /healthz.
func (c *Capacity) Snapshot() map[string]int {
	if c == nil {
		return map[string]int{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.inFlight))
	for name, count := range c.inFlight {
		out[name] = count
	}
	return out
}

func (c *Capacity) release(name string) {
	c.mu.Lock()
	if c.inFlight[name] > 0 {
		c.inFlight[name]--
	}
	if c.inFlight[name] == 0 {
		delete(c.inFlight, name)
	}
	c.wake(name)
	c.mu.Unlock()
}

func (c *Capacity) wake(name string) {
	if len(c.waiters[name]) == 0 {
		return
	}
	waiter := c.waiters[name][0]
	c.waiters[name] = c.waiters[name][1:]
	signal(waiter)
}

func (c *Capacity) removeWaiter(name string, waiter chan struct{}) {
	waiters := c.waiters[name]
	for index, candidate := range waiters {
		if candidate == waiter {
			c.waiters[name] = append(waiters[:index], waiters[index+1:]...)
			return
		}
	}
}

func signal(waiter chan struct{}) {
	select {
	case waiter <- struct{}{}:
	default:
	}
}
