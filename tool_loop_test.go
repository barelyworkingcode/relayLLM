package main

// Tool loop coverage. Drives BaseChatProvider.runToolLoop against a
// scripted FakeChatTransport + FakeMCPClient, with no real subprocess or
// network. Each test queues one or more "turns" of streamed deltas; the
// fake transport replays them when StreamChunks is called.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ----------------------------------------------------------------------------
// FakeChatTransport
// ----------------------------------------------------------------------------

// fakeTurn is the script for one call of StreamChunks.
type fakeTurn struct {
	deltas []ChatDelta
	result NormalizedStreamResult
}

// FakeChatTransport is a programmable ChatTransport for tool-loop tests. Each
// queued turn drives one streaming response: when the chat loop calls
// StreamChunks, the fake pops the next turn, emits its deltas, and returns
// its result.
type FakeChatTransport struct {
	mu     sync.Mutex
	turns  []fakeTurn
	called atomic.Int32
}

func NewFakeChatTransport() *FakeChatTransport { return &FakeChatTransport{} }

// QueueTextTurn enqueues a turn that emits one text delta then ends. The model
// signals "no more tool calls" by returning empty deltas — text alone here.
func (f *FakeChatTransport) QueueTextTurn(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, fakeTurn{
		deltas: []ChatDelta{{Text: text}},
		result: NormalizedStreamResult{FullText: text},
	})
}

// QueueToolCallTurn enqueues a turn where the model emits exactly one tool
// call (start + complete args + end). The chat loop will execute the tool
// then pop the next queued turn.
func (f *FakeChatTransport) QueueToolCallTurn(id, name string, args string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, fakeTurn{
		deltas: []ChatDelta{
			{ToolStart: &ToolStartEvent{Index: 0, ID: id, Name: name}},
			{ToolArgs: &ToolArgsEvent{Index: 0, Partial: args}},
		},
		result: NormalizedStreamResult{},
	})
}

func (f *FakeChatTransport) Name() string                           { return "fake" }
func (f *FakeChatTransport) Ping(ctx context.Context) error         { return nil }
func (f *FakeChatTransport) BuildMessages(_ string, msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{"role": m.Role, "content": string(m.Content)})
	}
	return out
}
func (f *FakeChatTransport) PostChat(_ context.Context, _, _ []map[string]any) (*http.Response, error) {
	return &http.Response{Body: io.NopCloser(strings.NewReader(""))}, nil
}
func (f *FakeChatTransport) StreamChunks(resp *http.Response, _ time.Time, emit func(ChatDelta)) NormalizedStreamResult {
	defer resp.Body.Close()
	f.called.Add(1)
	f.mu.Lock()
	if len(f.turns) == 0 {
		f.mu.Unlock()
		return NormalizedStreamResult{Err: io.EOF}
	}
	turn := f.turns[0]
	f.turns = f.turns[1:]
	f.mu.Unlock()
	for _, d := range turn.deltas {
		emit(d)
	}
	return turn.result
}
func (f *FakeChatTransport) AppendAssistantWithToolCalls(messages []map[string]any, text string, calls []NormalizedToolCall) []map[string]any {
	return append(messages, map[string]any{"role": "assistant", "text": text, "tool_calls": calls})
}
func (f *FakeChatTransport) AppendToolResult(messages []map[string]any, tc NormalizedToolCall, result string) []map[string]any {
	return append(messages, map[string]any{"role": "tool", "tool_call_id": tc.ID, "result": result})
}

// CallCount returns the number of times StreamChunks was invoked — one per
// chat-loop iteration.
func (f *FakeChatTransport) CallCount() int { return int(f.called.Load()) }

// ----------------------------------------------------------------------------
// Driver helpers
// ----------------------------------------------------------------------------

// newToolLoopHarness wires a BaseChatProvider against the fake transport +
// fake MCP. The captured events slice records everything the provider emitted.
type toolLoopHarness struct {
	provider  *BaseChatProvider
	transport *FakeChatTransport
	mcp       *FakeMCPClient
	events    *toolLoopEvents
}

