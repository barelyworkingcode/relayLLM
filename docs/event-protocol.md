# relayLLM Event Protocol

Version: **1**

This document defines the wire format between relayLLM and its clients (Eve, relayTelegram, HTTP callers, future clients). All providers — Claude CLI, Ollama, OpenAI-compatible, llama.cpp — emit events in this single canonical shape. The shape is Anthropic's `stream-json` with small extensions documented below.

If you are adding a new provider, your job is to translate that provider's wire format into the events described here. If you are writing a new client, this document is your contract.

## Transport

All events are JSON. Two channels exist:

- **WebSocket** (`/ws`) — server → client streaming. Each event is a single JSON object.
- **HTTP** (`POST /api/sessions/:id/message`) — synchronous wrapper. The server accumulates the same event stream into a `{text, stats}` reply via `response_collector.go`.

The WebSocket channel is the canonical source. HTTP callers see a degraded view (text only).

## Top-level event envelope

Every event sent over the WebSocket has a top-level `type` field:

```json
{"type": "session_joined", "sessionId": "...", "protocolVersion": "1", ...}
{"type": "user_message",   "sessionId": "...", "text": "..."}
{"type": "llm_event",      "sessionId": "...", "event": { ... }}
{"type": "stats_update",   "sessionId": "...", "stats": { ... }}
{"type": "message_complete","sessionId": "..."}
{"type": "error",          "sessionId": "...", "message": "..."}
{"type": "process_exited", "sessionId": "..."}
```

Clients route on the top-level `type`. The interesting payload is the `event` field of `llm_event` — that's the canonical shape this document specifies.

`session_joined` carries `"protocolVersion": "1"`. Clients that don't recognize the version should refuse to render and surface an upgrade prompt.

## The canonical event shape

The `event` field of every `llm_event` is one of the following. The shape mirrors Anthropic's streaming events (`message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_stop`) but flattened slightly so each block-level event carries the block index inline.

### `system.init`

Emitted once per turn, before any assistant content. Carries the static context the client needs (model, tools, working directory).

```json
{
  "type": "system",
  "subtype": "init",
  "model": "claude-opus-4-7",
  "cwd": "/Users/jonathan/source/foo",
  "tools": ["Read", "Edit", "Bash", "generate_image"],
  "mcp_servers": ["relay", "github"]
}
```

`tools` and `mcp_servers` are best-effort. Empty arrays are valid.

### `assistant.message_start`

Marks the start of an assistant turn. One per turn.

```json
{
  "type": "assistant",
  "message": {
    "id": "msg_abc123",
    "role": "assistant",
    "content": []
  }
}
```

Clients reset their per-turn renderer state when they see this event.

### `assistant.content_block_start`

Marks the start of a single content block within the assistant message. Block types are `text`, `thinking`, and `tool_use`.

```json
{
  "type": "assistant",
  "index": 0,
  "content_block": {"type": "text"}
}

{
  "type": "assistant",
  "index": 1,
  "content_block": {"type": "thinking"}
}

{
  "type": "assistant",
  "index": 2,
  "content_block": {
    "type": "tool_use",
    "id": "toolu_01ABC",
    "name": "Read",
    "input": {}
  }
}
```

`index` is monotonically increasing within one assistant turn and is the key that ties `content_block_start` → `content_block_delta` → `content_block_stop` together.

For `tool_use` blocks:
- `id` is **always present** and unique within the session. For providers that don't supply one (Ollama native), relayLLM synthesizes `tool_<index>_<name>`.
- `input` is `{}` at start; the actual arguments arrive via `input_json_delta` events.

### `assistant.content_block_delta`

Streamed updates to an open content block. The `delta.type` discriminates.

```json
{"type": "assistant", "index": 0, "delta": {"type": "text_delta",       "text": "Hello, "}}
{"type": "assistant", "index": 1, "delta": {"type": "thinking_delta",   "thinking": "Let me think..."}}
{"type": "assistant", "index": 2, "delta": {"type": "input_json_delta", "partial_json": "{\"path\":"}}
```

`input_json_delta` chunks must be concatenated by the client (or the response collector) and parsed at `content_block_stop`. Clients that don't need streaming arguments may ignore these and read the final `input` from the matching tool result event.

### `assistant.content_block_stop`

Marks the end of one content block. Carries the resolved final content for `tool_use` blocks (so clients that ignored `input_json_delta` can still render the call).

```json
{"type": "assistant", "index": 0, "content_block_stop": true}

{
  "type": "assistant",
  "index": 2,
  "content_block_stop": true,
  "content_block": {
    "type": "tool_use",
    "id": "toolu_01ABC",
    "name": "Read",
    "input": {"path": "/etc/hosts"}
  }
}
```

The tail `content_block` echo is **only** present for `tool_use` blocks. Text and thinking blocks emit `{content_block_stop: true}` alone.

