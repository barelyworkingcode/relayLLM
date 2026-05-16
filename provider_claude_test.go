package main

import (
	"encoding/json"
	"testing"
)

// claudeTestProvider builds a ClaudeProvider wired to a capture handler.
// Events are recorded as parsed maps so tests can assert on individual fields.
func claudeTestProvider() (*ClaudeProvider, *[]capturedEvent) {
	captured := &[]capturedEvent{}
	handler := func(eventType string, data json.RawMessage) {
		var payload map[string]any
		if len(data) > 0 {
			_ = json.Unmarshal(data, &payload)
		}
		*captured = append(*captured, capturedEvent{
			eventType: eventType,
			payload:   payload,
			raw:       data,
		})
	}
	p := &ClaudeProvider{
		handler: handler,
		emitter: NewEventEmitter(handler),
	}
	return p, captured
}

type capturedEvent struct {
	eventType string
	payload   map[string]any
	raw       json.RawMessage
}

func (c capturedEvent) llmEventType() string {
	t, _ := c.payload["type"].(string)
	return t
}

func (c capturedEvent) llmEventSubtype() string {
	s, _ := c.payload["subtype"].(string)
	return s
}

// All emitted llm_events must carry v: 2.
func assertVersion(t *testing.T, events []capturedEvent) {
	t.Helper()
	for i, ev := range events {
		if ev.eventType != "llm_event" {
			continue
		}
		v, ok := ev.payload["v"]
		if !ok {
			t.Errorf("event #%d (%s/%s) missing v field: %s", i, ev.llmEventType(), ev.llmEventSubtype(), ev.raw)
			continue
		}
		// JSON numbers decode as float64.
		if vf, ok := v.(float64); !ok || vf != float64(ProtocolVersionNum) {
			t.Errorf("event #%d (%s/%s) has v=%v, want %d", i, ev.llmEventType(), ev.llmEventSubtype(), v, ProtocolVersionNum)
		}
	}
}

func TestClaudeTranslate_SystemInit(t *testing.T) {
	p, captured := claudeTestProvider()
	raw := json.RawMessage(`{
		"type": "system",
		"subtype": "init",
		"session_id": "sid_abc",
		"model": "claude-opus-4-7",
		"cwd": "/tmp",
		"tools": ["Read", "Bash"],
		"mcp_servers": ["relay"]
	}`)
	p.processLine(raw)

	// Session id must be captured for --resume.
	if p.claudeSessionID != "sid_abc" {
		t.Errorf("claudeSessionID = %q, want sid_abc", p.claudeSessionID)
	}

	if len(*captured) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(*captured), *captured)
	}
	ev := (*captured)[0]
	if ev.llmEventType() != "system" || ev.llmEventSubtype() != "init" {
		t.Errorf("event = %s/%s, want system/init", ev.llmEventType(), ev.llmEventSubtype())
	}
	if ev.payload["model"] != "claude-opus-4-7" {
		t.Errorf("model = %v", ev.payload["model"])
	}
	assertVersion(t, *captured)
}

func TestClaudeTranslate_MessageStart(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`{
		"type": "assistant",
		"message": {"id": "msg_abc", "role": "assistant", "content": []}
	}`))

	if len(*captured) != 1 {
		t.Fatalf("got %d events, want 1", len(*captured))
	}
	ev := (*captured)[0]
	if ev.llmEventType() != "assistant" {
		t.Errorf("event type = %s", ev.llmEventType())
	}
	msg, _ := ev.payload["message"].(map[string]any)
	if msg["id"] != "msg_abc" {
		t.Errorf("message.id = %v", msg["id"])
	}
	assertVersion(t, *captured)
}

