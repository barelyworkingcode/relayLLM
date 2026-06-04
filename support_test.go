package main

// Shared test-only infrastructure. Lives in the same package so it can wire
// directly into production types (SessionManager, PermissionManager, etc.)
// without exposing test seams in the production API.
//
// Contents:
//   - FakeClock          — controllable Clock for permission-timeout etc.
//   - FakeMCPClient      — controllable MCPClient for tool-loop tests
//   - FakeProvider       — controllable Provider for session/WS/API tests
//   - support helpers    — small assertion / waiting utilities

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ----------------------------------------------------------------------------
// FakeClock
// ----------------------------------------------------------------------------

// FakeClock is a Clock whose time only advances when Advance() is called.
// After() returns a channel that fires the moment Advance crosses the
// requested duration. Sleep() blocks until the same condition. Now() and
// Since() always reflect the simulated current instant.
//
// All public methods are safe for concurrent use.
type FakeClock struct {
	mu       sync.Mutex
	now      time.Time
	waiters  []*fakeWaiter
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

// ----------------------------------------------------------------------------
// FakeMCPClient
// ----------------------------------------------------------------------------

// FakeMCPClient is an MCPClient driven by scripted responses. Tests register
// tool definitions and a handler for each tool name; the chat tool loop calls
// CallTool exactly as it would against a real MCPManager.
type FakeMCPClient struct {
	mu       sync.Mutex
	tools    []FakeTool
	calls    []FakeMCPCall
	started  bool
}

// FakeTool is a single scripted tool entry.
type FakeTool struct {
	Name        string
	Description string
	Schema      map[string]interface{} // JSON schema; nil if no params
	Handler     func(args json.RawMessage) (string, error)
}

// FakeMCPCall records one CallTool invocation for assertions.
type FakeMCPCall struct {
	Name string
	Args json.RawMessage
}

func NewFakeMCPClient(tools ...FakeTool) *FakeMCPClient {
	return &FakeMCPClient{tools: tools}
}

func (f *FakeMCPClient) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}

func (f *FakeMCPClient) HasTools() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tools) > 0
}

func (f *FakeMCPClient) ToolCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tools)
}

func (f *FakeMCPClient) ChatToolDefs() []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tools) == 0 {
		return nil
	}
	defs := make([]map[string]interface{}, 0, len(f.tools))
	for _, t := range f.tools {
		fn := map[string]interface{}{
			"name":        t.Name,
			"description": t.Description,
		}
		if t.Schema != nil {
			fn["parameters"] = t.Schema
		}
		defs = append(defs, map[string]interface{}{
			"type":     "function",
			"function": fn,
		})
	}
	return defs
}

func (f *FakeMCPClient) CallTool(ctx context.Context, name string, args json.RawMessage, onProgress func(message string)) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, FakeMCPCall{Name: name, Args: append(json.RawMessage(nil), args...)})
	var handler func(json.RawMessage) (string, error)
	for _, t := range f.tools {
		if t.Name == name {
			handler = t.Handler
			break
		}
	}
	f.mu.Unlock()
	if handler == nil {
		return "", fmt.Errorf("fake mcp: no handler for tool %q", name)
	}
	return handler(args)
}

func (f *FakeMCPClient) ToolNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.tools))
	for _, t := range f.tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

func (f *FakeMCPClient) ServerNames() []string {
	if f.HasTools() {
		return []string{"fake"}
	}
	return nil
}

func (f *FakeMCPClient) Close() {}

// Calls returns a snapshot of all CallTool invocations.
func (f *FakeMCPClient) Calls() []FakeMCPCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeMCPCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// ----------------------------------------------------------------------------
// FakeProvider
// ----------------------------------------------------------------------------

// FakeProvider implements Provider. Tests script the events the provider will
// emit when SendMessage is called. The fake holds no real subprocess and
// imposes no timing — events are dispatched synchronously on the goroutine
// SendMessage was called from, unless ScriptAsync is used.
//
// Typical flow:
//
//	p := NewFakeProvider(handler)
//	p.ScriptText("hello world")
//	p.ScriptResult("end_turn", SessionStats{...})
//	// later, the session calls p.SendMessage("user input", nil)
//	// the handler fires synchronously for each scripted event
type FakeProvider struct {
	handler EventHandler

	mu      sync.Mutex
	queue   []scriptedEvent
	sent    []FakeSend
	state   json.RawMessage
	stopped atomic.Bool
	killed  atomic.Bool
	deleted atomic.Bool
}

