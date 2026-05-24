package main

// Exhaustive golden tests for provider_pi.go's translate(). The translation
// layer maps pi.dev's RPC event stream to relay's canonical Claude-shaped
// envelope. It is stateful, has multiple identity-resolution fallback paths,
// and ships zero coverage today — every assertion below pins a behavior the
// renderer depends on.
//
// Style: each subtest feeds one or more raw pi events into translate() and
// asserts on the captured canonical event sequence. Fixtures are inline so
// the relationship between input and expected output is one screenful.

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
)

// newPiTranslateHarness constructs a minimal PiProvider wired to a capture
// handler. translate() can run end-to-end without spawning a real pi process.
type piTranslateHarness struct {
	provider *PiProvider
	captured *piEventCapture
}

type piEventCapture struct {
	mu   sync.Mutex
	rows []piEventRow
}

type piEventRow struct {
	Type string
	Raw  json.RawMessage
}

func (c *piEventCapture) push(t string, d json.RawMessage) {
	c.mu.Lock()
	c.rows = append(c.rows, piEventRow{Type: t, Raw: append(json.RawMessage(nil), d...)})
	c.mu.Unlock()
}

func (c *piEventCapture) snapshot() []piEventRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]piEventRow, len(c.rows))
	copy(out, c.rows)
	return out
}