// Legacy variant: content_block_start carries inline `.text`. Must normalize
// to bare BlockStart + TextDelta.
func TestClaudeTranslate_CombinedTextStartContent(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`{
		"type": "assistant",
		"index": 0,
		"content_block": {"type": "text", "text": "Hello, "}
	}`))

	if len(*captured) != 2 {
		t.Fatalf("got %d events, want 2 (start + delta), payloads: %+v", len(*captured), *captured)
	}

	// First event: bare content_block_start (no inline text).
	start := (*captured)[0]
	if start.llmEventType() != "assistant" {
		t.Errorf("event[0] type = %s", start.llmEventType())
	}
	cb, _ := start.payload["content_block"].(map[string]any)
	if cb["type"] != "text" {
		t.Errorf("content_block.type = %v", cb["type"])
	}
	if _, hasText := cb["text"]; hasText {
		t.Errorf("content_block.text should not be present on start; got %v", cb["text"])
	}

	// Second event: text_delta with the inline content.
	delta := (*captured)[1]
	d, _ := delta.payload["delta"].(map[string]any)
	if d["type"] != "text_delta" {
		t.Errorf("delta.type = %v, want text_delta", d["type"])
	}
	if d["text"] != "Hello, " {
		t.Errorf("delta.text = %v, want \"Hello, \"", d["text"])
	}
	assertVersion(t, *captured)
}

// Legacy variant: thinking block carrying inline `.thinking`. Same shape as
// the text variant, different block type.
func TestClaudeTranslate_CombinedThinkingStartContent(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`{
		"type": "assistant",
		"index": 1,
		"content_block": {"type": "thinking", "thinking": "let me think"}
	}`))

	if len(*captured) != 2 {
		t.Fatalf("got %d events, want 2", len(*captured))
	}
	d, _ := (*captured)[1].payload["delta"].(map[string]any)
	if d["type"] != "thinking_delta" || d["thinking"] != "let me think" {
		t.Errorf("thinking delta = %+v", d)
	}
	assertVersion(t, *captured)
}

func TestClaudeTranslate_TextStreaming(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`{"type":"assistant","index":0,"content_block":{"type":"text"}}`))
	p.processLine(json.RawMessage(`{"type":"assistant","index":0,"delta":{"type":"text_delta","text":"Hello"}}`))
	p.processLine(json.RawMessage(`{"type":"assistant","index":0,"delta":{"type":"text_delta","text":" world"}}`))
	p.processLine(json.RawMessage(`{"type":"assistant","index":0,"content_block_stop":true}`))

	if len(*captured) != 4 {
		t.Fatalf("got %d events, want 4", len(*captured))
	}
	// Last event must be content_block_stop with no resolved content_block.
	stop := (*captured)[3]
	if stop.payload["content_block_stop"] != true {
		t.Errorf("expected content_block_stop=true; got %v", stop.payload)
	}
	if _, has := stop.payload["content_block"]; has {
		t.Errorf("text block stop should not echo content_block; got %v", stop.payload["content_block"])
	}
	assertVersion(t, *captured)
}

// Block_stop must echo the resolved input so clients that skipped the
// streaming deltas still see the full call.
func TestClaudeTranslate_ToolUseStreaming(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`{
		"type": "assistant",
		"index": 0,
		"content_block": {"type": "tool_use", "id": "toolu_X", "name": "Read"}
	}`))
	p.processLine(json.RawMessage(`{
		"type": "assistant",
		"index": 0,
		"delta": {"type": "input_json_delta", "partial_json": "{\"path\":"}
	}`))
	p.processLine(json.RawMessage(`{
		"type": "assistant",
		"index": 0,
		"delta": {"type": "input_json_delta", "partial_json": "\"/etc/hosts\"}"}
	}`))
	p.processLine(json.RawMessage(`{
		"type": "assistant",
		"index": 0,
		"content_block_stop": true,
		"content_block": {"type": "tool_use", "id": "toolu_X", "name": "Read", "input": {"path": "/etc/hosts"}}
	}`))

	if len(*captured) != 4 {
		t.Fatalf("got %d events, want 4", len(*captured))
	}

	// Block start carries id/name.
	start := (*captured)[0]
	cb, _ := start.payload["content_block"].(map[string]any)
	if cb["id"] != "toolu_X" || cb["name"] != "Read" {
		t.Errorf("start content_block = %+v", cb)
	}

	// Block stop echoes resolved input.
	stop := (*captured)[3]
	if stop.payload["content_block_stop"] != true {
		t.Errorf("expected content_block_stop=true")
	}
	stopCB, _ := stop.payload["content_block"].(map[string]any)
	if stopCB["id"] != "toolu_X" {
		t.Errorf("stop content_block.id = %v", stopCB["id"])
	}
	resolvedInput, _ := stopCB["input"].(map[string]any)
	if resolvedInput["path"] != "/etc/hosts" {
		t.Errorf("resolved input = %+v", resolvedInput)
	}
	assertVersion(t, *captured)
}

