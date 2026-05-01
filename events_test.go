package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// captureEmitter records every emit call so tests can assert on the canonical
// event sequence produced by turnStreamState.
type captureEmitter struct {
	events []map[string]any
}

func (c *captureEmitter) handler(eventType string, data json.RawMessage) {
	if eventType != "llm_event" {
		return
	}
	var ev map[string]any
	_ = json.Unmarshal(data, &ev)
	c.events = append(c.events, ev)
}

func newCapture() (*captureEmitter, *EventEmitter) {
	c := &captureEmitter{}
	return c, NewEventEmitter(c.handler)
}

// TestStreamState_TextOnlyEmitsStartDeltaStop verifies the simplest path:
// text deltas → content_block_start, content_block_delta, content_block_stop.
func TestStreamState_TextOnlyEmitsStartDeltaStop(t *testing.T) {
	cap, ee := newCapture()
	s := newTurnStreamState(ee)

	s.onText("Hello")
	s.onText(", world")
	s.finalize()

	if len(cap.events) != 4 {
		t.Fatalf("events = %d, want 4 (start, delta, delta, stop):\n%s", len(cap.events), dumpEvents(cap.events))
	}
	assertContentBlockStart(t, cap.events[0], 0, BlockText)
	assertTextDelta(t, cap.events[1], 0, "Hello")
	assertTextDelta(t, cap.events[2], 0, ", world")
	assertBlockStop(t, cap.events[3], 0)
	if got := s.fullText.String(); got != "Hello, world" {
		t.Errorf("fullText = %q", got)
	}
}

// TestStreamState_ThinkingThenTextClosesAndOpensBlocks verifies that switching
// kind closes the open block and opens a new one with a fresh index.
func TestStreamState_ThinkingThenTextClosesAndOpensBlocks(t *testing.T) {
	cap, ee := newCapture()
	s := newTurnStreamState(ee)

	s.onThinking("hmm")
	s.onText("ok")
	s.finalize()

	// Expected: thinking_start(0), thinking_delta(0), block_stop(0),
	//           text_start(1),     text_delta(1),    block_stop(1).
	if len(cap.events) != 6 {
		t.Fatalf("events = %d, want 6:\n%s", len(cap.events), dumpEvents(cap.events))
	}
	assertContentBlockStart(t, cap.events[0], 0, BlockThinking)
	assertThinkingDelta(t, cap.events[1], 0, "hmm")
	assertBlockStop(t, cap.events[2], 0)
	assertContentBlockStart(t, cap.events[3], 1, BlockText)
	assertTextDelta(t, cap.events[4], 1, "ok")
	assertBlockStop(t, cap.events[5], 1)
}

// TestStreamState_ToolCallStartArgsStop verifies that a tool call goes through
// content_block_start (with id+name), input_json_delta(s), and content_block_stop
// (with the resolved input echoed back).
func TestStreamState_ToolCallStartArgsStop(t *testing.T) {
	cap, ee := newCapture()
	s := newTurnStreamState(ee)

	s.onToolStart(&ToolStartEvent{Index: 0, ID: "call_abc", Name: "search"})
	s.onToolArgs(&ToolArgsEvent{Index: 0, Partial: `{"q":"`})
	s.onToolArgs(&ToolArgsEvent{Index: 0, Partial: `foo"}`})
	tools := s.finalize()

	if len(cap.events) != 4 {
		t.Fatalf("events = %d, want 4:\n%s", len(cap.events), dumpEvents(cap.events))
	}
	// Start event with id, name, empty input.
	start := cap.events[0]
	cb := start["content_block"].(map[string]any)
	if cb["type"] != BlockToolUse || cb["id"] != "call_abc" || cb["name"] != "search" {
		t.Errorf("tool start = %+v", cb)
	}
	// Two input_json_delta events.
	assertInputJsonDelta(t, cap.events[1], 0, `{"q":"`)
	assertInputJsonDelta(t, cap.events[2], 0, `foo"}`)
	// Stop event echoes the resolved final content_block.
	stop := cap.events[3]
	if stop["content_block_stop"] != true {
		t.Errorf("stop missing content_block_stop=true: %+v", stop)
	}
	stopCB := stop["content_block"].(map[string]any)
	if stopCB["id"] != "call_abc" || stopCB["name"] != "search" {
		t.Errorf("stop content_block = %+v", stopCB)
	}
	// Resolved input should be the concatenated JSON.
	inputRaw := stopCB["input"]
	inputBytes, _ := json.Marshal(inputRaw)
	var decoded map[string]string
	// content_block.input arrived as json.RawMessage which marshals back to
	// its bytes; double-decode to verify.
	var rawString string
	_ = json.Unmarshal(inputBytes, &rawString)
	src := rawString
	if src == "" {
		src = string(inputBytes)
	}
	if err := json.Unmarshal([]byte(src), &decoded); err != nil {
		t.Fatalf("input not valid JSON: %v (raw=%s)", err, src)
	}
	if decoded["q"] != "foo" {
		t.Errorf("input.q = %q", decoded["q"])
	}

	// finalize returns the resolved tool calls.
	if len(tools) != 1 {
		t.Fatalf("finalize returned %d tools, want 1", len(tools))
	}
	if tools[0].ID != "call_abc" || tools[0].Name != "search" {
		t.Errorf("tool = %+v", tools[0])
	}
}

