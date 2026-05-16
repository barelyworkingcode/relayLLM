package main

import (
	"encoding/json"
	"testing"
)

func TestExtractUsageFromAgentEnd(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "agent_end",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}]},
			{
				"role": "assistant",
				"usage": {"input": 5834, "output": 16, "cacheRead": 0, "cacheWrite": 0, "cost": {"total": 0.0042}}
			}
		]
	}`)
	s := extractUsageFromAgentEnd(raw)
	if s.InputTokens != 5834 {
		t.Errorf("InputTokens: got %d want 5834", s.InputTokens)
	}
	if s.OutputTokens != 16 {
		t.Errorf("OutputTokens: got %d want 16", s.OutputTokens)
	}
	if s.CostUsd != 0.0042 {
		t.Errorf("CostUsd: got %v want 0.0042", s.CostUsd)
	}
}

func TestExtractUsageFromAgentEnd_MultiTurn(t *testing.T) {
	// Multi-turn run (one tool call + one final answer) — usage should sum.
	raw := json.RawMessage(`{
		"messages": [
			{"role": "user"},
			{"role": "assistant", "usage": {"input": 100, "output": 50, "cost": {"total": 0.001}}},
			{"role": "toolResult"},
			{"role": "assistant", "usage": {"input": 200, "output": 25, "cost": {"total": 0.002}}}
		]
	}`)
	s := extractUsageFromAgentEnd(raw)
	if s.InputTokens != 300 || s.OutputTokens != 75 {
		t.Errorf("sum failed: %+v", s)
	}
	if s.CostUsd < 0.003 || s.CostUsd > 0.0031 {
		t.Errorf("CostUsd: got %v want ~0.003", s.CostUsd)
	}
}

// TestBlockInterleaving verifies that pi's out-of-order block events
// (text_start before thinking_end) are translated into a well-formed
// canonical assistant event stream and that allBlocks accumulates the
// completed content blocks in the right order.
func TestBlockInterleaving(t *testing.T) {
	var events []map[string]any
	handler := func(_ string, data json.RawMessage) {
		var ev map[string]any
		_ = json.Unmarshal(data, &ev)
		events = append(events, ev)
	}
	p := &PiProvider{
		handler:      handler,
		piIdxToRelay: make(map[int]int),
	}
	p.emitter = NewEventEmitter(handler)

	// thinking_start (idx 0) → start a thinking block
	p.translateMessageUpdate(json.RawMessage(`{"assistantMessageEvent":{"type":"thinking_start","contentIndex":0}}`))
	p.translateMessageUpdate(json.RawMessage(`{"assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"thinking..."}}`))
	// text_start (idx 1) → must auto-close the thinking block first
	p.translateMessageUpdate(json.RawMessage(`{"assistantMessageEvent":{"type":"text_start","contentIndex":1}}`))
	p.translateMessageUpdate(json.RawMessage(`{"assistantMessageEvent":{"type":"text_delta","contentIndex":1,"delta":"hello"}}`))
	// thinking_end (idx 0) → no-op, block 0 already closed by auto-close
	p.translateMessageUpdate(json.RawMessage(`{"assistantMessageEvent":{"type":"thinking_end","contentIndex":0}}`))
	p.translateMessageUpdate(json.RawMessage(`{"assistantMessageEvent":{"type":"text_end","contentIndex":1}}`))

	// Every event must have type "assistant" — that's the canonical relay
	// wire format defined in events.go.
	for i, ev := range events {
		if ev["type"] != "assistant" {
			t.Errorf("event[%d].type: got %v want \"assistant\"", i, ev["type"])
		}
	}

	// Expected logical sequence:
	//   {index:0, content_block:{type:thinking}}         ThinkingBlockStart
	//   {index:0, delta:{type:thinking_delta}}           ThinkingDelta
	//   {index:0, content_block_stop:true}               auto-close on text_start
	//   {index:1, content_block:{type:text}}             TextBlockStart
	//   {index:1, delta:{type:text_delta}}               TextDelta
	//   {index:1, content_block_stop:true}               text_end
	if len(events) != 6 {
		t.Fatalf("want 6 events, got %d: %v", len(events), events)
	}
	if events[2]["content_block_stop"] != true || events[2]["index"].(float64) != 0 {
		t.Errorf("event[2] should be auto-close of block 0; got %v", events[2])
	}
	if events[5]["content_block_stop"] != true || events[5]["index"].(float64) != 1 {
		t.Errorf("event[5] should be close of block 1; got %v", events[5])
	}

	// Both blocks must end up in allBlocks for session.Messages persistence.
	if len(p.allBlocks) != 2 {
		t.Fatalf("want 2 accumulated blocks, got %d: %v", len(p.allBlocks), p.allBlocks)
	}
	if p.allBlocks[0]["type"] != "thinking" || p.allBlocks[0]["thinking"] != "thinking..." {
		t.Errorf("allBlocks[0] wrong: %v", p.allBlocks[0])
	}
	if p.allBlocks[1]["type"] != "text" || p.allBlocks[1]["text"] != "hello" {
		t.Errorf("allBlocks[1] wrong: %v", p.allBlocks[1])
	}
}
