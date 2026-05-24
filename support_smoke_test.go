package main

// Smoke test for the testsupport infrastructure itself. If this fails, every
// downstream test using TestServer/FakeProvider/etc. is broken — keep it
// minimal and quick.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestSupport_FakeClock_AdvanceFiresAfter(t *testing.T) {
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

func TestSupport_FakeProvider_EmitsScriptedEvents(t *testing.T) {
	var got []string
	p := NewFakeProvider(func(eventType string, _ json.RawMessage) {
		got = append(got, eventType)
	})
	p.ScriptText("hi")
	p.ScriptResult("end_turn", SessionStats{})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.SendMessage("hello", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	wantPrefix := []string{
		HandlerLLMEvent,    // message_start
		HandlerLLMEvent,    // content_block_start
		HandlerLLMEvent,    // content_block_delta
		HandlerLLMEvent,    // content_block_stop
		HandlerStatsUpdate, // emitted by ScriptResult
		HandlerLLMEvent,    // result envelope
		HandlerMessageComplete,
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

func TestSupport_TestServer_AuthEnforced(t *testing.T) {
	srv := NewTestServer(t, nil)

	// With no Authorization header, /api/status must 401.
	req, err := http.NewRequest("GET", srv.HTTP.URL+"/api/status", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth: got %d, want 401", resp.StatusCode)
	}

	// With the test bearer, the same call succeeds.
	var status map[string]interface{}
	resp2 := srv.GetJSON("/api/status", &status)
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("authed: got %d, want 200", resp2.StatusCode)
	}
	if _, ok := status["uptimeSeconds"]; !ok {
		t.Errorf("status missing uptimeSeconds: %v", status)
	}
}

func TestSupport_TestServer_EndToEndFakeProvider(t *testing.T) {
	srv := NewTestServer(t, nil)
	fp := srv.SetFakeProvider()

	fp.ScriptText("pong")
	fp.ScriptResult("end_turn", SessionStats{InputTokens: 3, OutputTokens: 1})

	conn := srv.DialWS()
	sessionID := srv.CreateSession(nil)

	WSSend(t, conn, map[string]interface{}{
		"type":      "join_session",
		"sessionId": sessionID,
	})
	ReadUntilType(t, conn, "session_joined", 2*time.Second)

	WSSend(t, conn, map[string]interface{}{
		"type":      "send_message",
		"sessionId": sessionID,
		"text":      "ping",
	})

	// Wait for the message_complete signal that the session emits when the
	// provider's scripted stream finishes.
	ReadUntilType(t, conn, HandlerMessageComplete, 2*time.Second)

	if sent := fp.Sent(); len(sent) != 1 || sent[0].Text != "ping" {
		t.Errorf("fake provider Sent: %+v", sent)
	}
}
