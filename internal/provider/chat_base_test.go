package provider

// Regression coverage for the SendMessage/StopGeneration locking fix: stop
// must be able to interrupt a SendMessage that is blocked inside a slow
// AcquireBackend (simulating a cold managed-server launch), rather than
// queuing up behind it on the shared mutex.

import (
	"context"
	"encoding/json"
	"relayllm/internal/types"
	"testing"
	"time"
)

// slowAcquireTransport wraps FakeChatTransport and blocks in AcquireBackend
// until either the passed context is cancelled or the test unblocks it.
type slowAcquireTransport struct {
	*FakeChatTransport
	acquireStarted chan struct{}
	unblock        chan struct{}
}

func newSlowAcquireTransport() *slowAcquireTransport {
	return &slowAcquireTransport{
		FakeChatTransport: NewFakeChatTransport(),
		acquireStarted:    make(chan struct{}),
		unblock:           make(chan struct{}),
	}
}

func (t *slowAcquireTransport) AcquireBackend(ctx context.Context) (func(), error) {
	close(t.acquireStarted)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.unblock:
		return func() {}, nil
	}
}

// TestStopGeneration_DoesNotBlockOnSlowAcquire verifies StopGeneration
// returns promptly while SendMessage is stuck inside AcquireBackend, and
// that cancelling it there unblocks SendMessage instead of leaving it (and
// every future StopGeneration/SendMessage call) hung on the shared mutex.
func TestStopGeneration_DoesNotBlockOnSlowAcquire(t *testing.T) {
	transport := newSlowAcquireTransport()
	transport.QueueTextTurn("unused")

	session := &types.Session{ID: "slow-acquire-test", Messages: []types.Message{}}
	provider := NewBaseChatProvider(session, func(string, json.RawMessage) {}, transport, nil, nil)
	if err := provider.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	sendErr := make(chan error, 1)
	go func() { sendErr <- provider.SendMessage("hi", nil) }()

	select {
	case <-transport.acquireStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for AcquireBackend to start")
	}

	stopDone := make(chan struct{})
	go func() {
		provider.StopGeneration()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StopGeneration blocked on the mutex held by SendMessage's in-flight AcquireBackend")
	}

	select {
	case err := <-sendErr:
		if err == nil {
			t.Fatal("expected SendMessage to fail once its context was cancelled by StopGeneration")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendMessage never returned after its context was cancelled")
	}

	if n := transport.CallCount(); n != 0 {
		t.Errorf("expected StreamChunks never to be called (AcquireBackend never succeeded), got %d calls", n)
	}
}

// TestSendMessage_SupersededByStop_DoesNotSpawnToolLoop verifies that if a
// StopGeneration bumps the generation counter while SendMessage is blocked
// inside AcquireBackend, SendMessage notices it was superseded once the
// acquire completes and never starts runToolLoop.
func TestSendMessage_SupersededByStop_DoesNotSpawnToolLoop(t *testing.T) {
	transport := newSlowAcquireTransport()
	transport.QueueTextTurn("unused")

	session := &types.Session{ID: "superseded-test", Messages: []types.Message{}}
	provider := NewBaseChatProvider(session, func(string, json.RawMessage) {}, transport, nil, nil)
	if err := provider.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	sendErr := make(chan error, 1)
	go func() { sendErr <- provider.SendMessage("hi", nil) }()

	select {
	case <-transport.acquireStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for AcquireBackend to start")
	}

	// Simulate a StopGeneration landing while AcquireBackend is in flight:
	// bump generation without touching cancelFn, then let the acquire
	// succeed. SendMessage must notice it was superseded and never reach
	// PostChat/runToolLoop.
	provider.generation.Add(1)
	close(transport.unblock)

	select {
	case err := <-sendErr:
		if err != nil {
			t.Fatalf("expected the superseded SendMessage to return nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendMessage never returned")
	}

	if n := transport.CallCount(); n != 0 {
		t.Errorf("expected the superseded call to skip runToolLoop entirely, got %d StreamChunks calls", n)
	}
}
