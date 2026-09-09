# ADR-013: Anthropic Messages API compatibility on the relay-router

**Status:** Accepted
**Date:** 2026-09-09

## Context

Claude Code (the `claude` CLI) only ever speaks Anthropic's Messages API
(`/v1/messages`) and only ever takes a single `ANTHROPIC_BASE_URL` — there is
no per-request provider selection the way relayLLM's own clients get one.
Pointing it at relayLLM instead of `api.anthropic.com` is attractive for two
reasons: it lets relayLLM's own local/managed models (llama.cpp, mlx-serve,
OpenAI endpoints) show up as models Claude Code can select, and it opens the
door to redirecting a specific model id (e.g. a Haiku-class id) to a local
model transparently, while every other request still reaches the real
Anthropic API unmodified.

relayLLM's router (`relay_router.go`) speaks OpenAI's Chat Completions
dialect exclusively — every managed server, virtual model, and configured
endpoint is dispatched through that one wire shape. Anthropic's Messages API
differs in ways that don't map 1:1: `system` is a top-level field, not a
message; content is typed blocks (`text`, `image`, `tool_use`,
`tool_result`, …) rather than a string-or-parts union; the streaming
protocol is a materially different event sequence
(`message_start`/`content_block_start`/`content_block_delta`/…) instead of
OpenAI's flat `choices[].delta` chunks; and Claude Code's actual traffic
under a custom base URL is wider than `/v1/messages` alone.

Two research passes shaped this design:

