package testutil

import (
	"sync"
	"time"
)

// FakeClock is a Clock whose time only advances when Advance() is called.
// After() returns a channel that fires the moment Advance crosses the
// requested duration. Sleep() blocks until the same condition. Now() and
// Since() always reflect the simulated current instant.
//
// All public methods are safe for concurrent use.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

// NewFakeClock returns a clock anchored at start. Use zero time if you don't
// care about the absolute value.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) Since(t time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(t)
}

func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, &fakeWaiter{deadline: c.now.Add(d), ch: ch})
	return ch
}

func (c *FakeClock) Sleep(d time.Duration) {
	<-c.After(d)
}

// Waiters returns the number of outstanding After/Sleep waiters. Tests use
// this to confirm a goroutine has entered its timeout select before
// advancing the clock.
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// Advance moves the simulated clock forward by d. Any waiter whose deadline
// falls at or before the new time is fired with that exact instant.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	kept := c.waiters[:0]
	var fire []*fakeWaiter
	for _, w := range c.waiters {
		if !w.deadline.After(now) {
			fire = append(fire, w)
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
	c.mu.Unlock()
	for _, w := range fire {
		w.ch <- now
	}
}
