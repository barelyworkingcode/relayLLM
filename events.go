package main

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the wire-format version exposed in session_joined.
// Bump on breaking changes. See docs/event-protocol.md.
const ProtocolVersion = "1"

// ContentBlockType enumerates the block types a canonical assistant turn can
// contain. These match the strings in the wire format.
const (
	BlockText     = "text"
	BlockThinking = "thinking"
	BlockToolUse  = "tool_use"
)

// EventEmitter is a typed wrapper around an EventHandler that emits canonical
// llm_event payloads as defined in docs/event-protocol.md. Providers should
// construct one of these and call its methods rather than building JSON
// inline — the constructors are the single point that enforces the contract.
type EventEmitter struct {
	handler EventHandler
}

func NewEventEmitter(handler EventHandler) *EventEmitter {
	return &EventEmitter{handler: handler}
}

// Raw forwards an opaque event payload. Used by the Claude provider while it
// passes through stream-json lines that are already canonical. New code should
// prefer the typed methods below.
func (e *EventEmitter) Raw(data json.RawMessage) {
	if e == nil || e.handler == nil {
		return
	}
	e.handler("llm_event", data)
}

// SystemInit emits the system.init event that announces a turn's static
// context (model, tools, mcp servers, working directory). Best-effort —
// callers may pass empty strings/slices for fields they don't have.
func (e *EventEmitter) SystemInit(model, cwd string, tools, mcpServers []string) {
	if tools == nil {
		tools = []string{}
	}
	if mcpServers == nil {
		mcpServers = []string{}
	}
	e.emit(map[string]any{
		"type":        "system",
		"subtype":     "init",
		"model":       model,
		"cwd":         cwd,
		"tools":       tools,
		"mcp_servers": mcpServers,
	})
}

// MessageStart emits assistant.message_start. id may be empty for providers
// that don't track per-turn message ids.
func (e *EventEmitter) MessageStart(id string) {
	e.emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"id":      id,
			"role":    "assistant",
			"content": []any{},
		},
	})
}

// TextBlockStart emits content_block_start for a text block at the given index.
func (e *EventEmitter) TextBlockStart(index int) {
	e.emit(map[string]any{
		"type":          "assistant",
		"index":         index,
		"content_block": map[string]any{"type": BlockText},
	})
}

// ThinkingBlockStart emits content_block_start for a thinking block.
func (e *EventEmitter) ThinkingBlockStart(index int) {
	e.emit(map[string]any{
		"type":          "assistant",
		"index":         index,
		"content_block": map[string]any{"type": BlockThinking},
	})
}

// ToolUseBlockStart emits content_block_start for a tool_use block. id must
// be unique within the session — providers without native ids should call
// SynthesizeToolUseID(index, name) to produce one.
func (e *EventEmitter) ToolUseBlockStart(index int, id, name string) {
	e.emit(map[string]any{
		"type":  "assistant",
		"index": index,
		"content_block": map[string]any{
			"type":  BlockToolUse,
			"id":    id,
			"name":  name,
			"input": map[string]any{},
		},
	})
}

// TextDelta emits a text_delta for the open text block at index.
func (e *EventEmitter) TextDelta(index int, text string) {
	e.emit(map[string]any{
		"type":  "assistant",
		"index": index,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	})
}

// ThinkingDelta emits a thinking_delta for the open thinking block at index.
func (e *EventEmitter) ThinkingDelta(index int, text string) {
	e.emit(map[string]any{
		"type":  "assistant",
		"index": index,
		"delta": map[string]any{
			"type":     "thinking_delta",
			"thinking": text,
		},
	})
}

// InputJsonDelta emits an input_json_delta for the open tool_use block at
// index. partial is a fragment of the JSON-encoded arguments as it streams in.
func (e *EventEmitter) InputJsonDelta(index int, partial string) {
	e.emit(map[string]any{
		"type":  "assistant",
		"index": index,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": partial,
		},
	})
}

// BlockStop emits content_block_stop for a non-tool block (text, thinking).
func (e *EventEmitter) BlockStop(index int) {
	e.emit(map[string]any{
		"type":               "assistant",
		"index":              index,
		"content_block_stop": true,
	})
}

// ToolUseBlockStop emits content_block_stop for a tool_use block, echoing the
// resolved final input so clients that ignored the streaming deltas still get
// the full call.
func (e *EventEmitter) ToolUseBlockStop(index int, id, name string, input json.RawMessage) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	e.emit(map[string]any{
		"type":               "assistant",
		"index":              index,
		"content_block_stop": true,
		"content_block": map[string]any{
			"type":  BlockToolUse,
			"id":    id,
			"name":  name,
			"input": input,
		},
	})
}

// ToolResult emits result.tool_result. toolUseID pairs the result back to its
// tool_use block; toolName is provided as a fallback for clients that pair by
// ordering. isError signals tool failure; content carries the (possibly
// truncated) tool output.
func (e *EventEmitter) ToolResult(toolUseID, toolName, content string, isError bool) {
	e.emit(map[string]any{
		"type":        "result",
		"subtype":     "tool_result",
		"tool_use_id": toolUseID,
		"tool_name":   toolName,
		"content":     content,
		"is_error":    isError,
	})
}

// ToolProgress emits result.tool_progress for long-running tool calls.
func (e *EventEmitter) ToolProgress(toolUseID, toolName, message string) {
	e.emit(map[string]any{
		"type":        "result",
		"subtype":     "tool_progress",
		"tool_use_id": toolUseID,
		"tool_name":   toolName,
		"message":     message,
	})
}

// ResultError emits result.error — distinct from the top-level error envelope,
// which is for session-level failures.
func (e *EventEmitter) ResultError(msg string) {
	e.emit(map[string]any{
		"type":    "result",
		"subtype": "error",
		"error":   msg,
	})
}

// emit marshals the canonical event and forwards it as an llm_event.
func (e *EventEmitter) emit(payload map[string]any) {
	if e == nil || e.handler == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		// Marshal failures here mean a programming bug — surface loudly.
		panic(fmt.Sprintf("event emitter: marshal failed: %v", err))
	}
	e.handler("llm_event", data)
}

// SynthesizeToolUseID produces a deterministic id for tool_use blocks emitted
// by providers that don't track ids natively (Ollama). Format: tool_<index>_<name>.
// The shape mirrors what OpenAI/Claude produce closely enough that downstream
// clients don't need a special case.
func SynthesizeToolUseID(index int, name string) string {
	return fmt.Sprintf("tool_%d_%s", index, name)
}