- Reading the installed `claude` CLI binary and Anthropic's own gateway docs
  (`code.claude.com/docs/en/llm-gateway-protocol.md`,
  `llm-gateway-connect.md`, `model-config.md`) established that **Claude Code
  never calls a model-discovery endpoint at all when the only credential is
  a claude.ai subscription OAuth login** (`CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY`
  is off by default, and even when set, discovery is skipped outright for
  OAuth-only sessions per the binary's own `[gatewayDiscovery] skipped: no
  credential` string). It also established that **any model string is
  accepted without validation** under a non-first-party `ANTHROPIC_BASE_URL`,
  and that OAuth-mode Claude Code hits more than `/v1/messages` under a
  custom base URL — notably `GET /api/claude_cli/bootstrap` and
  `HEAD /api/hello` at startup, both carrying the real OAuth bearer, both
  needed for Claude Code's own UI (model picker, entitlements) to behave
  normally.
- Reading this repo's existing patterns: `RouterConfig.ReasoningEffortMap`
  (relay_router.go) is the precedent for a config-driven, off-by-default,
  free-form-string body rewrite; ADR-012's `newEndpointTransport` is the
  precedent for a pinned outbound `http.Transport`; the router's existing
  dispatch order (managed alias → virtual → endpoint, `handleProxy`) is what
  a redirected request should re-enter rather than duplicate.

## Decision

### Three routes on the existing `--router-port` listener

`POST /v1/messages`, `POST /v1/messages/count_tokens`, and a catch-all
`/api/` prefix are registered unconditionally in `NewRelayRouter`'s mux
(`relay_router_anthropic.go`), sitting above the existing `/` →
`handleProxy` catch-all. All three 404 with an Anthropic error envelope at
request time when `RelayRouter.anthropic` is nil — the feature's off switch
is `settings.json`'s `router.anthropic` section being absent, exactly
mirroring `ReasoningEffortMap`'s "absent map, zero behavior change" shape.
One listener, one TLS/bind story, rather than a second port with its own
surface to document and secure.

An Anthropic-shaped `/v1/models` was deliberately **not built**. Given the
discovery finding above, it would be read by nobody in the target setup
(OAuth login) and would only matter for a different auth mode
(`ANTHROPIC_AUTH_TOKEN`/API key) this deployment doesn't use. Instead,
`router.anthropic.modelMap` keys are added as rows to the *existing*
OpenAI-shaped `/v1/models` (`handleModels`), and visibility inside Claude
Code's own `/model` picker is a client-side setting
(`ANTHROPIC_CUSTOM_MODEL_OPTION=<key>`, or a `modelPicker` entry in Claude
Code's own settings) — see CLAUDE.md's Relay-router section for the exact
value to set.

### Passthrough: byte-for-byte, not just `/v1/messages`

`newAnthropicPassthroughProxy` builds an `httputil.ReverseProxy` using the
`Rewrite` hook (not the older `Director` hook `newUpstreamProxy` uses
elsewhere in this router) — `Rewrite` injects no `X-Forwarded-*` headers,
which matters here because this hop must be invisible: Claude Code's OAuth
bearer, `anthropic-version`, and `anthropic-beta` (which carries
`oauth-2025-04-20` for a subscription login — stripping it 401s every
request) all have to reach `api.anthropic.com` exactly as sent. This is why
passthrough is never built on `newUpstreamProxy`: that function's `Director`
deliberately replaces/deletes `Authorization` before forwarding, which is
correct for every other route in this file (an inbound bearer is relayLLM's
own internal token, meaningless upstream) and exactly wrong here (the
inbound credential *is* the upstream credential). The proxy's transport
clones `http.DefaultTransport` with `DisableCompression: true` — otherwise
Go silently injects `Accept-Encoding: gzip` and transparently decompresses,
which is semantically harmless but not the byte-for-byte guarantee this
feature promises. `FlushInterval: -1` keeps SSE streaming responsive.
Non-2xx responses (a 401 on token expiry, a 413, whatever) are relayed with
their status and body untouched — Claude Code's own retry/refresh and
context-recovery logic key on exact wording, not just the status code.

The `/api/` prefix is unbuffered and un-inspected — the body, if any, streams
straight through. `/v1/messages` and `/v1/messages/count_tokens` must
buffer (bounded at 64 MiB) to read the top-level `model` field before
deciding passthrough vs. redirect; the buffered bytes are re-installed onto
the request unchanged before the proxy runs, so a passthrough request is
still byte-identical to what the client sent.

### Redirect: translate, then re-enter the router's own dispatch

`router.anthropic.modelMap` is an exact-match, free-form string map — same
convention as `ReasoningEffortMap` — from a client-facing model id to a
router-dispatchable target (a managed alias, a virtual model name, or an
`endpoint/model` id). A match is resolved **once, at the very top of
`handleProxy`** (relay_router.go), rewriting the request's `model` field in
place before the managed/virtual/endpoint checks that already exist — this
makes a modelMap key a router-wide alias, reachable from a plain OpenAI
client too, for free, rather than requiring a second dispatcher that
duplicates those three checks. `warnAnthropicModelMap` (main.go) flags at
startup when a key collides with an existing managed alias or
endpoint-prefixed id, since this ordering means such a key would silently
shadow it on every route.

`handleAnthropicRedirect` (relay_router_anthropic.go) translates the
Anthropic body to OpenAI (`anthropicToOpenAIRequest`, anthropic_translate.go
— pure, no I/O, mirroring `provider_pi.go`'s translate seam), builds a
**fresh** `*http.Request` carrying only a `Content-Type` header — never a
copy of the inbound request's headers — and calls `p.handleProxy` on it
directly. This is deliberate on two counts: it gets managed-server
leases/memory-budget, virtual-model failover/affinity, and the
`reasoningEffortMap`/`TemplateKwargs` rewrites for free, since it's the same
code path an OpenAI client's request would take; and it guarantees the
client's real Anthropic credential (OAuth bearer or API key) can never reach
a local backend, because nothing carries it into the synthesized request in
the first place — verified by a test asserting the fake local backend sees
no `Authorization`/`anthropic-*` headers at all.

**Every system-content source collapses into one leading message.** Anthropic
has two ways to supply system content — the top-level `system` field, and a
mid-conversation `role: "system"` message (a beta feature Claude Code's own
system-reminder mechanism uses in real sessions). Both are folded into a
single `{role: "system"}` message at index 0 of the translated request,
rather than each emitted at its own position. This was found live, not
designed in advance: a real llama.cpp backend (Qwen's Jinja chat template)
500s with `System message must be at the beginning` the instant a second
`role: "system"` message appears anywhere past index 0, and Claude Code's
own `-p` sessions routinely include one. Anthropic supports multiple/
mid-conversation system messages; most OpenAI-facing chat templates assume
exactly one, always first — merging is the difference between "works" and
every redirected session with a mid-conversation reminder breaking outright.

### The translating response writer

`p.handleProxy` writes into a `translatingResponseWriter`
(relay_router_anthropic.go), not directly into the client's real
`http.ResponseWriter`. It defers ever touching the real writer until the
backend's status is known: a 200 begins the Anthropic-shaped response
(SSE or, for a non-streaming client request, nothing yet); anything else is
buffered and remapped to an Anthropic error envelope in `finish()`
(`mapAnthropicBackendError`, anthropic_translate.go — including the
context-overflow wording normalization Claude Code's auto-compact keys on:
a backend's own wording ("exceeds the available context size", "maximum
context length is N") is substring-matched and re-emitted as `400 "prompt is
too long: <original>"`, the phrase Claude Code actually recognizes). This is
what turns a managed-server `Acquire` failure, a dead endpoint, or a bad
translation into a proper Anthropic error instead of a half-written stream.

The upstream request is **always** made with `stream: true` regardless of
what the client asked for — `anthropicStreamTranslator` (anthropic_translate.go)
accumulates state identically either way, and only the *rendering* differs:
streaming mode writes Anthropic SSE events live as OpenAI deltas arrive
(`Feed`) plus a final `Finish`; non-streaming mode discards Feed's output
argument and renders one JSON `Message` from the same accumulated state at
the end (`BuildMessage`). One state machine, two renderings, rather than two
parallel implementations that could drift.

Text streams live; tool calls are buffered per index and emitted as complete
blocks (`content_block_start` + one `input_json_delta` carrying the full
arguments + `content_block_stop`) only at `Finish`. This is deliberate, not
a shortcut: OpenAI permits interleaved argument fragments across tool-call
indices mid-stream, but Anthropic content blocks are strictly sequential —
and nothing can execute a tool before `message_stop` reaches the client
anyway, so buffering costs no real latency while removing a whole class of
interleaving bugs.

A ping ticker (`router.anthropic.pingIntervalSeconds`, default 15) starts
the moment the backend's response headers arrive and stops when the
exchange finishes. This covers the gap Claude Code's own streaming-byte
watchdog (300s idle) would otherwise trip on: real llama.cpp/oMLX backends
send response headers before prompt processing finishes, so a long prompt on
local hardware can go silent for minutes between the connection opening and
the first token. It does **not** cover the gap before those headers arrive —
that window (model cold-launch, admission-queue wait) is bounded by Claude
Code's own request timeout (`API_TIMEOUT_MS`, 10 minutes by default, user
override available), not by anything this router can inject bytes into,
since nothing has reached the client yet at that point.