type toolLoopEvents struct {
	mu   sync.Mutex
	rows []toolLoopEvent
}

type toolLoopEvent struct {
	Type string
	Data json.RawMessage
}

func (c *toolLoopEvents) push(t string, d json.RawMessage) {
	c.mu.Lock()
	c.rows = append(c.rows, toolLoopEvent{Type: t, Data: append(json.RawMessage(nil), d...)})
	c.mu.Unlock()
}

func (c *toolLoopEvents) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.rows))
	for i, r := range c.rows {
		out[i] = r.Type
	}
	return out
}

// hasToolResult returns true if any captured llm_event carries a result with
// subtype tool_result for the given tool id.
func (c *toolLoopEvents) hasToolResult(toolUseID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.rows {
		if r.Type != HandlerLLMEvent {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(r.Data, &ev) != nil {
			continue
		}
		if ev.Type == "result" && ev.Subtype == "tool_result" && ev.ToolUseID == toolUseID {
			return true
		}
	}
	return false
}

func newToolLoopHarness(t *testing.T, mcpTools ...FakeTool) *toolLoopHarness {
	t.Helper()
	events := &toolLoopEvents{}
	handler := EventHandler(func(eventType string, data json.RawMessage) {
		events.push(eventType, data)
	})
	transport := NewFakeChatTransport()
	mcp := NewFakeMCPClient(mcpTools...)

	session := &Session{
		ID:       "tool-loop-test",
		Messages: []Message{},
	}

	provider := &BaseChatProvider{
		session:    session,
		handler:    handler,
		transport:  transport,
		mcpManager: mcp,
	}
	// Mark started so SendMessage doesn't reject the call.
	provider.started.Store(true)
	if err := mcp.Start(context.Background()); err != nil {
		t.Fatalf("mcp start: %v", err)
	}

	return &toolLoopHarness{provider: provider, transport: transport, mcp: mcp, events: events}
}

// runOneMessage triggers SendMessage and waits for the message_complete event.
func (h *toolLoopHarness) runOneMessage(t *testing.T, text string) {
	t.Helper()
	if err := h.provider.SendMessage(text, nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		for _, ty := range h.events.types() {
			if ty == HandlerMessageComplete {
				return true
			}
		}
		return false
	})
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

func TestToolLoop_NoTools_OneTurnTextOnly(t *testing.T) {
	h := newToolLoopHarness(t) // no MCP tools

	h.transport.QueueTextTurn("hello world")

	h.runOneMessage(t, "say hi")

	if h.transport.CallCount() != 1 {
		t.Errorf("StreamChunks calls: got %d, want 1", h.transport.CallCount())
	}
	if calls := h.mcp.Calls(); len(calls) != 0 {
		t.Errorf("MCP calls: got %d, want 0", len(calls))
	}
}

func TestToolLoop_ToolUse_DispatchesToMCP_AndContinues(t *testing.T) {
	addCalled := atomic.Int32{}
	tool := FakeTool{
		Name:        "add",
		Description: "Add two numbers",
		Handler: func(args json.RawMessage) (string, error) {
			addCalled.Add(1)
			var p struct {
				A, B int
			}
			_ = json.Unmarshal(args, &p)
			return strconv.Itoa(p.A + p.B), nil
		},
	}
	h := newToolLoopHarness(t, tool)

	// Turn 1: model calls the tool.
	h.transport.QueueToolCallTurn("call-1", "add", `{"a":17,"b":25}`)
	// Turn 2: model emits a final text turn — no more tool calls, loop ends.
	h.transport.QueueTextTurn("answer is 42")

	h.runOneMessage(t, "compute 17+25")

	if addCalled.Load() != 1 {
		t.Errorf("add handler invocations: got %d, want 1", addCalled.Load())
	}
	if calls := h.mcp.Calls(); len(calls) != 1 || calls[0].Name != "add" {
		t.Errorf("MCP calls: %+v", calls)
	}
	if !h.events.hasToolResult("call-1") {
		t.Errorf("no tool_result event for call-1; events=%v", h.events.types())
	}
	if h.transport.CallCount() != 2 {
		t.Errorf("StreamChunks calls: got %d, want 2 (tool turn + text turn)", h.transport.CallCount())
	}
}

func TestToolLoop_BuiltinTool_DispatchedBeforeMCP(t *testing.T) {
	// Built-in tools are checked first in runToolLoop. If a name collides
	// with both a built-in and an MCP tool, the built-in wins. We assert that
	// by registering both for the same name and verifying the built-in
	// handler is the one invoked.
	builtinCalled := atomic.Int32{}
	registry := NewBuiltinToolRegistry()
	registry.Register(BuiltinToolDef{Name: "generate_image", Description: "test"},
		func(_ context.Context, _ json.RawMessage, _ []FileAttachment, _ string, _ *EventEmitter) (string, error) {
			builtinCalled.Add(1)
			return "fake-image-url", nil
		})

	mcpCalled := atomic.Int32{}
	mcpTool := FakeTool{
		Name: "generate_image", // intentional collision
		Handler: func(_ json.RawMessage) (string, error) {
			mcpCalled.Add(1)
			return "should-not-run", nil
		},
	}
	h := newToolLoopHarness(t, mcpTool)
	h.provider.builtinTools = registry

	h.transport.QueueToolCallTurn("call-img", "generate_image", `{"prompt":"a cat"}`)
	h.transport.QueueTextTurn("here's your image: fake-image-url")

	h.runOneMessage(t, "make an image")

	if builtinCalled.Load() != 1 {
		t.Errorf("built-in handler invocations: got %d, want 1", builtinCalled.Load())
	}
	if mcpCalled.Load() != 0 {
		t.Errorf("MCP handler invocations: got %d, want 0 (built-in should win)", mcpCalled.Load())
	}
}

func TestToolLoop_IterationCap_StopsAfter10Tools(t *testing.T) {
	// runToolLoop's safety bound: at most 10 tool-execution iterations per
	// user turn. Going around the loop an 11th time should terminate the
	// turn via the iteration==maxIterations branch instead of executing
	// another tool call.
	const maxIters = 10
	callCount := atomic.Int32{}
	tool := FakeTool{
		Name: "loop_forever",
		Handler: func(_ json.RawMessage) (string, error) {
			callCount.Add(1)
			return "result", nil
		},
	}
	h := newToolLoopHarness(t, tool)

	// Queue more turns than the cap; loop should bail out before consuming them.
	for i := 0; i < maxIters+5; i++ {
		h.transport.QueueToolCallTurn("call-"+strconv.Itoa(i), "loop_forever", `{}`)
	}

	h.runOneMessage(t, "spam tools")

	if got := int(callCount.Load()); got != maxIters {
		t.Errorf("tool executions: got %d, want %d (the iteration cap)", got, maxIters)
	}
}

func TestToolLoop_MCPErrorPropagatesAsErrorResult(t *testing.T) {
	tool := FakeTool{
		Name: "broken",
		Handler: func(_ json.RawMessage) (string, error) {
			return "", errors.New("boom")
		},
	}
	h := newToolLoopHarness(t, tool)

	h.transport.QueueToolCallTurn("call-x", "broken", `{}`)
	h.transport.QueueTextTurn("recovered")

	h.runOneMessage(t, "try the broken tool")

	// The event for the failed tool should still be a result/tool_result,
	// with is_error set (asserted by walking the captured events).
	found := false
	h.events.mu.Lock()
	for _, r := range h.events.rows {
		if r.Type != HandlerLLMEvent {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
			Content   string `json:"content"`
		}
		if json.Unmarshal(r.Data, &ev) != nil {
			continue
		}
		if ev.Type == "result" && ev.Subtype == "tool_result" && ev.ToolUseID == "call-x" {
			if !ev.IsError {
				t.Errorf("expected is_error=true for failed tool, got %+v", ev)
			}
			if !strings.Contains(ev.Content, "boom") {
				t.Errorf("error content should mention the wrapped error, got %q", ev.Content)
			}
			found = true
		}
	}
	h.events.mu.Unlock()
	if !found {
		t.Errorf("no tool_result event found for failed tool; events=%v", h.events.types())
	}
}

