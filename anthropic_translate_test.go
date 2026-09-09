package main

// Coverage for the pure Anthropic <-> OpenAI translation in
// anthropic_translate.go — request shape translation and the OpenAI-SSE ->
// Anthropic-SSE streaming state machine. No HTTP here; relay_router_anthropic_test.go
// covers the router-level wiring (dispatch, passthrough, credential handling).

import (
	"bytes"
	"encoding/json"
	"testing"
)

func decodeOpenAIRequest(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode translated request %q: %v", body, err)
	}
	return out
}

func TestAnthropicToOpenAIRequest_SystemAndTextMessage(t *testing.T) {
	anthropicBody := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": "be terse",
		"messages": [{"role": "user", "content": "hello there"}],
		"max_tokens": 512,
		"stream": true
	}`)

	openaiBody, wantStream, aerr := anthropicToOpenAIRequest(anthropicBody, "qwen3-8b", anthropicTranslateOpts{})
	if aerr != nil {
		t.Fatalf("unexpected error: %+v", aerr)
	}
	if !wantStream {
		t.Errorf("wantStream = false, want true (client sent stream:true)")
	}

	got := decodeOpenAIRequest(t, openaiBody)
	if got["model"] != "qwen3-8b" {
		t.Errorf("model = %v, want target %q", got["model"], "qwen3-8b")
	}
	if got["stream"] != true {
		t.Errorf("stream = %v, want true (always requested upstream regardless of client's original value)", got["stream"])
	}
	if got["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v, want 512", got["max_tokens"])
	}
	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2 entries (system + user)", got["messages"])
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be terse" {
		t.Errorf("system message = %v, want role=system content=%q", sys, "be terse")
	}
	user := msgs[1].(map[string]any)
	userParts, ok := user["content"].([]any)
	if user["role"] != "user" || !ok || len(userParts) != 1 {
		t.Fatalf("user message = %v, want role=user content=[one text part]", user)
	}
	part := userParts[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "hello there" {
		t.Errorf("user content part = %v, want type=text text=%q", part, "hello there")
	}
}

func TestAnthropicToOpenAIRequest_NonStreamingClientStillStreamsUpstream(t *testing.T) {
	anthropicBody := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`) // no "stream" field -> defaults false

	openaiBody, wantStream, aerr := anthropicToOpenAIRequest(anthropicBody, "t", anthropicTranslateOpts{})
	if aerr != nil {
		t.Fatalf("unexpected error: %+v", aerr)
	}
	if wantStream {
		t.Errorf("wantStream = true, want false (client omitted stream)")
	}
	got := decodeOpenAIRequest(t, openaiBody)
	if got["stream"] != true {
		t.Errorf("upstream stream = %v, want true regardless of client's own stream value", got["stream"])
	}
}

func TestAnthropicToOpenAIRequest_ToolUseAndToolResult(t *testing.T) {
	anthropicBody := []byte(`{
		"model": "x",
		"messages": [
			{"role": "user", "content": "what's 2+2"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "let me check"},
				{"type": "tool_use", "id": "toolu_1", "name": "calc", "input": {"expr": "2+2"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "4"}
			]}
		]
	}`)

	openaiBody, _, aerr := anthropicToOpenAIRequest(anthropicBody, "t", anthropicTranslateOpts{})
	if aerr != nil {
		t.Fatalf("unexpected error: %+v", aerr)
	}
	got := decodeOpenAIRequest(t, openaiBody)
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d entries, want 3 (user, assistant-with-tool_calls, tool)", len(msgs))
	}

	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" || assistant["content"] != "let me check" {
		t.Errorf("assistant message = %v", assistant)
	}
	toolCalls, ok := assistant["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("assistant.tool_calls = %v, want 1 entry", assistant["tool_calls"])
	}
	tc := toolCalls[0].(map[string]any)
	if tc["id"] != "toolu_1" {
		t.Errorf("tool_calls[0].id = %v, want toolu_1 (Anthropic's own id preserved)", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "calc" {
		t.Errorf("tool_calls[0].function.name = %v, want calc", fn["name"])
	}

	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_1" || toolMsg["content"] != "4" {
		t.Errorf("tool message = %v, want role=tool tool_call_id=toolu_1 content=4", toolMsg)
	}
}

