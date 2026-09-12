package testutil

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"relayllm/internal/events"
	"relayllm/internal/types"
)

// FakeProvider implements types.Provider. Tests script the events the
// provider will emit when SendMessage is called. The fake holds no real
// subprocess and imposes no timing — events are dispatched synchronously on
// the goroutine SendMessage was called from.
//
// Typical flow:
//
//	p := testutil.NewFakeProvider(handler)
//	p.ScriptText("hello world")
//	p.ScriptResult("end_turn", types.SessionStats{...})
//	// later, the session calls p.SendMessage("user input", nil)
//	// the handler fires synchronously for each scripted event
type FakeProvider struct {
	handler types.EventHandler

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
	Files []types.FileAttachment
}

func NewFakeProvider(handler types.EventHandler) *FakeProvider {
	return &FakeProvider{handler: handler}
}

// SetHandler replaces the event handler. Useful when the provider is constructed
// before the consuming SessionManager is ready.
func (p *FakeProvider) SetHandler(h types.EventHandler) { p.handler = h }

func (p *FakeProvider) Start() error { return nil }

func (p *FakeProvider) SendMessage(text string, files []types.FileAttachment) error {
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
	p.ScriptEvent(events.HandlerLLMEvent, map[string]interface{}{
		"type": "message_start",
		"v":    2,
	})
	p.ScriptEvent(events.HandlerLLMEvent, map[string]interface{}{
		"type":          "content_block_start",
		"v":             2,
		"index":         0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	p.ScriptEvent(events.HandlerLLMEvent, map[string]interface{}{
		"type":  "content_block_delta",
		"v":     2,
		"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	})
	p.ScriptEvent(events.HandlerLLMEvent, map[string]interface{}{
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
func (p *FakeProvider) ScriptResult(stopReason string, stats types.SessionStats) {
	// stats_update payload is the raw SessionStats JSON, not wrapped.
	statsData, err := json.Marshal(stats)
	if err != nil {
		panic(fmt.Sprintf("fake provider: marshal stats: %v", err))
	}
	p.mu.Lock()
	p.queue = append(p.queue, scriptedEvent{eventType: events.HandlerStatsUpdate, data: statsData})
	p.mu.Unlock()

	p.ScriptEvent(events.HandlerLLMEvent, map[string]interface{}{
		"type":        "result",
		"subtype":     "stop",
		"v":           2,
		"stop_reason": stopReason,
	})
	p.ScriptEvent(events.HandlerMessageComplete, nil)
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
