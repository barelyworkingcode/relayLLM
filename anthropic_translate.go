package main

// Pure Anthropic Messages API <-> OpenAI Chat Completions translation. No
// I/O, no globals — mirrors provider_pi.go's translate seam (ADR-003/008):
// every function here is a plain data transform, testable without a server.
//
// Scope is deliberately trimmed from the full Anthropic surface to what's
// load-bearing for Claude Code's common path (system + text + images + tool
// calls). See docs/decisions/013-anthropic-messages-compat.md for what's
// explicitly out of scope (extended thinking, prompt caching, documents,
// structured outputs, server-side tools).

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// ---------------------------------------------------------------------------
// Anthropic request shape (only the fields this translator reads)
// ---------------------------------------------------------------------------

type anthropicContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`

	// tool_use (assistant turn)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (user turn)
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or []anthropicContentBlock
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []anthropicContentBlock
}

type anthropicTool struct {
	Type        string          `json:"type,omitempty"` // "" or "custom" == user-defined; anything else is a server tool we can't serve locally
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type anthropicToolChoice struct {
	Type string `json:"type"` // "auto" | "any" | "tool" | "none"
	Name string `json:"name,omitempty"`
}

type anthropicThinking struct {
	Type string `json:"type"` // "enabled" | "disabled" | "adaptive"
}

type anthropicMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

type anthropicRequest struct {
	Model       string               `json:"model"`
	Messages    []anthropicMessage   `json:"messages"`
	System      json.RawMessage      `json:"system,omitempty"`
	MaxTokens   int                  `json:"max_tokens,omitempty"`
	Temperature *float64             `json:"temperature,omitempty"`
	TopP        *float64             `json:"top_p,omitempty"`
	TopK        *int                 `json:"top_k,omitempty"`
	StopSeqs    []string             `json:"stop_sequences,omitempty"`
	Stream      bool                 `json:"stream,omitempty"`
	Tools       []anthropicTool      `json:"tools,omitempty"`
	ToolChoice  *anthropicToolChoice `json:"tool_choice,omitempty"`
	Thinking    *anthropicThinking   `json:"thinking,omitempty"`
	Metadata    *anthropicMetadata   `json:"metadata,omitempty"`
}

// anthropicError is both a translate-time refusal and the shape
// writeAnthropicError needs to answer the client with the real Anthropic
// error envelope instead of a generic 500.
type anthropicError struct {
	status  int
	errType string
	message string
}

func (e *anthropicError) Error() string { return e.message }

func newAnthropicError(status int, errType, message string) *anthropicError {
	return &anthropicError{status: status, errType: errType, message: message}
}

// ---------------------------------------------------------------------------
// Request translation
// ---------------------------------------------------------------------------

// anthropicTranslateOpts carries target-dependent decisions the pure
// translator can't determine on its own (whether the redirect target's
// catalog entry advertises vision support) — resolved by the caller so this
// file stays free of any RelayRouter/manager dependency.
type anthropicTranslateOpts struct {
	SupportsImages bool
}

// anthropicToOpenAIRequest translates an Anthropic /v1/messages body into an
// OpenAI Chat Completions body targeting the given model id. Returns the
// client's original "stream" value (the caller always requests streaming
// upstream regardless — see relay_router_anthropic.go — and aggregates
// locally when the client asked for a non-streaming response).
func anthropicToOpenAIRequest(body []byte, target string, opts anthropicTranslateOpts) ([]byte, bool, *anthropicError) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, newAnthropicError(400, "invalid_request_error", "invalid request body: "+err.Error())
	}
	if len(req.Messages) == 0 {
		return nil, false, newAnthropicError(400, "invalid_request_error", "messages: at least one message is required")
	}

	// System content is collected from every source — the top-level "system"
	// field and any mid-conversation role:"system" message (Anthropic's beta
	// for that) — and emitted as exactly ONE leading system message, not one
	// per source at its original position. Measured live against a real
	// backend (Qwen's Jinja chat template via llama.cpp): a second
	// role:"system" message anywhere past index 0 raises "System message
	// must be at the beginning" and 500s the whole request. Anthropic
	// supports multiple/mid-conversation system messages; most OpenAI-facing
	// chat templates do not, and folding them all to the front is the
	// difference between a working redirect and every mid-conversation
	// system reminder Claude Code sends breaking the request outright.
	var systemParts []string
	if len(req.System) > 0 {
		text, err := anthropicSystemText(req.System)
		if err != nil {
			return nil, false, newAnthropicError(400, "invalid_request_error", "invalid system field: "+err.Error())
		}
		if text != "" {
			systemParts = append(systemParts, text)
		}
	}

	var openaiMessages []map[string]any
	for _, m := range req.Messages {
		blocks, err := anthropicContentBlocks(m.Content)
		if err != nil {
			return nil, false, newAnthropicError(400, "invalid_request_error", "invalid message content: "+err.Error())
		}
		if !opts.SupportsImages {
			blocks = stripAnthropicImages(blocks)
		}
		if m.Role == "system" {
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					systemParts = append(systemParts, b.Text)
				}
			}
			continue
		}
		msgs, aerr := translateAnthropicMessage(m.Role, blocks)
		if aerr != nil {
			return nil, false, aerr
		}
		openaiMessages = append(openaiMessages, msgs...)
	}
	if len(systemParts) > 0 {
		openaiMessages = append([]map[string]any{{"role": "system", "content": strings.Join(systemParts, "\n\n")}}, openaiMessages...)
	}

	out := map[string]any{
		"model":          target,
		"messages":       openaiMessages,
		"stream":         true, // always stream upstream; caller aggregates when the client didn't ask for streaming
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		out["top_k"] = *req.TopK
	}
	if len(req.StopSeqs) > 0 {
		out["stop"] = req.StopSeqs
	}
	if len(req.Tools) > 0 {
		tools, aerr := anthropicToolsToOpenAI(req.Tools)
		if aerr != nil {
			return nil, false, aerr
		}
		out["tools"] = tools
		if choice := anthropicToolChoiceToOpenAI(req.ToolChoice); choice != nil {
			out["tool_choice"] = choice
		}
	}
	if req.Thinking != nil && req.Thinking.Type == "disabled" {
		out["reasoning_effort"] = "none"
	}
	if key := anthropicAffinityKey(req.Metadata); key != "" {
		out["prompt_cache_key"] = key
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, false, newAnthropicError(500, "api_error", "failed to encode translated request: "+err.Error())
	}
	return encoded, req.Stream, nil
}

// anthropicAffinityKey extracts a stable per-conversation identifier from
// Claude Code's metadata.user_id, which carries a JSON object (not a plain
// user id) containing "session_id" among other fields. Falls back to the raw
// string when it isn't that shape — still a stable, unique-enough key for
// conversation affinity (ADR-010), just less legible in logs.
func anthropicAffinityKey(metadata *anthropicMetadata) string {
	if metadata == nil || metadata.UserID == "" {
		return ""
	}
	var parsed struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(metadata.UserID), &parsed); err == nil && parsed.SessionID != "" {
		return parsed.SessionID
	}
	return metadata.UserID
}

// anthropicContentBlocks normalizes an Anthropic "content" field, which is
// either a plain string or an array of typed blocks, into the array shape.
func anthropicContentBlocks(raw json.RawMessage) ([]anthropicContentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		if s == "" {
			return nil, nil
		}
		return []anthropicContentBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// anthropicSystemText joins an Anthropic "system" field's text blocks (or
// returns the field directly if it's a plain string) into one string for an
// OpenAI leading system message. cache_control on any block is dropped —
// prompt caching does not survive this translation (see the ADR).
func anthropicSystemText(raw json.RawMessage) (string, error) {
	blocks, err := anthropicContentBlocks(raw)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" || b.Type == "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func stripAnthropicImages(blocks []anthropicContentBlock) []anthropicContentBlock {
	out := make([]anthropicContentBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "image" {
			out = append(out, anthropicContentBlock{Type: "text", Text: "[image omitted: target model does not support vision]"})
			continue
		}
		out = append(out, b)
	}
	return out
}

// translateAnthropicMessage converts one Anthropic "user" or "assistant"
// message into zero or more OpenAI messages. tool_result blocks become their
// own {role:tool} messages, emitted before any remaining content in the same
// Anthropic message — mirroring how Anthropic already orders a tool-results
// turn ahead of any trailing user text. role:"system" never reaches this
// function — anthropicToOpenAIRequest intercepts it before the call to fold
// every system source into one leading message (see its comment for why).
func translateAnthropicMessage(role string, blocks []anthropicContentBlock) ([]map[string]any, *anthropicError) {
	var out []map[string]any
	switch role {
	case "user":
		var toolMsgs []map[string]any
		var otherParts []map[string]any
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				toolMsgs = append(toolMsgs, anthropicToolResultToOpenAI(b))
			case "text":
				otherParts = append(otherParts, map[string]any{"type": "text", "text": b.Text})
			case "image":
				if part, ok := anthropicImageToOpenAI(b); ok {
					otherParts = append(otherParts, part)
				} else {
					otherParts = append(otherParts, map[string]any{"type": "text", "text": "[image omitted]"})
				}
			case "document":
				otherParts = append(otherParts, map[string]any{"type": "text", "text": "[document omitted]"})
			default:
				otherParts = append(otherParts, map[string]any{"type": "text", "text": fmt.Sprintf("[unsupported content: %s]", b.Type)})
			}
		}
		out = append(out, toolMsgs...)
		if len(otherParts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": otherParts})
		}
	case "assistant":
		var textParts []string
		var toolCalls []map[string]any
		for _, b := range blocks {
			switch b.Type {
			case "text":
				textParts = append(textParts, b.Text)
			case "tool_use":
				args := "{}"
				if len(b.Input) > 0 {
					args = string(b.Input)
				}
				id := b.ID
				if id == "" {
					id = "toolu_" + randHex(12)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      b.Name,
						"arguments": args,
					},
				})
			case "thinking", "redacted_thinking":
				// Dropped — see the ADR's deferred-scope list. A thinking
				// block's signature can't be reconstructed from an OpenAI
				// backend, and replaying an empty one back to real Anthropic
				// later is a worse failure than never having emitted it.
			default:
				textParts = append(textParts, fmt.Sprintf("[unsupported content: %s]", b.Type))
			}
		}
		msg := map[string]any{"role": "assistant"}
		if len(textParts) > 0 {
			msg["content"] = strings.Join(textParts, "\n\n")
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		out = append(out, msg)
	default:
		return nil, newAnthropicError(400, "invalid_request_error", fmt.Sprintf("unsupported message role %q", role))
	}
	return out, nil
}

func anthropicToolResultToOpenAI(b anthropicContentBlock) map[string]any {
	text := anthropicToolResultText(b.Content)
	if b.IsError {
		text = "Error: " + text
	}
	return map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": text}
}

// anthropicToolResultText renders a tool_result block's "content" field
// (string or []anthropicContentBlock) as plain text — the only shape an
// OpenAI tool message's content field takes.
func anthropicToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	blocks, err := anthropicContentBlocks(raw)
	if err != nil {
		return string(raw)
	}
	var parts []string
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			parts = append(parts, blk.Text)
		case "image":
			parts = append(parts, "[image omitted]")
		default:
			parts = append(parts, fmt.Sprintf("[%s omitted]", blk.Type))
		}
	}
	return strings.Join(parts, "\n\n")
}

func anthropicImageToOpenAI(b anthropicContentBlock) (map[string]any, bool) {
	if b.Source == nil {
		return nil, false
	}
	switch b.Source.Type {
	case "base64":
		if b.Source.Data == "" || b.Source.MediaType == "" {
			return nil, false
		}
		url := fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data)
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, true
	case "url":
		if b.Source.URL == "" {
			return nil, false
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": b.Source.URL}}, true
	default:
		return nil, false
	}
}

func anthropicToolsToOpenAI(tools []anthropicTool) ([]map[string]any, *anthropicError) {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "custom" {
			return nil, newAnthropicError(400, "invalid_request_error",
				fmt.Sprintf("tool type %q is a server-side Anthropic tool and cannot be served by a redirected local model", t.Type))
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  schema,
			},
		})
	}
	return out, nil
}

func anthropicToolChoiceToOpenAI(tc *anthropicToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// OpenAI SSE -> Anthropic event stream state machine
// ---------------------------------------------------------------------------

type anthropicUsage struct {
	InputTokens  int64
	OutputTokens int64
}

type anthropicToolCallState struct {
	id      string
	name    string
	argsBuf bytes.Buffer
}

// anthropicStreamTranslator accumulates one response's worth of OpenAI SSE
// deltas and either streams the equivalent Anthropic SSE events live
// (streaming == true) or silently builds internal state for BuildMessage to
// render as one JSON Message (streaming == false). Text streams live; tool
// calls are buffered and emitted as complete blocks at Finish — see
// docs/decisions/013-anthropic-messages-compat.md for why (OpenAI permits
// interleaved tool-call fragments across indices; Anthropic content blocks
// are strictly sequential, and nothing can execute a tool before
// message_stop anyway).
type anthropicStreamTranslator struct {
	streaming           bool
	requestedModel      string
	inputTokensEstimate int64

	messageID      string
	textStarted    bool
	textBlockIndex int
	nextBlockIndex int
	toolCalls      map[int]*anthropicToolCallState
	toolOrder      []int
	finishReason   string
	usage          anthropicUsage
	sseBuf         bytes.Buffer
	textAccum      strings.Builder
}

func newAnthropicStreamTranslator(streaming bool, model string, inputTokensEstimate int64) *anthropicStreamTranslator {
	return &anthropicStreamTranslator{
		streaming:           streaming,
		requestedModel:      model,
		inputTokensEstimate: inputTokensEstimate,
		toolCalls:           make(map[int]*anthropicToolCallState),
	}
}

// Start emits message_start. No-op in non-streaming mode (BuildMessage emits
// the whole message at once instead).
func (t *anthropicStreamTranslator) Start(w io.Writer) {
	if !t.streaming {
		return
	}
	t.messageID = "msg_" + randHex(24)
	writeSSEEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            t.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         t.requestedModel,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                t.inputTokensEstimate,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	})
}

// Feed consumes a chunk of raw OpenAI SSE bytes (which may split a "data:
// ...\n\n" event across multiple calls) and applies every complete event it
// finds. In streaming mode, text deltas are written to w immediately as
// Anthropic content_block_start/delta events; tool-call deltas are always
// buffered regardless of mode.
func (t *anthropicStreamTranslator) Feed(chunk []byte, w io.Writer) {
	if len(chunk) > 0 {
		t.sseBuf.Write(chunk)
	}
	for {
		buf := t.sseBuf.Bytes()
		idx := bytes.Index(buf, []byte("\n\n"))
		if idx < 0 {
			return
		}
		event := make([]byte, idx)
		copy(event, buf[:idx])
		t.sseBuf.Next(idx + 2)
		t.applyEvent(event, w)
	}
}

func (t *anthropicStreamTranslator) applyEvent(event []byte, w io.Writer) {
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // ignore "event:" lines, comments, keepalive blanks
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || string(payload) == "[DONE]" {
			continue
		}
		var chunk openaiStreamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			slog.Debug("relay router: anthropic translate: skipping unparseable SSE chunk", "error", err)
			continue
		}
		t.applyChunk(chunk, w)
	}
}

type openaiStreamChunk struct {
	Choices []openaiStreamChoice `json:"choices"`
	Usage   *openaiUsage         `json:"usage"`
}

type openaiStreamChoice struct {
	Delta        openaiDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

type openaiDelta struct {
	Content   string                `json:"content"`
	ToolCalls []openaiToolCallDelta `json:"tool_calls"`
}

type openaiToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

func (t *anthropicStreamTranslator) applyChunk(chunk openaiStreamChunk, w io.Writer) {
	if chunk.Usage != nil {
		t.usage.InputTokens = chunk.Usage.PromptTokens
		t.usage.OutputTokens = chunk.Usage.CompletionTokens
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != "" {
			t.finishReason = choice.FinishReason
		}
		if choice.Delta.Content != "" {
			t.applyTextDelta(choice.Delta.Content, w)
		}
		for _, tc := range choice.Delta.ToolCalls {
			t.applyToolDelta(tc)
		}
	}
}

func (t *anthropicStreamTranslator) applyTextDelta(text string, w io.Writer) {
	t.textAccum.WriteString(text)
	if !t.streaming {
		return
	}
	if !t.textStarted {
		t.textStarted = true
		t.textBlockIndex = t.nextBlockIndex
		t.nextBlockIndex++
		writeSSEEvent(w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": t.textBlockIndex,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		})
	}
	writeSSEEvent(w, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": t.textBlockIndex,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (t *anthropicStreamTranslator) applyToolDelta(tc openaiToolCallDelta) {
	call, ok := t.toolCalls[tc.Index]
	if !ok {
		call = &anthropicToolCallState{}
		t.toolCalls[tc.Index] = call
		t.toolOrder = append(t.toolOrder, tc.Index)
	}
	if tc.ID != "" {
		call.id = tc.ID
	}
	if tc.Function.Name != "" {
		call.name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		call.argsBuf.WriteString(tc.Function.Arguments)
	}
}

// Finish emits the remaining streaming events (closing the text block if
// open, then each buffered tool_use block, then message_delta+message_stop).
// No-op in non-streaming mode — call BuildMessage instead.
func (t *anthropicStreamTranslator) Finish(w io.Writer) {
	if !t.streaming {
		return
	}
	if t.textStarted {
		writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": t.textBlockIndex})
	}
	for _, idx := range t.toolOrder {
		call := t.toolCalls[idx]
		blockIndex := t.nextBlockIndex
		t.nextBlockIndex++
		id := call.id
		if id == "" {
			id = "toolu_" + randHex(12)
		}
		parsed := parsedToolArgs(call.argsBuf.Bytes(), call.name)
		writeSSEEvent(w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  call.name,
				"input": map[string]any{},
			},
		})
		writeSSEEvent(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(parsed)},
		})
		writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
	}

	outputTokens := t.usage.OutputTokens
	if outputTokens == 0 {
		outputTokens = estimateTokens(t.textAccum.String())
	}
	writeSSEEvent(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": t.anthropicStopReason(), "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":                t.usage.InputTokens,
			"output_tokens":               outputTokens,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		},
	})
	writeSSEEvent(w, "message_stop", map[string]any{"type": "message_stop"})
}

// BuildMessage renders the accumulated state as one non-streaming Anthropic
// Message. Only meaningful in non-streaming mode (Feed still populated
// textAccum/toolCalls even though it wrote nothing to its io.Writer).
func (t *anthropicStreamTranslator) BuildMessage() map[string]any {
	var content []map[string]any
	if t.textAccum.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": t.textAccum.String()})
	}
	for _, idx := range t.toolOrder {
		call := t.toolCalls[idx]
		id := call.id
		if id == "" {
			id = "toolu_" + randHex(12)
		}
		var input any = map[string]any{}
		args := call.argsBuf.Bytes()
		if len(bytes.TrimSpace(args)) > 0 && json.Valid(args) {
			input = json.RawMessage(args)
		}
		content = append(content, map[string]any{"type": "tool_use", "id": id, "name": call.name, "input": input})
	}
	if content == nil {
		content = []map[string]any{}
	}

	outputTokens := t.usage.OutputTokens
	if outputTokens == 0 {
		outputTokens = estimateTokens(t.textAccum.String())
	}
	return map[string]any{
		"id":            "msg_" + randHex(24),
		"type":          "message",
		"role":          "assistant",
		"model":         t.requestedModel,
		"content":       content,
		"stop_reason":   t.anthropicStopReason(),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                t.usage.InputTokens,
			"output_tokens":               outputTokens,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		},
	}
}

func (t *anthropicStreamTranslator) anthropicStopReason() string {
	if len(t.toolOrder) > 0 {
		return "tool_use"
	}
	switch t.finishReason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// parsedToolArgs returns args as-is if it's valid (possibly empty) JSON, or
// "{}" otherwise — a backend that streamed malformed argument JSON must not
// be allowed to break the whole response; the SDK's partial-JSON accumulator
// would otherwise throw trying to parse it.
func parsedToolArgs(args []byte, toolName string) json.RawMessage {
	if len(bytes.TrimSpace(args)) == 0 {
		return json.RawMessage("{}")
	}
	if json.Valid(args) {
		return json.RawMessage(args)
	}
	slog.Warn("relay router: anthropic translate: tool call arguments were not valid JSON, substituting {}", "tool", toolName)
	return json.RawMessage("{}")
}

func writeSSEEvent(w io.Writer, event string, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("relay router: anthropic translate: failed to encode SSE event", "event", event, "error", err)
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
}

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

// estimateTokens is a crude bytes/4 heuristic, used only as a placeholder
// until a backend's real usage arrives (message_start's initial input_tokens,
// and output_tokens when a backend never sends stream_options usage).
func estimateTokens(s string) int64 {
	if len(s) == 0 {
		return 0
	}
	n := int64(len(s)) / 4
	if n < 1 {
		n = 1
	}
	return n
}

func estimateAnthropicInputTokens(body []byte) int64 {
	return estimateTokens(string(body))
}

func randHex(n int) string {
	b := make([]byte, n/2+1)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read failing is effectively unreachable on any real
		// platform; a static fallback keeps this a total function without
		// silently reusing a fixed id — a caller receiving a duplicate id
		// across concurrent requests would be a stranger bug than this one.
		for i := range b {
			b[i] = byte(i)
		}
	}
	return hex.EncodeToString(b)[:n]
}

// ---------------------------------------------------------------------------
// Backend error -> Anthropic error envelope mapping
// ---------------------------------------------------------------------------

type anthropicErrorMapped struct {
	status  int
	errType string
	message string
}

// mapAnthropicBackendError translates a local backend's HTTP failure into
// the Anthropic error envelope Claude Code's recovery logic keys on.
// Context-overflow normalization is the load-bearing case: Claude Code's
// auto-compact recognizes only specific wording ("prompt is too long",
// "context window", …) on the message, not the HTTP status alone — see the
// ADR for the measured backend messages this substring-matches against.
func mapAnthropicBackendError(status int, body []byte) anthropicErrorMapped {
	msg := extractBackendErrorMessage(body)
	if status == 400 && isContextOverflowMessage(msg) {
		return anthropicErrorMapped{400, "invalid_request_error", "prompt is too long: " + msg}
	}
	switch status {
	case 401:
		return anthropicErrorMapped{status, "authentication_error", msg}
	case 403:
		return anthropicErrorMapped{status, "permission_error", msg}
	case 404:
		return anthropicErrorMapped{status, "not_found_error", msg}
	case 429:
		return anthropicErrorMapped{status, "rate_limit_error", msg}
	case 400:
		return anthropicErrorMapped{status, "invalid_request_error", msg}
	case 502, 503, 504:
		return anthropicErrorMapped{status, "overloaded_error", msg}
	default:
		if status >= 500 {
			return anthropicErrorMapped{status, "api_error", msg}
		}
		return anthropicErrorMapped{status, "api_error", msg}
	}
}

func extractBackendErrorMessage(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "backend returned an error with no body"
	}
	return trimmed
}

func isContextOverflowMessage(msg string) bool {
	lower := strings.ToLower(msg)
	for _, substr := range []string{
		"context size", "context length", "context window",
		"exceeds the available context", "maximum context length", "too long",
	} {
		if strings.Contains(lower, substr) {
			return true
		}
	}
	return false
}