func TestAnthropicToOpenAIRequest_ToolResultError(t *testing.T) {
	anthropicBody := []byte(`{
		"model": "x",
		"messages": [{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "t1", "content": "boom", "is_error": true}
		]}]
	}`)
	openaiBody, _, aerr := anthropicToOpenAIRequest(anthropicBody, "t", anthropicTranslateOpts{})
	if aerr != nil {
		t.Fatalf("unexpected error: %+v", aerr)
	}
	got := decodeOpenAIRequest(t, openaiBody)
	msgs := got["messages"].([]any)
	toolMsg := msgs[0].(map[string]any)
	if toolMsg["content"] != "Error: boom" {
		t.Errorf("content = %v, want %q", toolMsg["content"], "Error: boom")
	}
}

func TestAnthropicToOpenAIRequest_ImagesStrippedWhenTargetLacksVision(t *testing.T) {
	anthropicBody := []byte(`{
		"model": "x",
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "what is this"},
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
		]}]
	}`)
	openaiBody, _, aerr := anthropicToOpenAIRequest(anthropicBody, "t", anthropicTranslateOpts{SupportsImages: false})
	if aerr != nil {
		t.Fatalf("unexpected error: %+v", aerr)
	}
	if bytes.Contains(openaiBody, []byte("image_url")) {
		t.Errorf("translated body contains image_url despite SupportsImages:false: %s", openaiBody)
	}
	if !bytes.Contains(openaiBody, []byte("image omitted")) {
		t.Errorf("translated body missing image-omitted placeholder: %s", openaiBody)
	}
}

func TestAnthropicToOpenAIRequest_ImagesKeptWhenTargetSupportsVision(t *testing.T) {
	anthropicBody := []byte(`{
		"model": "x",
		"messages": [{"role": "user", "content": [
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
		]}]
	}`)
	openaiBody, _, aerr := anthropicToOpenAIRequest(anthropicBody, "t", anthropicTranslateOpts{SupportsImages: true})
	if aerr != nil {
		t.Fatalf("unexpected error: %+v", aerr)
	}
	if !bytes.Contains(openaiBody, []byte("data:image/png;base64,AAAA")) {
		t.Errorf("translated body missing data URL: %s", openaiBody)
	}
}

func TestAnthropicToOpenAIRequest_ServerToolRefused(t *testing.T) {
	anthropicBody := []byte(`{
		"model": "x",
		"messages": [{"role": "user", "content": "search the web"}],
		"tools": [{"type": "web_search_20250305", "name": "web_search"}]
	}`)
	_, _, aerr := anthropicToOpenAIRequest(anthropicBody, "t", anthropicTranslateOpts{})
	if aerr == nil {
		t.Fatal("expected a refusal for a server-side tool type, got nil error")
	}
	if aerr.status != 400 || aerr.errType != "invalid_request_error" {
		t.Errorf("error = %+v, want 400 invalid_request_error", aerr)
	}
}

func TestAnthropicToOpenAIRequest_CustomToolTranslated(t *testing.T) {
	anthropicBody := []byte(`{
		"model": "x",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"name": "get_weather", "description": "gets weather", "input_schema": {"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice": {"type": "any"}
	}`)
	openaiBody, _, aerr := anthropicToOpenAIRequest(anthropicBody, "t", anthropicTranslateOpts{})
	if aerr != nil {
		t.Fatalf("unexpected error: %+v", aerr)
	}
	got := decodeOpenAIRequest(t, openaiBody)
	tools := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 entry", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tools[0].function.name = %v, want get_weather", fn["name"])
	}
	if got["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v, want %q (any -> required)", got["tool_choice"], "required")
	}
}

func TestAnthropicToOpenAIRequest_ThinkingDisabledMapsToReasoningEffortNone(t *testing.T) {
	anthropicBody := []byte(`{
		"model": "x",
		"messages": [{"role": "user", "content": "hi"}],
		"thinking": {"type": "disabled"}
	}`)
	openaiBody, _, aerr := anthropicToOpenAIRequest(anthropicBody, "t", anthropicTranslateOpts{})
	if aerr != nil {
		t.Fatalf("unexpected error: %+v", aerr)
	}
	got := decodeOpenAIRequest(t, openaiBody)
	if got["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort = %v, want none", got["reasoning_effort"])
	}
}

