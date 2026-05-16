# relayLLM Event Protocol

Version: **2**

This document defines the wire format between relayLLM and its clients (Eve, relayTelegram, HTTP callers, future clients). All five providers — Claude CLI, pi.dev CLI, Ollama, OpenAI-compatible, llama.cpp — emit events in this single canonical shape via translators. The shape is owned by relayLLM: no provider's wire format leaks through.

If you are adding a new provider, your job is to translate that provider's wire format into the events described here. If you are writing a new client, this document is your contract.

## Transport

All events are JSON. Two channels exist:

- **WebSocket** (`/ws`) — server → client streaming. Each event is a single JSON object.
- **HTTP** (`POST /api/sessions/:id/message`) — synchronous wrapper. The server accumulates the same event stream into a `{text, stats}` reply via `response_collector.go`. Only text deltas are surfaced — thinking and tool blocks are deliberately excluded.

The WebSocket channel is the canonical source.

## Top-level event envelope

Every event sent over the WebSocket has a top-level `type` field:

```json
{"type": "session_joined",  "sessionId": "...", "protocolVersion": "2", ...}
{"type": "user_message",    "sessionId": "...", "text": "..."}
{"type": "llm_event",       "sessionId": "...", "event": { ... }}
{"type": "stats_update",    "sessionId": "...", "stats": { ... }}
{"type": "message_complete","sessionId": "..."}
{"type": "error",           "sessionId": "...", "message": "..."}
{"type": "process_exited",  "sessionId": "..."}
```

Clients route on the top-level `type`. The interesting payload is the `event` field of `llm_event` — that's the canonical shape this document specifies.

`session_joined` carries `"protocolVersion": "2"`. Clients on an unknown major must refuse to render and surface an upgrade prompt.

## Per-event versioning

Every payload inside `llm_event.event` carries a `"v": 2` field at the top level. Clients should gate on this field per-event, in addition to the session-join check, so a misrouted-from-the-future event doesn't render against an incompatible parser.

## The canonical event shape

The `event` field of every `llm_event` is one of the shapes below. All of them are typed in Go (`events.go`) — the wire shape is what `json.Marshal` produces from those structs. JSON tag changes are wire-breaking changes.

The three top-level discriminators are `system`, `assistant`, and `result`.

---

### `system.init`

Emitted once per turn, before any assistant content. Carries the static context the client needs (model, tools, working directory).

```json
{
  "v": 2,
  "type": "system",
  "subtype": "init",
  "model": "claude-opus-4-7",
  "cwd": "/Users/jonathan/source/foo",
  "tools": ["Read", "Edit", "Bash", "generate_image"],
  "mcp_servers": ["relay", "github"]
}
```

`tools` and `mcp_servers` are best-effort. Empty arrays are valid.

---

### `system.permission_request`

Inline permission prompt for a tool call. Claude-only today; other providers don't support permissions (see `provider_capabilities.go`). The provider holds generation until a response arrives via the `/api/permission` endpoint.

```json
{
  "v": 2,
  "type": "system",
  "subtype": "permission_request",
  "permission_id": "perm_01ABC",
  "tool_name": "Bash",
  "tool_use_id": "toolu_01XYZ",
  "tool_input": {"command": "rm -rf /"}
}
```

`tool_use_id` is optional — some permission requests are not tied to a streamed tool_use block.

---

### `system.question`

Inline free-form input prompt. Clients display a modal and forward the response.

```json
{
  "v": 2,
  "type": "system",
  "subtype": "question",
  "prompt": "What database connection string should I use?",
  "metadata": {"...optional..."}
}
```

---

### `system.status`

Updates the thinking-indicator text mid-turn. Clients should replace the previous status message rather than append.

```json
{
  "v": 2,
  "type": "system",
  "subtype": "status",
  "message": "Searching the codebase..."
}
```

---

### `system.api_error`

Retryable upstream API error context. Distinct from `result.error` (per-tool) and the top-level `error` envelope (session-level).

```json
{
  "v": 2,
  "type": "system",
  "subtype": "api_error",
  "message": "Upstream API returned 503",
  "retrying": true
}
```

---

### `system.bridge_status`

State updates from an external bridge (currently the claude.ai remote-control bridge).

```json
{
  "v": 2,
  "type": "system",
  "subtype": "bridge_status",
  "status": "connected",
  "detail": "..."
}
```

---

### `system.stop_hook_summary`

Result of a stop-hook execution.

```json
{
  "v": 2,
  "type": "system",
  "subtype": "stop_hook_summary",
  "summary": "...",
  "is_error": false
}
```

---

### `assistant.message_start`

Marks the start of an assistant turn. One per turn. Clients reset their per-turn renderer state when they see this event.

```json
{
  "v": 2,
  "type": "assistant",
  "message": {
    "id": "msg_abc123",
    "role": "assistant",
    "content": []
  }
}
```