// TestStreamState_ToolWithoutIDSynthesizes verifies that providers without
// native ids (Ollama) get a synthesized id of the form tool_<index>_<name>.
func TestStreamState_ToolWithoutIDSynthesizes(t *testing.T) {
	cap, ee := newCapture()
	s := newTurnStreamState(ee)

	s.onToolStart(&ToolStartEvent{Index: 0, Name: "fetch"})
	s.onToolArgs(&ToolArgsEvent{Index: 0, Partial: `{"url":"x"}`})
	tools := s.finalize()

	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	want := SynthesizeToolUseID(0, "fetch")
	if tools[0].ID != want {
		t.Errorf("synthesized ID = %q, want %q", tools[0].ID, want)
	}
	// The same id appears in the start event.
	cb := cap.events[0]["content_block"].(map[string]any)
	if cb["id"] != want {
		t.Errorf("start id = %v, want %q", cb["id"], want)
	}
}

// TestStreamState_TextThenToolThenText emits two text blocks separated by a
// tool block with ascending indices.
func TestStreamState_TextThenToolThenText(t *testing.T) {
	cap, ee := newCapture()
	s := newTurnStreamState(ee)

	s.onText("before ")
	s.onToolStart(&ToolStartEvent{Index: 0, ID: "id1", Name: "fn"})
	s.onToolArgs(&ToolArgsEvent{Index: 0, Partial: `{}`})
	s.onText("after")
	s.finalize()

	indices := []int{}
	for _, ev := range cap.events {
		if cb, ok := ev["content_block"].(map[string]any); ok && cb != nil {
			if idx, ok := ev["index"].(float64); ok {
				indices = append(indices, int(idx))
				_ = cb
			}
		}
	}
	// Block start indices: 0 (text), 1 (tool_use), 2 (text). The tool stop also
	// echoes content_block, so we'll see indices 0, 1, 1, 2 in that order.
	wantIndices := []int{0, 1, 1, 2}
	if len(indices) != len(wantIndices) {
		t.Fatalf("indices = %v, want %v\n%s", indices, wantIndices, dumpEvents(cap.events))
	}
	for i, want := range wantIndices {
		if indices[i] != want {
			t.Errorf("index[%d] = %d, want %d", i, indices[i], want)
		}
	}
}

// TestStreamState_AccumulatesCanonicalBlocks verifies that the state machine
// builds up s.blocks in canonical Anthropic content_block shape so callers can
// persist a refresh-stable assistant message that includes thinking.
func TestStreamState_AccumulatesCanonicalBlocks(t *testing.T) {
	_, ee := newCapture()
	s := newTurnStreamState(ee)

	s.onThinking("hmm let me think")
	s.onText("here is the answer")
	s.onToolStart(&ToolStartEvent{Index: 0, ID: "call_X", Name: "search"})
	s.onToolArgs(&ToolArgsEvent{Index: 0, Partial: `{"q":"foo"}`})
	s.finalize()

	if len(s.blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 (thinking + text + tool_use):\n%s", len(s.blocks), dumpBlocks(s.blocks))
	}
	expectBlock(t, s.blocks[0], "thinking", "hmm let me think")
	expectBlock(t, s.blocks[1], "text", "here is the answer")
	var toolBlock map[string]any
	_ = json.Unmarshal(s.blocks[2], &toolBlock)
	if toolBlock["type"] != "tool_use" || toolBlock["name"] != "search" {
		t.Errorf("tool block = %+v, want tool_use named search", toolBlock)
	}
}

// expectBlock asserts a canonical content block has the given type and the
// matching content field (text→"text", thinking→"thinking"). Tool blocks
// don't go through here — see TestStreamState_ToolCallStartArgsStop.
func expectBlock(t *testing.T, raw json.RawMessage, wantType, wantContent string) {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b["type"] != wantType {
		t.Errorf("type = %v, want %s", b["type"], wantType)
	}
	// Anthropic's content-field name matches the block type for text/thinking.
	if b[wantType] != wantContent {
		t.Errorf("%s = %v, want %q", wantType, b[wantType], wantContent)
	}
}