func TestAnthropicToOpenAIRequest_AffinityKeyFromMetadataSessionID(t *testing.T) {
	anthropicBody := []byte(`{
		"model": "x",
		"messages": [{"role": "user", "content": "hi"}],
		"metadata": {"user_id": "{\"device_id\":\"d1\",\"session_id\":\"sess-123\"}"}
	}`)
	openaiBody, _, aerr := anthropicToOpenAIRequest(anthropicBody, "t", anthropicTranslateOpts{})
	if aerr != nil {
		t.Fatalf("unexpected error: %+v", aerr)
	}
	got := decodeOpenAIRequest(t, openaiBody)
	if got["prompt_cache_key"] != "sess-123" {
		t.Errorf("prompt_cache_key = %v, want sess-123", got["prompt_cache_key"])
	}
}

// TestAnthropicToOpenAIRequest_MidConversationSystemMessageMergedToFront is a
// regression test for a live 500: a real llama.cpp backend (Qwen's Jinja
// chat template) raises "System message must be at the beginning" the
// moment a SECOND role:"system" message appears anywhere past index 0.
// Anthropic supports mid-conversation system messages (a beta feature); most
// OpenAI-facing chat templates do not. Every system source — the top-level
// "system" field and any mid-conversation role:"system" message — must
// collapse into exactly one leading message.
func TestAnthropicToOpenAIRequest_MidConversationSystemMessageMergedToFront(t *testing.T) {
	anthropicBody := []byte(`{
		"model": "x",
		"system": "top-level instructions",
		"messages": [
			{"role": "user", "content": "first question"},
			{"role": "assistant", "content": "first answer"},
			{"role": "system", "content": "mid-conversation reminder"},
			{"role": "user", "content": "second question"}
		]
	}`)
	openaiBody, _, aerr := anthropicToOpenAIRequest(anthropicBody, "t", anthropicTranslateOpts{})
	if aerr != nil {
		t.Fatalf("unexpected error: %+v", aerr)
	}
	got := decodeOpenAIRequest(t, openaiBody)
	msgs := got["messages"].([]any)

	var systemCount int
	for i, m := range msgs {
		msg := m.(map[string]any)
		if msg["role"] == "system" {
			systemCount++
			if i != 0 {
				t.Errorf("system message found at index %d, want only ever at index 0", i)
			}
		}
	}
	if systemCount != 1 {
		t.Fatalf("found %d system messages, want exactly 1 (merged)", systemCount)
	}
	first := msgs[0].(map[string]any)
	wantContent := "top-level instructions\n\nmid-conversation reminder"
	if first["content"] != wantContent {
		t.Errorf("merged system content = %q, want %q", first["content"], wantContent)
	}

	// The non-system messages must survive in their original relative order.
	if len(msgs) != 4 { // 1 merged system + 3 user/assistant turns
		t.Fatalf("messages = %d entries, want 4 (1 system + 3 turns), got %v", len(msgs), msgs)
	}
}

func TestAnthropicToOpenAIRequest_NoMessagesRejected(t *testing.T) {
	_, _, aerr := anthropicToOpenAIRequest([]byte(`{"model":"x","messages":[]}`), "t", anthropicTranslateOpts{})
	if aerr == nil || aerr.status != 400 {
		t.Fatalf("expected 400 for empty messages, got %+v", aerr)
	}
}

// ---------------------------------------------------------------------------
// Streaming state machine
// ---------------------------------------------------------------------------

type sseEventPair struct {
	event string
	data  map[string]any
}