type scriptedEvent struct {
	eventType string
	data      json.RawMessage
}

// FakeSend records one SendMessage call.
type FakeSend struct {
	Text  string
	Files []FileAttachment
}

func NewFakeProvider(handler EventHandler) *FakeProvider {
	return &FakeProvider{handler: handler}
}

// SetHandler replaces the event handler. Useful when the provider is constructed
// before the consuming SessionManager is ready.
func (p *FakeProvider) SetHandler(h EventHandler) { p.handler = h }

func (p *FakeProvider) Start() error { return nil }

func (p *FakeProvider) SendMessage(text string, files []FileAttachment) error {
	p.mu.Lock()
	p.sent = append(p.sent, FakeSend{Text: text, Files: files})
	queue := p.queue
	p.queue = nil
	p.mu.Unlock()

	for _, ev := range queue {
		if p.stopped.Load() {
			return nil
		}
		p.handler(ev.eventType, ev.data)
	}
	return nil
}

func (p *FakeProvider) StopGeneration()           { p.stopped.Store(true) }
func (p *FakeProvider) Kill()                     { p.killed.Store(true) }
func (p *FakeProvider) DeleteSession() error      { p.deleted.Store(true); return nil }
func (p *FakeProvider) Alive() bool               { return !p.killed.Load() }
func (p *FakeProvider) GetState() json.RawMessage { return p.state }
func (p *FakeProvider) RestoreState(s json.RawMessage) {
	p.state = append(json.RawMessage(nil), s...)
}

// --- Scripting API ---

// ScriptEvent enqueues an arbitrary canonical event. The other Script*
// helpers wrap this with the right type/payload.
func (p *FakeProvider) ScriptEvent(eventType string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("fake provider: marshal event %q: %v", eventType, err))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(p.queue, scriptedEvent{eventType: eventType, data: data})
}

// ScriptText emits a complete assistant text turn: message_start →
// content_block_start(text) → content_block_delta → content_block_stop.
func (p *FakeProvider) ScriptText(text string) {
	p.ScriptEvent(HandlerLLMEvent, map[string]interface{}{
		"type": "message_start",
		"v":    2,
	})
	p.ScriptEvent(HandlerLLMEvent, map[string]interface{}{
		"type":  "content_block_start",
		"v":     2,
		"index": 0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	p.ScriptEvent(HandlerLLMEvent, map[string]interface{}{
		"type":  "content_block_delta",
		"v":     2,
		"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	})
	p.ScriptEvent(HandlerLLMEvent, map[string]interface{}{
		"type":               "content_block_stop",
		"v":                  2,
		"index":              0,
		"content_block_stop": true,
	})
}

// ScriptResult emits the same sequence a real provider does at end-of-turn:
// a stats_update event (so the session's Stats field and the collector both
// see the token counts), a result envelope (stop_reason), then
// message_complete to flush.
func (p *FakeProvider) ScriptResult(stopReason string, stats SessionStats) {
	// stats_update payload is the raw SessionStats JSON, not wrapped.
	statsData, err := json.Marshal(stats)
	if err != nil {
		panic(fmt.Sprintf("fake provider: marshal stats: %v", err))
	}
	p.mu.Lock()
	p.queue = append(p.queue, scriptedEvent{eventType: HandlerStatsUpdate, data: statsData})
	p.mu.Unlock()

	p.ScriptEvent(HandlerLLMEvent, map[string]interface{}{
		"type":        "result",
		"subtype":     "stop",
		"v":           2,
		"stop_reason": stopReason,
	})
	p.ScriptEvent(HandlerMessageComplete, nil)
}

// Sent returns a snapshot of all SendMessage calls.
func (p *FakeProvider) Sent() []FakeSend {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]FakeSend, len(p.sent))
	copy(out, p.sent)
	return out
}

// Stopped reports whether StopGeneration was called.
func (p *FakeProvider) Stopped() bool { return p.stopped.Load() }

// Killed reports whether Kill was called.
func (p *FakeProvider) Killed() bool { return p.killed.Load() }

// Deleted reports whether DeleteSession was called.
func (p *FakeProvider) Deleted() bool { return p.deleted.Load() }

// ----------------------------------------------------------------------------
// Small assertion helpers
// ----------------------------------------------------------------------------

// waitFor polls cond every 5 ms until it returns true or the timeout elapses.
// Tests use this when a state change is driven by a goroutine they don't
// directly control (e.g. event delivery through a channel).
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition not met within %v", timeout)
}
