package testutil

// Smoke test for the testutil fakes themselves. If this fails, every
// downstream test using FakeClock/FakeBridge/etc. is broken — keep it
// minimal and quick.

import (
	"testing"
	"time"
)

func TestFakeClock_AdvanceFiresAfter(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	ch := c.After(100 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("After fired prematurely")
	default:
	}
	c.Advance(50 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("After fired before deadline")
	default:
	}
	c.Advance(50 * time.Millisecond)
	select {
	case <-ch:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("After did not fire after Advance crossed deadline")
	}
}