func parseSSEEvents(t *testing.T, raw []byte) []sseEventPair {
	t.Helper()
	var out []sseEventPair
	blocks := bytes.Split(bytes.TrimSpace(raw), []byte("\n\n"))
	for _, block := range blocks {
		block = bytes.TrimSpace(block)
		if len(block) == 0 {
			continue
		}
		var event string
		var dataLine []byte
		for _, line := range bytes.Split(block, []byte("\n")) {
			line = bytes.TrimSpace(line)
			switch {
			case bytes.HasPrefix(line, []byte("event:")):
				event = string(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("event:"))))
			case bytes.HasPrefix(line, []byte("data:")):
				dataLine = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			}
		}
		var data map[string]any
		if len(dataLine) > 0 {
			if err := json.Unmarshal(dataLine, &data); err != nil {
				t.Fatalf("parse SSE data %q: %v", dataLine, err)
			}
		}
		out = append(out, sseEventPair{event: event, data: data})
	}
	return out
}

func eventTypes(events []sseEventPair) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.event
	}
	return out
}

func TestAnthropicStreamTranslator_TextStreaming(t *testing.T) {
	tr := newAnthropicStreamTranslator(true, "claude-sonnet-4-5", 10)
	var out bytes.Buffer
	tr.Start(&out)
	tr.Feed([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"},\"finish_reason\":null}]}\n\n"), &out)
	tr.Feed([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n"), &out)
	tr.Feed([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n"), &out)
	tr.Finish(&out)

	events := parseSSEEvents(t, out.Bytes())
	got := eventTypes(events)
	want := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop",
	}
	if len(got) != len(want) {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q (full sequence: %v)", i, got[i], want[i], got)
		}
	}

	msg := events[0].data["message"].(map[string]any)
	if msg["model"] != "claude-sonnet-4-5" {
		t.Errorf("message_start.message.model = %v, want the requested model echoed back", msg["model"])
	}

	delta := events[len(events)-2].data["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Errorf("message_delta.delta.stop_reason = %v, want end_turn", delta["stop_reason"])
	}
	usage := events[len(events)-2].data["usage"].(map[string]any)
	if usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(2) {
		t.Errorf("message_delta.usage = %v, want input=5 output=2 (real usage from the backend, not the estimate)", usage)
	}
}

func TestAnthropicStreamTranslator_PartialSSEChunkAcrossFeedCalls(t *testing.T) {
	tr := newAnthropicStreamTranslator(true, "m", 0)
	var out bytes.Buffer
	tr.Start(&out)
	full := `data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"
	// Split mid-line, including mid-JSON, to prove the line buffer handles an
	// arbitrary split point, not just one that happens to land on "\n\n".
	split := len(full) / 2
	tr.Feed([]byte(full[:split]), &out)
	tr.Feed([]byte(full[split:]), &out)
	tr.Finish(&out)

	events := parseSSEEvents(t, out.Bytes())
	found := false
	for _, e := range events {
		if e.event == "content_block_delta" && e.data["delta"].(map[string]any)["text"] == "hi" {
			found = true
		}
	}
	if !found {
		t.Errorf("text delta split across two Feed() calls was not reassembled; events: %v", events)
	}
}

func TestAnthropicStreamTranslator_ToolCallsBufferedAndEmittedAtEnd(t *testing.T) {
	tr := newAnthropicStreamTranslator(true, "m", 0)
	var out bytes.Buffer
	tr.Start(&out)
	// Interleaved fragments across two tool-call indices, as OpenAI permits
	// but Anthropic's strictly-sequential blocks cannot represent live.
	tr.Feed([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"{\"x\":"}}]}}]}`+"\n\n"), &out)
	tr.Feed([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"b","arguments":"{\"y\":2}"}}]}}]}`+"\n\n"), &out)
	tr.Feed([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`+"\n\n"), &out)
	tr.Feed([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n"), &out)
	tr.Finish(&out)

	events := parseSSEEvents(t, out.Bytes())
	// No content_block_start for text should appear at all (no text deltas were fed).
	var toolStarts []map[string]any
	for _, e := range events {
		if e.event == "content_block_start" {
			toolStarts = append(toolStarts, e.data["content_block"].(map[string]any))
		}
	}
	if len(toolStarts) != 2 {
		t.Fatalf("content_block_start count = %d, want 2 (one per tool call)", len(toolStarts))
	}
	if toolStarts[0]["name"] != "a" || toolStarts[1]["name"] != "b" {
		t.Errorf("tool blocks out of order: got %v then %v, want a then b (declaration order)", toolStarts[0]["name"], toolStarts[1]["name"])
	}

	var deltas []map[string]any
	for _, e := range events {
		if e.event == "content_block_delta" {
			deltas = append(deltas, e.data["delta"].(map[string]any))
		}
	}
	if len(deltas) != 2 {
		t.Fatalf("content_block_delta count = %d, want 2", len(deltas))
	}
	if deltas[0]["partial_json"] != `{"x":1}` {
		t.Errorf("tool 0 args = %v, want reassembled %q", deltas[0]["partial_json"], `{"x":1}`)
	}

	lastDelta := events[len(events)-2].data["delta"].(map[string]any)
	if lastDelta["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", lastDelta["stop_reason"])
	}
}