func dumpBlocks(blocks []json.RawMessage) string {
	var sb strings.Builder
	for i, b := range blocks {
		sb.WriteString("[")
		sb.WriteString(string(b))
		sb.WriteString("]")
		if i < len(blocks)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// TestStreamState_DuplicateToolStartIgnored verifies the defensive guard
// against transports re-emitting ToolStart for an existing index.
func TestStreamState_DuplicateToolStartIgnored(t *testing.T) {
	cap, ee := newCapture()
	s := newTurnStreamState(ee)

	s.onToolStart(&ToolStartEvent{Index: 0, ID: "id1", Name: "fn"})
	s.onToolStart(&ToolStartEvent{Index: 0, ID: "DUPLICATE", Name: "other"})
	s.onToolArgs(&ToolArgsEvent{Index: 0, Partial: `{}`})
	tools := s.finalize()

	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	if tools[0].ID != "id1" {
		t.Errorf("ID = %q, want id1 (duplicate should be ignored)", tools[0].ID)
	}
	// Only one start event should have been emitted, plus deltas + stop.
	starts := 0
	for _, ev := range cap.events {
		if cb, ok := ev["content_block"].(map[string]any); ok && cb["type"] == BlockToolUse {
			if _, isStop := ev["content_block_stop"]; !isStop {
				starts++
			}
		}
	}
	if starts != 1 {
		t.Errorf("tool starts emitted = %d, want 1\n%s", starts, dumpEvents(cap.events))
	}
}

// --- helpers ---

func assertContentBlockStart(t *testing.T, ev map[string]any, wantIdx int, wantType string) {
	t.Helper()
	if ev["type"] != "assistant" {
		t.Errorf("type = %v, want assistant", ev["type"])
	}
	if int(ev["index"].(float64)) != wantIdx {
		t.Errorf("index = %v, want %d", ev["index"], wantIdx)
	}
	if _, hasStop := ev["content_block_stop"]; hasStop {
		t.Errorf("expected start, got stop: %+v", ev)
	}
	cb, ok := ev["content_block"].(map[string]any)
	if !ok {
		t.Fatalf("content_block missing on %+v", ev)
	}
	if cb["type"] != wantType {
		t.Errorf("content_block.type = %v, want %s", cb["type"], wantType)
	}
}

func assertTextDelta(t *testing.T, ev map[string]any, wantIdx int, wantText string) {
	t.Helper()
	if int(ev["index"].(float64)) != wantIdx {
		t.Errorf("index = %v, want %d", ev["index"], wantIdx)
	}
	d, ok := ev["delta"].(map[string]any)
	if !ok {
		t.Fatalf("delta missing on %+v", ev)
	}
	if d["type"] != "text_delta" {
		t.Errorf("delta.type = %v, want text_delta", d["type"])
	}
	if d["text"] != wantText {
		t.Errorf("delta.text = %v, want %q", d["text"], wantText)
	}
}

func assertThinkingDelta(t *testing.T, ev map[string]any, wantIdx int, wantText string) {
	t.Helper()
	if int(ev["index"].(float64)) != wantIdx {
		t.Errorf("index = %v, want %d", ev["index"], wantIdx)
	}
	d := ev["delta"].(map[string]any)
	if d["type"] != "thinking_delta" {
		t.Errorf("delta.type = %v, want thinking_delta", d["type"])
	}
	if d["thinking"] != wantText {
		t.Errorf("delta.thinking = %v, want %q", d["thinking"], wantText)
	}
}

func assertInputJsonDelta(t *testing.T, ev map[string]any, wantIdx int, wantPartial string) {
	t.Helper()
	if int(ev["index"].(float64)) != wantIdx {
		t.Errorf("index = %v, want %d", ev["index"], wantIdx)
	}
	d := ev["delta"].(map[string]any)
	if d["type"] != "input_json_delta" {
		t.Errorf("delta.type = %v, want input_json_delta", d["type"])
	}
	if d["partial_json"] != wantPartial {
		t.Errorf("delta.partial_json = %v, want %q", d["partial_json"], wantPartial)
	}
}

func assertBlockStop(t *testing.T, ev map[string]any, wantIdx int) {
	t.Helper()
	if int(ev["index"].(float64)) != wantIdx {
		t.Errorf("index = %v, want %d", ev["index"], wantIdx)
	}
	if ev["content_block_stop"] != true {
		t.Errorf("content_block_stop = %v, want true", ev["content_block_stop"])
	}
}

func dumpEvents(events []map[string]any) string {
	var sb strings.Builder
	for i, e := range events {
		b, _ := json.MarshalIndent(e, "", "  ")
		sb.WriteString("[")
		sb.WriteString(strings.TrimSuffix(string(b), "\n"))
		sb.WriteString("]")
		if i < len(events)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