// Claude CLI wraps tool results inside `user` events. Must surface as
// canonical result.tool_result.
func TestClaudeTranslate_ToolResultFromUserEvent(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`{
		"type": "user",
		"message": {
			"role": "user",
			"content": [{
				"type": "tool_result",
				"tool_use_id": "toolu_X",
				"tool_name": "Read",
				"content": "file contents here",
				"is_error": false
			}]
		}
	}`))

	if len(*captured) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(*captured), *captured)
	}
	ev := (*captured)[0]
	if ev.llmEventType() != "result" || ev.llmEventSubtype() != "tool_result" {
		t.Errorf("event = %s/%s", ev.llmEventType(), ev.llmEventSubtype())
	}
	if ev.payload["tool_use_id"] != "toolu_X" {
		t.Errorf("tool_use_id = %v", ev.payload["tool_use_id"])
	}
	if ev.payload["content"] != "file contents here" {
		t.Errorf("content = %v", ev.payload["content"])
	}
	assertVersion(t, *captured)
}

// Polymorphic tool_result content: array of {type,text} blocks. Flattened to
// one string for the canonical wire shape.
func TestClaudeTranslate_ToolResultContentArray(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`{
		"type": "user",
		"message": {
			"role": "user",
			"content": [{
				"type": "tool_result",
				"tool_use_id": "toolu_Y",
				"content": [{"type": "text", "text": "part 1 "}, {"type": "text", "text": "part 2"}]
			}]
		}
	}`))

	if len(*captured) != 1 {
		t.Fatalf("got %d events", len(*captured))
	}
	if (*captured)[0].payload["content"] != "part 1 part 2" {
		t.Errorf("flattened content = %v", (*captured)[0].payload["content"])
	}
}

// `result` ends the turn: stats_update + message_complete (nil data so the
// session layer's fallback-save branch is a no-op). The result's content
// blocks already arrived via the streamed path and are not re-emitted.
func TestClaudeTranslate_ResultEventEmitsStatsAndComplete(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`{
		"type": "result",
		"usage": {
			"input_tokens": 100,
			"output_tokens": 50,
			"cache_read_input_tokens": 0,
			"cache_creation_input_tokens": 0
		},
		"total_cost_usd": 0.001
	}`))

	if len(*captured) != 2 {
		t.Fatalf("got %d events, want 2 (stats_update + message_complete): %+v", len(*captured), *captured)
	}
	if (*captured)[0].eventType != "stats_update" {
		t.Errorf("event[0] = %s, want stats_update", (*captured)[0].eventType)
	}
	if (*captured)[1].eventType != "message_complete" {
		t.Errorf("event[1] = %s, want message_complete", (*captured)[1].eventType)
	}
	// message_complete data must be nil to suppress the session layer's
	// fallback-save branch.
	if (*captured)[1].raw != nil && len((*captured)[1].raw) > 0 {
		t.Errorf("message_complete data should be nil, got %s", (*captured)[1].raw)
	}
}