---

### `assistant.content_block_start`

Marks the start of a single content block within the assistant message. Block types are `text`, `thinking`, and `tool_use`. **No other block types exist** in v2 — Claude CLI's legacy `tool_use_input` is normalized away by the Claude translator.

```json
{
  "v": 2,
  "type": "assistant",
  "index": 0,
  "content_block": {"type": "text"}
}

{
  "v": 2,
  "type": "assistant",
  "index": 1,
  "content_block": {"type": "thinking"}
}

{
  "v": 2,
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
- `id` is **always present and unique** within the session. For providers that don't supply one (Ollama native), relayLLM synthesizes `tool_<index>_<name>`.
- `input` is `{}` at start; the actual arguments arrive via `input_json_delta` events.

For `text` and `thinking` blocks: **no inline content on start.** Content arrives via deltas only. Claude CLI's combined start+content quirk is normalized at the translator boundary.

---

### `assistant.content_block_delta`

Streamed updates to an open content block. The `delta.type` discriminates.

```json
{"v": 2, "type": "assistant", "index": 0, "delta": {"type": "text_delta",       "text": "Hello, "}}
{"v": 2, "type": "assistant", "index": 1, "delta": {"type": "thinking_delta",   "thinking": "Let me think..."}}
{"v": 2, "type": "assistant", "index": 2, "delta": {"type": "input_json_delta", "partial_json": "{\"path\":"}}
```

`input_json_delta` chunks must be concatenated by the client (or the response collector) and parsed at `content_block_stop`. Clients that don't need streaming arguments may ignore these and read the final `input` from the matching `content_block_stop` event.

---

### `assistant.content_block_stop`

Marks the end of one content block. Carries the resolved final content for `tool_use` blocks (so clients that ignored `input_json_delta` can still render the call).

```json
{"v": 2, "type": "assistant", "index": 0, "content_block_stop": true}

{
  "v": 2,
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

---

### `result.tool_result`

Emitted after a tool call returns. Pairs to its `tool_use` block by `tool_use_id`.

```json
{
  "v": 2,
  "type": "result",
  "subtype": "tool_result",
  "tool_use_id": "toolu_01ABC",
  "tool_name": "Read",
  "content": "file contents...",
  "is_error": false
}
```

**Clients must pair by `tool_use_id` only.** `tool_name` is metadata (UI labelling); ordering-based pairing is not part of the contract.

---

### `result.tool_progress`

Optional progress events emitted by long-running tools (e.g. image generation).

```json
{
  "v": 2,
  "type": "result",
  "subtype": "tool_progress",
  "tool_use_id": "toolu_01ABC",
  "tool_name": "generate_image",
  "message": "Generating image..."
}
```

---

### `result.error`

Streamed error from a tool or the model itself. Distinct from `system.api_error` (recoverable upstream context) and the top-level `error` envelope (session-level failure).

```json
{
  "v": 2,
  "type": "result",
  "subtype": "error",
  "error": "model returned 503"
}
```

---

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
5. `message_complete` is terminal for the turn. Anything that arrives after it should be discarded by the client until the next `message_start`.
6. **All providers emit `message_complete` with no payload (top-level `data: null`).** Providers persist assistant turns to `session.Messages` themselves (chat-base via `turnStreamState.blocks`, pi via its block accumulator, Claude via its CLI's JSONL replayed on join). There is no fallback "save as text-only" path.

## Adding a new provider

Write a translator function in your provider file that consumes the upstream wire format and emits canonical events via an `*EventEmitter` constructed at provider start. The state machine pattern from `provider_pi.go::translate` (lines 477–654) is the working reference:

1. Reset per-turn block state on the upstream's turn-start signal.
2. Call `emitter.SystemInit(model, cwd, tools, mcpServers)`, then `emitter.MessageStart("")`.
3. For each text/thinking/tool_use block: emit `*BlockStart`, then deltas, then `BlockStop`/`ToolUseBlockStop`. Auto-close any open block when a new block of a different kind starts.
4. Map sparse upstream block indices to monotonic relay-canonical indices.
5. Accumulate blocks locally for `session.Messages` persistence.
6. Emit `ToolResult` after each tool execution, paired by `tool_use_id`.
7. On turn end: emit `stats_update` (via the session handler), then `message_complete` with `nil` data.

Avoid building JSON inline. The `EventEmitter` methods are the single point that enforces the wire contract.

## Versioning

Breaking changes bump the major version (`"2"` → `"3"`). Adding new event types or new fields to existing events is non-breaking. Clients should ignore unknown event types and unknown fields silently — but they should still gate on the major version (`v` field) per-event so a misrouted-from-the-future payload isn't fed to an incompatible parser.

Under "no legacy support" as of v2: there is no compatibility shim period. A bump from v2 to v3 is a hard break.