### `translatingResponseWriter`'s `Flush()` must gate on streaming mode

One real bug surfaced writing the tests for this ADR, worth recording since
it is easy to reintroduce: `httputil.ReverseProxy`'s body-copy loop calls
`dst.(http.Flusher).Flush()` after *every* write, independent of whether
`Write` itself did anything — the reverse proxy has `FlushInterval: -1`, and
that is unconditional. In non-streaming mode, `Write` correctly buffers
everything into the translator's internal state without touching the real
`http.ResponseWriter` (so `finish()` can decide the client's real status
after the fact). But an unconditional `Flush()` forwarding to the real
writer calls the standard library's own `Flush()` on a response that has
never had `WriteHeader` called — which implicitly sends a **200** right
then, before `finish()` ever gets to decide the real status. `Flush()` must
gate on `wantStream` exactly like `Write()` already does; it cannot rely on
`Write()`'s guard alone because it is invoked independently.

## Consequences

**Explicitly out of scope** (documented here as deliberate non-goals, not
oversights):

- **Extended thinking.** Anthropic `thinking` content blocks carry a
  `signature` Claude Code replays verbatim on the next turn; there is no way
  to produce one from an OpenAI-shaped backend's reasoning output.
  `thinking`/`redacted_thinking` blocks are always dropped from translated
  history, and `thinking: {type: disabled}` maps to `reasoning_effort:
  "none"` — but a redirected model's reasoning is never streamed back as an
  Anthropic `thinking` block.
- **Prompt caching.** `cache_control` is dropped everywhere it appears (system
  blocks, message content, tool definitions); usage always reports zero
  cached tokens. Local backends still do their own prefix caching invisibly
  (llama.cpp KV reuse) — just not surfaced as Anthropic cache usage.
- **Documents/PDFs.** A `document` content block becomes a text placeholder;
  no backend here can be handed a PDF.
- **Structured outputs** (`output_config.format` / `response_format`) —
  not translated.
- **Server-side Anthropic tools** — any `tools[]` entry whose `type` is
  anything other than absent/`"custom"` (web search, bash, code execution,
  the MCP connector, computer use, memory) is refused with a `400
  invalid_request_error` naming the type, rather than silently dropped or
  passed through broken. Claude Code issues these on tool-specific request
  shapes distinct from a normal chat turn, so a redirected coding session is
  unaffected in practice.
- **An Anthropic-shaped `/v1/models`** — see Decision above; not needed for
  the OAuth-login case this was built for, and would need re-evaluating if
  API-key/`ANTHROPIC_AUTH_TOKEN` clients with gateway discovery enabled ever
  become a target.

**Known sharp edges:**

- A model id hijacking a real Claude id (e.g. mapping `claude-haiku-4-5` to
  a local model) makes Claude Code assume a Claude-sized context window for
  it; a smaller local model will overflow sooner than Claude Code expects.
  The context-overflow wording normalization above turns that into an
  auto-compact rather than a dead session, but choosing a modelMap key that
  does **not** start with `claude-` (e.g. `relay/coder`) avoids the mismatch
  entirely, at the cost of needing `ANTHROPIC_CUSTOM_MODEL_OPTION` to make it
  selectable.
- A mid-stream backend death (after headers, before `message_stop`) is not
  retried and is not guaranteed to surface as a clean Anthropic `error`
  event — `httputil.ReverseProxy` gives no hook for a body-copy failure once
  headers are sent. The stream simply ends with whatever partial state
  `Finish` can render from what arrived.
- The router's own listener is unauthenticated by design (relay handles
  auth in front of it in the enhanced deployment); enabling `router.anthropic` on
  a non-loopback `--router-bind` with no `--router-tls-cert` configured
  fails relayLLM startup outright (main.go) — passthrough forwards the
  client's real Anthropic credential on every request, so an open,
  unencrypted listener would leak it.
