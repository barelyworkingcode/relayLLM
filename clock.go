package main

import "time"

// Clock abstracts the few timing operations that the production code needs
// callers to be able to fake in tests. Most timestamps in the codebase are
// metrics-only (TTFT, TPS) and use time.Now() directly — those are not worth
// the wiring cost. This interface exists for the timing that materially
// changes program behavior: deadlines on user-facing waits, retry backoffs,
// and lifecycle grace periods.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	After(d time.Duration) <-chan time.Time
	Sleep(d time.Duration)
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) Since(t time.Time) time.Duration        { return time.Since(t) }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) Sleep(d time.Duration)                  { time.Sleep(d) }

// DefaultClock is the wall clock. Production code uses this; tests can pass
// a fake to constructors that accept a Clock.
var DefaultClock Clock = realClock{}