### `result.tool_result`

Emitted after a tool call returns. Pairs to its `tool_use` block by `tool_use_id`.

```json
{
  "type": "result",
  "subtype": "tool_result",
  "tool_use_id": "toolu_01ABC",
  "tool_name": "Read",
  "content": "file contents...",
  "is_error": false
}
```

`tool_name` is provided as a fallback for clients that pair by ordering (Eve currently does this); new clients should pair by `tool_use_id`.

### `result.tool_progress`

Optional progress events emitted by long-running tools (e.g. image generation).

```json
{
  "type": "result",
  "subtype": "tool_progress",
  "tool_use_id": "toolu_01ABC",
  "tool_name": "generate_image",
  "message": "Generating image..."
}
```

### `result.error`

Streamed error from a tool or the model itself. Distinct from the top-level `error` envelope, which signals a session-level failure.

```json
{
  "type": "result",
  "subtype": "error",
  "error": "model returned 503"
}
```

## Stream lifecycle

A typical assistant turn:

```
session_joined (once at WS open)

user_message (sender's input)

llm_event: system.init
llm_event: assistant.message_start
llm_event: assistant.content_block_start  (index 0, text)
llm_event: assistant.content_block_delta  (index 0, text_delta) ×N
llm_event: assistant.content_block_stop   (index 0)
llm_event: assistant.content_block_start  (index 1, tool_use, id=toolu_X)
llm_event: assistant.content_block_delta  (index 1, input_json_delta) ×M
llm_event: assistant.content_block_stop   (index 1, with final content_block)
llm_event: result.tool_result             (tool_use_id=toolu_X)
llm_event: assistant.content_block_start  (index 2, text)
llm_event: assistant.content_block_delta  (index 2, text_delta) ×N
llm_event: assistant.content_block_stop   (index 2)
stats_update
message_complete
```

Invariants:
1. Every `content_block_start` is matched by exactly one `content_block_stop` with the same `index`.
2. `index` values are monotonically increasing within a single turn and reset to 0 on the next `message_start`.
3. `result.tool_result` appears after the `content_block_stop` of its corresponding `tool_use` block, before the next assistant `content_block_start`.
4. `stats_update` may arrive at any time after `message_start`; clients should treat the latest value as authoritative.
5. `message_complete` is terminal for the turn. Anything that arrives after it should be discarded by the client until the next `message_start` (the session layer guards against this server-side via the generation counter, but clients should be defensive).

## Provider-specific notes

These are not part of the contract — they document how each provider maps to it.

### Claude CLI

The Claude CLI emits stream-json that is the historical inspiration for this contract — most events match exactly. The Claude provider currently passes lines through unchanged. Two known differences:

- **Combined start+content for text.** Claude CLI may emit `{type:"assistant", content_block:{type:"text", text:"..."}}` as a single event (block starts and carries some content in one go), rather than separate start (no text) + delta. Clients should accept both: when `content_block` has a `.text` field, treat it as initial content; when it doesn't, treat it as a bare start.
- **Tool argument streaming via `tool_use_input` blocks.** Claude CLI streams tool arguments as `{content_block:{type:"tool_use_input", input:string|object}}` events instead of `delta.type:"input_json_delta"`. Clients should accept both shapes; new providers should emit only `input_json_delta`.

These are the only legacy variations. All other event types match exactly.

### Ollama (`/api/chat`)

- Thinking arrives on `chunk.message.thinking` → emitted as a `thinking` block.
- Text arrives on `chunk.message.content` → emitted as a `text` block.
- Tool calls arrive whole on `chunk.message.tool_calls[]` (not streamed) → emitted as `tool_use` block with a single `input_json_delta` containing the full JSON, then `content_block_stop`.
- Ollama doesn't track tool-call IDs, so relayLLM synthesizes `tool_<index>_<name>`.

### OpenAI-compatible (`/v1/chat/completions`)

- Reasoning text arrives on `delta.reasoning_content` or `delta.reasoning` (server-dependent) → emitted as a `thinking` block.
- Text arrives on `delta.content` → emitted as a `text` block.
- Tool calls arrive incrementally on `delta.tool_calls[i].function.arguments` as partial JSON strings → forwarded as `input_json_delta` chunks. The `tool_use` block's `id` is the server-supplied ID.
- Some servers emit usage in the final chunk only (`stream_options.include_usage`); relayLLM synthesizes TTFT/TPS from wall clock.

### llama.cpp (managed via `LlamaServerManager`)

Identical to OpenAI-compatible — the transport is shared.

## Versioning

Breaking changes bump the major version (`"1"` → `"2"`). Adding new event types or new fields to existing events is non-breaking. Clients should ignore unknown event types and unknown fields silently.

When the server bumps a major, older clients on `protocolVersion: "1"` will continue to receive the v1 shape behind a feature flag for one release cycle, then be cut.
