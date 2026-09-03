package simtest

import (
	"sync"
	"time"
)

// Clock is a deterministic manual clock for tests. It never sleeps and
// never touches wall time after construction.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a Clock fixed at start.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d (d must be non-negative) and
// returns the new time.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d < 0 {
		panic("simtest: Clock.Advance with negative duration")
	}
	c.now = c.now.Add(d)
	return c.now
}