func TestAnthropicStreamTranslator_MalformedToolArgsFallBackToEmptyObject(t *testing.T) {
	tr := newAnthropicStreamTranslator(true, "m", 0)
	var out bytes.Buffer
	tr.Start(&out)
	tr.Feed([]byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"not json"}}]}}]}`+"\n\n"), &out)
	tr.Finish(&out)

	events := parseSSEEvents(t, out.Bytes())
	for _, e := range events {
		if e.event == "content_block_delta" {
			if e.data["delta"].(map[string]any)["partial_json"] != "{}" {
				t.Errorf("partial_json = %v, want {} fallback for invalid JSON args", e.data["delta"])
			}
		}
	}
}

func TestAnthropicStreamTranslator_NonStreamingAggregation(t *testing.T) {
	tr := newAnthropicStreamTranslator(false, "m", 0)
	tr.Start(nil) // no-op in non-streaming mode
	tr.Feed([]byte(`data: {"choices":[{"index":0,"delta":{"content":"answer: 4"},"finish_reason":null}]}`+"\n\n"), nil)
	tr.Feed([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`+"\n\n"), nil)
	tr.Finish(nil) // no-op in non-streaming mode; BuildMessage is the real output

	msg := tr.BuildMessage()
	content := msg["content"].([]map[string]any)
	if len(content) != 1 || content[0]["text"] != "answer: 4" {
		t.Fatalf("content = %v, want one text block %q", content, "answer: 4")
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", msg["stop_reason"])
	}
	usage := msg["usage"].(map[string]any)
	if usage["input_tokens"] != int64(3) || usage["output_tokens"] != int64(4) {
		t.Errorf("usage = %v, want input=3 output=4", usage)
	}
}

// ---------------------------------------------------------------------------
// Backend error -> Anthropic error envelope mapping
// ---------------------------------------------------------------------------

func TestMapAnthropicBackendError_ContextOverflowNormalized(t *testing.T) {
	body := []byte(`{"error":{"message":"the request exceeds the available context size (32768 tokens)"}}`)
	mapped := mapAnthropicBackendError(400, body)
	if mapped.status != 400 || mapped.errType != "invalid_request_error" {
		t.Fatalf("mapped = %+v, want 400 invalid_request_error", mapped)
	}
	if !bytes.HasPrefix([]byte(mapped.message), []byte("prompt is too long:")) {
		t.Errorf("message = %q, want prefix %q", mapped.message, "prompt is too long:")
	}
}

func TestMapAnthropicBackendError_OverloadedFor5xxGateway(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		mapped := mapAnthropicBackendError(status, []byte(`{"error":"down"}`))
		if mapped.errType != "overloaded_error" {
			t.Errorf("status %d: errType = %q, want overloaded_error", status, mapped.errType)
		}
	}
}

func TestMapAnthropicBackendError_AuthAndGenericMapping(t *testing.T) {
	cases := map[int]string{
		401: "authentication_error",
		403: "permission_error",
		404: "not_found_error",
		429: "rate_limit_error",
		400: "invalid_request_error",
		500: "api_error",
	}
	for status, wantType := range cases {
		mapped := mapAnthropicBackendError(status, []byte(`{"error":{"message":"nope"}}`))
		if mapped.errType != wantType {
			t.Errorf("status %d: errType = %q, want %q", status, mapped.errType, wantType)
		}
	}
}