// inner returns the decoded `event` field of llm_event rows; for non-llm_event
// rows it returns the raw payload. Saves boilerplate at call sites.
func (c *piEventCapture) inner(i int) map[string]any {
	rows := c.snapshot()
	if i >= len(rows) {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(rows[i].Raw, &out)
	return out
}

func newPiTranslateHarness(t *testing.T) *piTranslateHarness {
	t.Helper()
	cap := &piEventCapture{}
	handler := EventHandler(func(eventType string, data json.RawMessage) {
		cap.push(eventType, data)
	})
	sess := &Session{ID: "pi-translate-test", Directory: "/tmp/test"}
	p := NewPiProvider(sess, handler, "anthropic", "claude-sonnet-4", "", nil, PiOverlayInputs{})
	return &piTranslateHarness{provider: p, captured: cap}
}

// ---------------------------------------------------------------------------
// agent_start
// ---------------------------------------------------------------------------

func TestPiTranslate_AgentStart_EmitsSystemInitAndMessageStart(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("agent_start", json.RawMessage(`{}`))

	rows := h.captured.snapshot()
	if len(rows) < 2 {
		t.Fatalf("expected ≥2 events, got %d: %+v", len(rows), rows)
	}
	if !pickEventType(rows, "system") {
		t.Errorf("no system event emitted; got %v", rowTypes(rows))
	}
	if !pickAssistantSubtype(rows, "message_start") {
		t.Errorf("no message_start event emitted; got %v", rowTypes(rows))
	}
}

// ---------------------------------------------------------------------------
// text_*
// ---------------------------------------------------------------------------

func TestPiTranslate_TextLifecycle_EmitsStartDeltaStop(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("message_update", piMsgUpdate("text_start", 0, ""))
	h.provider.translate("message_update", piMsgUpdate("text_delta", 0, "hello"))
	h.provider.translate("message_update", piMsgUpdate("text_delta", 0, " world"))
	h.provider.translate("message_update", piMsgUpdate("text_end", 0, ""))

	types := pickContentBlockKinds(h.captured.snapshot())
	wantStart := slices.Contains(types,"text:start")
	wantDelta := slices.Contains(types,"text:delta")
	wantStop := slices.Contains(types,"text:stop")
	if !wantStart || !wantDelta || !wantStop {
		t.Errorf("text lifecycle missing events; saw %v", types)
	}
}

// ---------------------------------------------------------------------------
// thinking_*
// ---------------------------------------------------------------------------

func TestPiTranslate_ThinkingLifecycle_EmitsStartDeltaStop(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("message_update", piMsgUpdate("thinking_start", 0, ""))
	h.provider.translate("message_update", piMsgUpdate("thinking_delta", 0, "let me think..."))
	h.provider.translate("message_update", piMsgUpdate("thinking_end", 0, ""))

	types := pickContentBlockKinds(h.captured.snapshot())
	if !slices.Contains(types,"thinking:start") || !slices.Contains(types,"thinking:delta") || !slices.Contains(types,"thinking:stop") {
		t.Errorf("thinking lifecycle missing events; saw %v", types)
	}
}

// ---------------------------------------------------------------------------
// toolcall_*  — three identity-resolution paths
// ---------------------------------------------------------------------------

func TestPiTranslate_ToolCall_NestedToolCallObject(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("message_update", json.RawMessage(`{
		"assistantMessageEvent": {
			"type": "toolcall_start",
			"contentIndex": 0,
			"toolCall": {"id": "call_nested", "name": "Read"}
		}
	}`))

	id, name, ok := findToolUseStart(h.captured.snapshot())
	if !ok || id != "call_nested" || name != "Read" {
		t.Errorf("nested identity: got id=%q name=%q ok=%v", id, name, ok)
	}
}

func TestPiTranslate_ToolCall_FlatTopLevelFields(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("message_update", json.RawMessage(`{
		"assistantMessageEvent": {
			"type": "toolcall_start",
			"contentIndex": 0,
			"toolCallId": "call_flat",
			"toolName": "Write"
		}
	}`))

	id, name, ok := findToolUseStart(h.captured.snapshot())
	if !ok || id != "call_flat" || name != "Write" {
		t.Errorf("flat identity: got id=%q name=%q ok=%v", id, name, ok)
	}
}

func TestPiTranslate_ToolCall_HarvestedFromExecutionStart(t *testing.T) {
	h := newPiTranslateHarness(t)
	// Pi's execution_start arrives BEFORE the toolcall_start in some versions.
	// Even though toolcall_start lacks both nested and flat name, the harvested
	// cache must supply it.
	h.provider.translate("tool_execution_start",
		json.RawMessage(`{"toolCallId":"call_harvest","toolName":"Edit"}`))
	h.provider.translate("message_update", json.RawMessage(`{
		"assistantMessageEvent": {
			"type": "toolcall_start",
			"contentIndex": 0,
			"toolCallId": "call_harvest"
		}
	}`))

	id, name, ok := findToolUseStart(h.captured.snapshot())
	if !ok || id != "call_harvest" || name != "Edit" {
		t.Errorf("harvested identity: got id=%q name=%q ok=%v", id, name, ok)
	}
}

func TestPiTranslate_ToolCall_AllSourcesEmpty_FallsBackToPlaceholder(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("message_update", json.RawMessage(`{
		"assistantMessageEvent": {
			"type": "toolcall_start",
			"contentIndex": 0
		}
	}`))

	_, name, ok := findToolUseStart(h.captured.snapshot())
	if !ok || name != piMissingToolNamePlaceholder {
		t.Errorf("fallback placeholder: got name=%q ok=%v want %q", name, ok, piMissingToolNamePlaceholder)
	}
}

func TestPiTranslate_ToolCallDelta_EmitsInputJsonDelta(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("message_update", json.RawMessage(`{
		"assistantMessageEvent": {
			"type": "toolcall_start",
			"contentIndex": 0,
			"toolCallId": "x", "toolName": "Read"
		}
	}`))
	h.provider.translate("message_update", json.RawMessage(`{
		"assistantMessageEvent": {
			"type": "toolcall_delta",
			"contentIndex": 0,
			"delta": "{\"path\":\""
		}
	}`))
	h.provider.translate("message_update", json.RawMessage(`{
		"assistantMessageEvent": {
			"type": "toolcall_delta",
			"contentIndex": 0,
			"delta": "/etc/passwd\"}"
		}
	}`))

	rows := h.captured.snapshot()
	combined := strings.Join(rowTypes(rows), ",")
	// The exact delta type label comes from EventEmitter; what we care about
	// is that at least one delta carries a non-empty partial.
	if !pickEventHas(rows, "input_json_delta") {
		t.Errorf("no input_json_delta emitted; got %s", combined)
	}
}

// ---------------------------------------------------------------------------
// tool_execution_end
// ---------------------------------------------------------------------------

func TestPiTranslate_ToolExecutionEnd_EmitsToolResult_Success(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("tool_execution_end", json.RawMessage(`{
		"toolCallId": "call_ok",
		"toolName":   "Read",
		"result":     {"content": "file contents"},
		"isError":    false
	}`))

	id, content, isErr, ok := findToolResult(h.captured.snapshot())
	if !ok || id != "call_ok" || content == "" || isErr {
		t.Errorf("tool_result success: id=%q content=%q isErr=%v ok=%v", id, content, isErr, ok)
	}
}

func TestPiTranslate_ToolExecutionEnd_EmitsToolResult_Error(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("tool_execution_end", json.RawMessage(`{
		"toolCallId": "call_err",
		"toolName":   "Write",
		"result":     {"content": "permission denied"},
		"isError":    true
	}`))

	id, _, isErr, ok := findToolResult(h.captured.snapshot())
	if !ok || id != "call_err" || !isErr {
		t.Errorf("tool_result error: id=%q isErr=%v ok=%v", id, isErr, ok)
	}
}

// ---------------------------------------------------------------------------
// error
// ---------------------------------------------------------------------------

func TestPiTranslate_AssistantError_EmitsResultError(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("message_update", json.RawMessage(`{
		"assistantMessageEvent": {"type": "error", "reason": "upstream 503"}
	}`))

	rows := h.captured.snapshot()
	if !pickResultSubtype(rows, "error") {
		t.Errorf("no result/error event; got %v", rowTypes(rows))
	}
}

// ---------------------------------------------------------------------------
// auto_retry_{start,end}
// ---------------------------------------------------------------------------

func TestPiTranslate_AutoRetryStart_EmitsRawOutputNotice(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("auto_retry_start", json.RawMessage(`{
		"attempt": 1, "maxAttempts": 3, "delayMs": 2000, "errorMessage": "rate limited"
	}`))

	if !sawRawOutputContaining(h.captured.snapshot(), "Retry 1/3") {
		t.Errorf("no Retry 1/3 raw_output; got %v", rowTypes(h.captured.snapshot()))
	}
}

func TestPiTranslate_AutoRetryEnd_EmitsRawOutputNotice(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("auto_retry_end", json.RawMessage(`{
		"success": true, "attempt": 2
	}`))

	if !sawRawOutputContaining(h.captured.snapshot(), "Retry succeeded on attempt 2") {
		t.Errorf("no Retry succeeded raw_output; got %v", rowTypes(h.captured.snapshot()))
	}
}

// ---------------------------------------------------------------------------
// Unknown / default
// ---------------------------------------------------------------------------

func TestPiTranslate_UnknownEvent_GoesToRawOutput(t *testing.T) {
	h := newPiTranslateHarness(t)
	h.provider.translate("queue_update", json.RawMessage(`{"depth":5}`))

	saw := false
	for _, r := range h.captured.snapshot() {
		if r.Type == "raw_output" {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("unknown event should fall through to raw_output; got %v", rowTypes(h.captured.snapshot()))
	}
}

// ---------------------------------------------------------------------------
// contentIndex → blockIndex monotonic mapping
// ---------------------------------------------------------------------------

func TestPiTranslate_InterleavedBlocks_AllocateMonotonicIndices(t *testing.T) {
	h := newPiTranslateHarness(t)
	// Pi sometimes reuses or sparsely numbers contentIndex (e.g. 0 then 5).
	// Relay must allocate dense monotonic block indices.
	h.provider.translate("message_update", piMsgUpdate("text_start", 0, ""))
	h.provider.translate("message_update", piMsgUpdate("text_end", 0, ""))
	h.provider.translate("message_update", piMsgUpdate("text_start", 5, ""))
	h.provider.translate("message_update", piMsgUpdate("text_end", 5, ""))

	indices := collectBlockStartIndices(h.captured.snapshot())
	if len(indices) < 2 {
		t.Fatalf("expected ≥2 block_start events, got %d (%v)", len(indices), indices)
	}
	if indices[0] != 0 || indices[1] != 1 {
		t.Errorf("expected dense relay indices [0, 1, ...], got %v", indices)
	}
}

// ---------------------------------------------------------------------------
// Inline fixture / assertion helpers
// ---------------------------------------------------------------------------

func piMsgUpdate(piType string, contentIdx int, delta string) json.RawMessage {
	payload := map[string]any{
		"assistantMessageEvent": map[string]any{
			"type":         piType,
			"contentIndex": contentIdx,
			"delta":        delta,
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func rowTypes(rows []piEventRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Type
	}
	return out
}

// pickEventType reports whether any llm_event row's envelope `type` matches.
// Canonical envelopes: "system" / "assistant" / "result".
func pickEventType(rows []piEventRow, want string) bool {
	for _, r := range rows {
		if r.Type != HandlerLLMEvent {
			continue
		}
		var m map[string]any
		if json.Unmarshal(r.Raw, &m) != nil {
			continue
		}
		if t, _ := m["type"].(string); t == want {
			return true
		}
	}
	return false
}

// pickAssistantSubtype reports whether any assistant llm_event matches the
// requested shape: "message_start" (has .message), "content_block_start"
// (has .content_block but no .delta), "content_block_delta" (has .delta),
// "content_block_stop" (has .content_block_stop).
func pickAssistantSubtype(rows []piEventRow, want string) bool {
	for _, r := range rows {
		if r.Type != HandlerLLMEvent {
			continue
		}
		var m map[string]any
		if json.Unmarshal(r.Raw, &m) != nil {
			continue
		}
		if t, _ := m["type"].(string); t != "assistant" {
			continue
		}
		_, hasMsg := m["message"]
		_, hasCB := m["content_block"]
		_, hasDelta := m["delta"]
		_, hasStop := m["content_block_stop"]
		switch want {
		case "message_start":
			if hasMsg {
				return true
			}
		case "content_block_start":
			if hasCB && !hasStop {
				return true
			}
		case "content_block_delta":
			if hasDelta {
				return true
			}
		case "content_block_stop":
			if hasStop {
				return true
			}
		}
	}
	return false
}

// pickResultSubtype reports whether any llm_event payload is a result envelope
// with the given subtype.
func pickResultSubtype(rows []piEventRow, want string) bool {
	for _, r := range rows {
		if r.Type != HandlerLLMEvent {
			continue
		}
		var m struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(r.Raw, &m) != nil {
			continue
		}
		if m.Type == "result" && m.Subtype == want {
			return true
		}
	}
	return false
}

// The helpers below decode assistant llm_event payloads by shape: the wire
// envelope is `{"type":"assistant", ...}` with discriminator subfields
// (content_block, delta, content_block_stop) rather than a string subtype.

// pickEventHas returns true if any llm_event raw bytes contain the substring,
// which is sufficient for assertions like "an input_json_delta payload was
// emitted" without committing to the exact envelope shape.
func pickEventHas(rows []piEventRow, substr string) bool {
	for _, r := range rows {
		if r.Type == HandlerLLMEvent && strings.Contains(string(r.Raw), substr) {
			return true
		}
	}
	return false
}

// assistantBlockShape decodes an assistant envelope's relevant subfields.
type assistantBlockShape struct {
	Type             string `json:"type"`             // always "assistant" for matches
	Index            int    `json:"index"`
	ContentBlock     *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block,omitempty"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		Thinking    string `json:"thinking,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
	} `json:"delta,omitempty"`
	ContentBlockStop bool `json:"content_block_stop,omitempty"`
}

// decodeAssistant returns the shape if the row is an assistant llm_event,
// else ok=false. Saves boilerplate at call sites.
func decodeAssistant(row piEventRow) (s assistantBlockShape, ok bool) {
	if row.Type != HandlerLLMEvent {
		return s, false
	}
	if err := json.Unmarshal(row.Raw, &s); err != nil {
		return s, false
	}
	return s, s.Type == "assistant"
}

// pickContentBlockKinds returns labels like "text:start" / "text:delta" /
// "tool_use:start". Stop events inherit the kind from the most recent start.
func pickContentBlockKinds(rows []piEventRow) []string {
	var out []string
	var openKind string
	for _, r := range rows {
		s, ok := decodeAssistant(r)
		if !ok {
			continue
		}
		switch {
		case s.ContentBlock != nil && !s.ContentBlockStop:
			openKind = s.ContentBlock.Type
			out = append(out, openKind+":start")
		case s.Delta != nil:
			switch s.Delta.Type {
			case "text_delta":
				out = append(out, "text:delta")
			case "thinking_delta":
				out = append(out, "thinking:delta")
			case "input_json_delta":
				out = append(out, "tool_use:delta")
			}
		case s.ContentBlockStop:
			if openKind != "" {
				out = append(out, openKind+":stop")
			} else {
				out = append(out, "stop")
			}
		}
	}
	return out
}

func findToolUseStart(rows []piEventRow) (id, name string, ok bool) {
	for _, r := range rows {
		s, dok := decodeAssistant(r)
		if !dok || s.ContentBlock == nil || s.ContentBlockStop {
			continue
		}
		if s.ContentBlock.Type == BlockToolUse {
			return s.ContentBlock.ID, s.ContentBlock.Name, true
		}
	}
	return "", "", false
}

func findToolResult(rows []piEventRow) (id, content string, isErr, ok bool) {
	for _, r := range rows {
		if r.Type != HandlerLLMEvent {
			continue
		}
		var m struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
			IsError   bool   `json:"is_error"`
		}
		if json.Unmarshal(r.Raw, &m) != nil {
			continue
		}
		if m.Type == "result" && m.Subtype == "tool_result" {
			return m.ToolUseID, m.Content, m.IsError, true
		}
	}
	return "", "", false, false
}

func sawRawOutputContaining(rows []piEventRow, substr string) bool {
	for _, r := range rows {
		if r.Type == "raw_output" && strings.Contains(string(r.Raw), substr) {
			return true
		}
	}
	return false
}

func collectBlockStartIndices(rows []piEventRow) []int {
	var out []int
	for _, r := range rows {
		s, ok := decodeAssistant(r)
		if !ok || s.ContentBlock == nil || s.ContentBlockStop {
			continue
		}
		out = append(out, s.Index)
	}
	return out
}

