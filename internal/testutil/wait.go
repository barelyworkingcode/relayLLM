package testutil

import (
	"testing"
	"time"
)

// WaitFor polls cond every 5 ms until it returns true or the timeout elapses.
// Tests use this when a state change is driven by a goroutine they don't
// directly control (e.g. event delivery through a channel).
func WaitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("WaitFor: condition not met within %v", timeout)
}
