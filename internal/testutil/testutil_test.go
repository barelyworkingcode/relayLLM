package testutil

// Smoke test for the testutil fakes themselves. If this fails, every
// downstream test using FakeClock/FakeProvider/etc. is broken — keep it
// minimal and quick.

import (
	"encoding/json"
	"testing"
	"time"

	"relayllm/internal/events"
	"relayllm/internal/types"
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

func TestFakeProvider_EmitsScriptedEvents(t *testing.T) {
	var got []string
	p := NewFakeProvider(func(eventType string, _ json.RawMessage) {
		got = append(got, eventType)
	})
	p.ScriptText("hi")
	p.ScriptResult("end_turn", types.SessionStats{})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.SendMessage("hello", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	wantPrefix := []string{
		events.HandlerLLMEvent,    // message_start
		events.HandlerLLMEvent,    // content_block_start
		events.HandlerLLMEvent,    // content_block_delta
		events.HandlerLLMEvent,    // content_block_stop
		events.HandlerStatsUpdate, // emitted by ScriptResult
		events.HandlerLLMEvent,    // result envelope
		events.HandlerMessageComplete,
	}
	if len(got) != len(wantPrefix) {
		t.Fatalf("event count: got %d, want %d (events=%v)", len(got), len(wantPrefix), got)
	}
	for i, want := range wantPrefix {
		if got[i] != want {
			t.Errorf("event[%d]: got %q, want %q", i, got[i], want)
		}
	}
	if sent := p.Sent(); len(sent) != 1 || sent[0].Text != "hello" {
		t.Errorf("Sent: %+v", sent)
	}
}