// Forward-compat: unknown top-level types (permission-mode, ai-title, etc.)
// pass through with v stamped on so clients can choose to render them.
func TestClaudeTranslate_UnknownTopLevel_GetsVersionStamped(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`{"type":"permission-mode","mode":"plan"}`))

	if len(*captured) != 1 {
		t.Fatalf("got %d events", len(*captured))
	}
	ev := (*captured)[0]
	if ev.payload["type"] != "permission-mode" || ev.payload["mode"] != "plan" {
		t.Errorf("payload = %+v", ev.payload)
	}
	assertVersion(t, *captured)
}

// system.api_error is a typed subtype — the translator extracts message and
// retrying into the canonical SystemAPIError event.
func TestClaudeTranslate_SystemAPIError(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`{
		"type": "system",
		"subtype": "api_error",
		"message": "Upstream returned 503",
		"retrying": true
	}`))

	if len(*captured) != 1 {
		t.Fatalf("got %d events", len(*captured))
	}
	ev := (*captured)[0]
	if ev.llmEventType() != "system" || ev.llmEventSubtype() != "api_error" {
		t.Errorf("event = %s/%s", ev.llmEventType(), ev.llmEventSubtype())
	}
	if ev.payload["message"] != "Upstream returned 503" {
		t.Errorf("message = %v", ev.payload["message"])
	}
	if ev.payload["retrying"] != true {
		t.Errorf("retrying = %v", ev.payload["retrying"])
	}
	assertVersion(t, *captured)
}

func TestClaudeTranslate_NonJsonOutput(t *testing.T) {
	p, captured := claudeTestProvider()
	p.processLine(json.RawMessage(`this is not valid json`))

	if len(*captured) != 1 {
		t.Fatalf("got %d events", len(*captured))
	}
	if (*captured)[0].eventType != "raw_output" {
		t.Errorf("eventType = %s, want raw_output", (*captured)[0].eventType)
	}
}

func TestClaudeTranslate_FullTurn(t *testing.T) {
	p, captured := claudeTestProvider()

	p.processLine(json.RawMessage(`{"type":"system","subtype":"init","session_id":"s1","model":"opus","cwd":"/x","tools":[],"mcp_servers":[]}`))
	p.processLine(json.RawMessage(`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[]}}`))
	p.processLine(json.RawMessage(`{"type":"assistant","index":0,"content_block":{"type":"text"}}`))
	p.processLine(json.RawMessage(`{"type":"assistant","index":0,"delta":{"type":"text_delta","text":"Hi"}}`))
	p.processLine(json.RawMessage(`{"type":"assistant","index":0,"content_block_stop":true}`))
	p.processLine(json.RawMessage(`{"type":"result","usage":{"input_tokens":10,"output_tokens":5},"total_cost_usd":0.0001}`))

	// Expected: init, message_start, block_start, delta, block_stop (5 llm_events), then stats_update + message_complete (2 non-llm_event).
	if len(*captured) != 7 {
		t.Fatalf("got %d events: %+v", len(*captured), *captured)
	}

	expected := []struct {
		eventType    string
		llmType      string
		llmSubtype   string
	}{
		{"llm_event", "system", "init"},
		{"llm_event", "assistant", ""},
		{"llm_event", "assistant", ""},
		{"llm_event", "assistant", ""},
		{"llm_event", "assistant", ""},
		{"stats_update", "", ""},
		{"message_complete", "", ""},
	}
	for i, want := range expected {
		got := (*captured)[i]
		if got.eventType != want.eventType {
			t.Errorf("event[%d].eventType = %s, want %s", i, got.eventType, want.eventType)
		}
		if want.eventType == "llm_event" {
			if got.llmEventType() != want.llmType {
				t.Errorf("event[%d].type = %s, want %s", i, got.llmEventType(), want.llmType)
			}
			if want.llmSubtype != "" && got.llmEventSubtype() != want.llmSubtype {
				t.Errorf("event[%d].subtype = %s, want %s", i, got.llmEventSubtype(), want.llmSubtype)
			}
		}
	}
	assertVersion(t, *captured)
}
