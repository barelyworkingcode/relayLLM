# relayLLM (Go)

Standalone LLM engine service. Manages providers (Claude CLI, pi.dev CLI, Ollama HTTP, OpenAI-compatible HTTP, llama.cpp + MLX managed processes), sessions, projects, permissions, and terminal sessions (PTY). Runs independently or as a relay-enhanced service.

Under relay, relayLLM registers a [service manifest](../relay/docs/service-manifest.md) describing the routes it serves; relay's front-door dispatcher forwards matching traffic. relayLLM does not know about projects, tasks, or any sibling service — it stays focused on session/provider execution.

## For open-ended / lead-developer requests

When the user gives an open-ended ask (e.g. "what's next", "act as lead developer", "improve this codebase") — not a specific task — start by reading:

1. [`ROADMAP.md`](ROADMAP.md) — prioritized backlog, survives session compaction.
2. [`docs/decisions/`](docs/decisions/) — the *why* behind current architecture (manifest protocol, three-tier testing, test seams, no-carveouts rule).
3. [Releases & consumers](#releases--consumers) below — release model, who depends on relayLLM, "done" definition, breaking-change protocol.

Propose a plan before executing. For specific tasks ("fix this bug", "add X"), do the task — don't read ROADMAP first.

## Architecture

```
main.go                   Entry point, flag parsing, server wiring, manifest registration
manifest.go               Service manifest declaration + maybeRegisterManifest
relay_bridge_client.go    Bridge socket transport (sendBridgeRequest helper) +
                          PtyEnv resolution. RELAY_BRIDGE_SOCKET env var detection.
api_status.go             GET /api/status, GET+DELETE /api/llama/instances[/{alias}],
                          GET+DELETE /api/mlx/instances[/{alias}]
session.go                Session lifecycle management
session_store.go          Session persistence to disk
provider.go               Provider interface + shared types + extractTextContent
provider_claude.go        Claude CLI provider (stream-json, persistent process)
provider_pi.go            pi.dev CLI provider (--mode rpc, JSONL → canonical Claude stream-json)
pi_models.go              pi model discovery via `pi --list-models` (cached)
provider_ollama.go        Ollama HTTP provider (NDJSON streaming)
provider_openai.go        OpenAI-compatible HTTP provider (SSE streaming)
provider_chat_base.go     Base provider: tool-calling loop, MCP + built-in tool dispatch
provider_settings.go      Per-provider settings schema for Eve UI
response_collector.go     Headless response accumulation for HTTP clients
config.go                 Unified config loader (settings.json → OpenAI + llama-server + mlx-serve configs)
server_manager.go         Profile-driven managed-server process manager (launch, health check,
                          port allocation, per-alias stop, instance listing, memory budget +
                          leases + idle reaper). One ServerManager
                          per profile: llama-server (llamaProfile) + mlx-serve (mlxProfile)
gguf.go                   GGUF metadata-header reader + KV-cache size math (SWA + per-layer GQA)
server_memory.go          Per-model resident-memory estimation (GGUF weights+KV, MLX dir+config.json).
relay_router.go           Unified OpenAI-compatible router fronting managed servers + OpenAI endpoints
proxy_registry.go         Reachability + model-list cache for configured OpenAI endpoints (15s TTL)
builtin_tools.go          Generic in-process tool registry (emit-capable; ships no tools today)
terminal_template.go      Terminal template types + store in settings.json's pty map (built-in + custom)
terminal_session.go       Terminal session with PTY management (creack/pty)
relay_spawn.go            Shared relay-managed spawn prep (project-token resolution + ${SUB} expansion)
terminal_manager.go       Terminal CRUD + lifecycle management
terminal_log.go           On-disk head/tail log files for PTY replay after eviction
api.go                    HTTP routes (sessions, terminals, permissions, generated images)
ws.go                     WebSocket server (streaming events to Eve, terminal I/O)
permission.go             Permission request/response tracking
cmd/hook/                 Compiled PreToolUse hook binary
```

## Providers

- **Claude**: Persistent process. `claude --print --output-format stream-json --input-format stream-json --verbose --model <model>`. Resumes via `--resume <sessionId>`. Headless sessions add `--dangerously-skip-permissions --permission-mode bypassPermissions` and set `RELAY_LLM_HEADLESS=true` env var (hook auto-approves).
- **pi (pi.dev coding agent)**: Persistent process spawned as `pi --mode rpc --provider <upstream> --model <id> --session-dir {dataDir}/pi-sessions [--thinking <level>] [--session <piSessionId>]`. Model identifier convention: `pi/<provider>/<modelId>` (e.g. `pi/anthropic/claude-sonnet-4-20250514`). Resume via `--session <piSessionId>` — pi owns its session JSONL under `{dataDir}/pi-sessions/`. No PreToolUse hook: pi runs in its no-permission-popups default and auto-executes tools; `CapabilitiesForProvider("pi")` therefore omits `SupportsPermissions`. Auth (API keys, OAuth tokens) is inherited from the user's environment / `~/.pi/agent/auth.json`. `PI_OFFLINE=1` + `PI_SKIP_VERSION_CHECK=1` are set at spawn to suppress pi's startup network calls. Pi's RPC event stream (`message_update` with `assistantMessageEvent`, `tool_execution_*`, `agent_end`) is canonicalized into the Claude `content_block_start`/`delta`/`stop` + `result` envelope inside `provider_pi.go:translate`, so Eve renders pi sessions with the existing Claude renderer. Mid-session capabilities: `PUT /api/sessions/:id/model` (sends `set_model` RPC), `PUT /api/sessions/:id/thinking-level` (sends `set_thinking_level`). `StopGeneration` uses pi's in-band `abort` RPC instead of process kill, so the subprocess survives stop/resume cycles. Per-session settings: `thinkingLevel` (off/minimal/low/medium/high/xhigh).
- **Ollama**: HTTP client with NDJSON streaming. Base URL via `--ollama-url` / `OLLAMA_URL` (default `http://localhost:11434`). Sends full conversation history per request; relies on Ollama's automatic KV cache prefix reuse. Per-session settings: `temperature`, `top_p`, `top_k`, `min_p`, `think` (bool), `num_ctx`. Explicitly sends `think: false` to suppress built-in reasoning on thinking models (e.g. Gemma 4). Supports image attachments via base64.
- **OpenAI-compatible**: HTTP client with SSE streaming. Configured via `settings.json` `openai` section (or legacy `openai_endpoints.json` / `OPENAI_BASE_URL`/`OPENAI_API_KEY`). Model selection: `prefix/model-id` (e.g. `omlx/Qwen3.5-27B`). Supports tool calling.
- **llama.cpp**: Managed llama-server processes via `ServerManager` (`llamaProfile`). Configured via `settings.json` `llama-server` section (or legacy `llama_models.json`). Model selection: `llama/{alias}` (e.g. `llama/qwen3-8b`). Launches llama-server on demand with configured GGUF model and flags, reuses running instances across sessions. Communicates via OpenAI-compatible API (reuses `OpenAIChatTransport`). Binary path: `--llama-server-path` / `LLAMA_SERVER_PATH` / config `binaryPath` / `llama-server` on PATH. Config keys in each model entry map 1:1 to llama-server CLI flags (except `alias` which is the routing name). Per-model locking: launches of different models proceed concurrently; concurrent requests for the same model wait on a shared `ready` channel. Per-session settings: same as OpenAI (temperature, top_p, top_k, min_p, etc.) — override server-level defaults set in the config.
- **MLX (mlx-serve)**: Managed [mlx-serve](https://github.com/ddalcu/mlx-serve) processes via `ServerManager` (`mlxProfile`) — native Zig + mlx-c, no Python. Same shape as llama.cpp in every way: `settings.json` `mlx-serve` section (identical schema to `llama-server`), model selection `mlx/{alias}`, on-demand launch + `/health` poll + instance reuse, OpenAI transport. Differences: the `model` config key is an **MLX model directory** (e.g. an `mlx-community/*` HF snapshot), the manager always appends `--serve`, base port defaults to 9400, and binary resolution is `--mlx-serve-path` / `MLX_SERVE_PATH` / config `binaryPath` / `mlx-serve` on PATH. Useful per-model flags: `ctx-size`, `temp`, `max-tokens`, `kv-quant`, `reasoning-budget`, `no-vision`. See `docs/decisions/007-profile-driven-server-manager.md`.

## Relay-router (`relay_router.go`)

Optional unified OpenAI-compatible router (`--router-port` / `RELAY_ROUTER_PORT`). Single TCP listener that aggregates every locally-routable model behind one endpoint:

- `GET /v1/models` and `GET /models` — lists managed-server aliases (bare, e.g. `qwen3-8b` — llama-server and mlx-serve alike) plus every reachable OpenAI-endpoint model prefixed with the endpoint name (e.g. `omlx/Qwen3.5-27B`).
- `POST /models/load` / `POST /models/unload` — `{"model": "<alias>"}`, managed aliases only (endpoint-prefixed models 400). Load is **asynchronous**: it starts the launch and returns immediately, because llama.cpp-compatible clients put a short timeout on the request itself (pi uses 15s) and a cold 40GB model would otherwise abort it. Poll `/models` for `status.value` to reach `loaded`. Unload is idempotent.
- `GET /health` — liveness.

**llama.cpp router-mode compatibility.** Catalog rows carry llama.cpp's extra fields alongside the OpenAI ones — `status: {value: "loaded"|"loading"|"unloaded", failed?, error?}`, `meta.n_ctx` / `meta.n_ctx_train`, `architecture.input_modalities` (`image` when `mmproj` is configured). Endpoint-backed rows carry all three too — a client that reads `architecture.input_modalities` unconditionally would reject a catalog where only managed rows had it. Their modalities come from the upstream's own `architecture.input_modalities` when it declares one (llama.cpp router mode does); plain OpenAI `/v1/models` has no modality field, so a quiet upstream reads as text-only. Vision is never inferred — offering images to a server that cannot take them fails mid-turn, which is worse than not offering. **`loaded` means usable, not resident**: we launch on demand, so any configured alias serves a request immediately. Reporting residency would make models disappear from a client's picker every time the idle reaper ran, because clients filter their model list to `loaded`. `loading` is reported while a launch is genuinely in flight, and `unloaded` + `failed` when an explicit load failed and nothing is running. Real residency lives in `/api/status` `instances` (leases, memory, idle time). Context: `n_ctx` comes from the model's configured `ctx-size`, `n_ctx_train` from the GGUF header (or MLX `config.json`); clients read `n_ctx ?? n_ctx_train`, so an unpinned model still reports a real window. OpenAI-endpoint models get `n_ctx` from whatever the upstream advertises (`max_model_len`, `max_context_length`, `context_length`, `context_window`, or `meta.n_ctx`). This is load-bearing, not decorative: clients written against llama.cpp router mode validate that *every* row has a string `status.value` and reject the entire catalog otherwise. pi ships a hidden built-in `llama.cpp` extension that does exactly this, so the fields are what let pi enumerate our models live instead of from a hand-maintained `models.json` array. The fields are additive, so plain OpenAI clients ignore them. `status.failed` matters too — a client polling for `loaded` after a load spins until its own timeout without it. Not implemented: `GET /models/sse` (progress events; clients fall back to polling) and `POST /models` (Hugging Face download).
- Everything else — dispatched by reading the request body's `model` field:
  - If the model matches a configured managed-server alias, `GetOrLaunch()` brings up the right server and the request is reverse-proxied to it (SSE flushed via `FlushInterval: -1`). Managers are checked in priority order — llama first, then mlx — so llama wins alias collisions (logged at startup; shadowed aliases are also dropped from `/v1/models` and the pi overlay).
  - Otherwise, if the model matches a configured virtual name (`virtual-llms`), the router attempts its ordered candidates — see **Virtual LLM failover** below.
  - Otherwise the model is parsed as `endpoint.Name/upstreamID`; the registry resolves the endpoint, the body's `model` field is rewritten to bare `upstreamID`, the inbound `Authorization` is replaced with the endpoint's API key, and the request is reverse-proxied to the endpoint's baseURL. Bare managed aliases and `endpoint.Name/id` model ids occupy distinct namespaces, so an endpoint name equal to a managed alias collides with nothing; only an alias that itself contains `/` can intercept an endpoint model id (warned at startup).
  - Unknown / not-currently-online models 400.

Reachability of OpenAI endpoints is tracked by `ProxyRegistry` (`proxy_registry.go`) with a 15 s natural-expiry TTL — no background goroutine. The first `/v1/models` request after expiry triggers parallel probes (single-flighted per endpoint) of upstream `/v1/models`. Offline endpoints disappear from the router's listing until the next probe succeeds; rejecting routing into a known-down endpoint avoids surfacing confusing upstream errors to clients. Probe results — including failures — re-stamp `LastChecked = now`, so a dead upstream isn't re-probed every request. A probe's own network call runs on a context detached via `context.WithoutCancel` from the inbound request that triggered it (`ProxyRegistry.probe`): the probe is shared across every concurrent `Snapshot()` caller (single-flighted per endpoint), so one caller hanging up mid-probe is not evidence the upstream is down, and cancelling on that basis would poison the 15s cache with a false "offline" for a healthy endpoint. `FetchOpenAIModels`'s own 3s client timeout still bounds it.

External clients use either the bare managed alias (`"model": "qwen3-8b"`) or the endpoint-prefixed id (`"model": "omlx/Qwen3.5-27B"`) — no `llama/`, `mlx/`, or `openai/` prefix.

**Virtual LLM failover** (`virtual-llms` config, `resolvedVirtualTarget` / `virtualCandidates` / `routeVirtual`). A virtual name (e.g. `vCode`) maps to an ordered list of targets — each either an `{endpoint, model}` pair or a local managed-server `{alias}`. Declared order is a *preference*, not a hard gate: `candidatesForVirtual` walks the targets twice — pass one collects everything currently believed usable (an alias present in some manager, or an endpoint the registry's last probe found online), pass two appends the rest of the endpoint targets (configured, just currently believed offline) as last-resort attempts, still in declared order. Treating "online" as a hard gate would make a healthy endpoint unroutable for up to 15s after it recovers, and a dead one look routable for up to 15s after it drops (the registry's cache is 15s stale by design); preferring-but-still-attempting means the virtual name works whenever *any* target actually works, not only when the cache agrees with reality.

`routeVirtual` attempts candidates in that order, advancing to the next one only on a **pre-response failure** — a dial/connection error or a managed-server `Acquire` error, i.e. nothing has reached the client's socket yet. It detects this with a small `http.ResponseWriter` wrapper (`virtualResponseRecorder`) that tracks whether `WriteHeader`/`Write` ever ran, and an `onError` hook threaded into `newUpstreamProxy` so a retryable attempt's `ErrorHandler` never gets to write its default 502 (which would otherwise beat the retry to the client). Once a response byte has reached the client — including mid-SSE-stream — the exchange is committed and the router never retries; it surfaces whatever the upstream gave, because retrying could duplicate a side-effecting request onto a second backend or splice two responses together. The retry path's transport (`virtualDialTransport`) clones `http.DefaultTransport` with a 3s `DialContext` timeout so a target that black-holes packets (as opposed to actively refusing the connection) can't eat the default ~30s dial timeout per candidate before failover even starts; `ResponseHeaderTimeout` is deliberately left unset since generation can legitimately be slow. Only the virtual retry path uses this bounded transport — direct managed/endpoint routes keep the default.

When every candidate for a *configured* virtual name fails, the router answers **503**, naming the model and every target tried plus its error (`virtual model "vCode": no target reachable (endpoint "europa": dial tcp ...; endpoint "omlx": connection refused)`) — never the 400 "unknown model" response, since a configured virtual name is never actually unknown and that error sends debugging in the wrong direction. 400 "unknown model" is reserved for names matching nothing at all (checked via `p.virtual.Find` before falling through).

**Conversation affinity** (`virtual_affinity.go`, `virtualAffinityStore` / `applyAffinity` / `resolvedVirtualTarget.identity`). Reachability-preferred ordering is the wrong default once a conversation is already underway: two backends encode reasoning differently (llama.cpp's `content` array + `encrypted_content` vs. oMLX's `summary`-only), a client that replays reasoning history on every turn (Oh My Pi does) will have that history rejected outright by a backend that didn't produce it, and there is no way to translate between the two — `encrypted_content` is opaque and valid only for the model that emitted it. A mid-conversation failover-and-failback is therefore not a hiccup, it's a wedge: every retry replays the same poisoned history forever. See [ADR-010](docs/decisions/010-virtual-model-conversation-affinity.md) for the production incident (97 turns on one backend, 8 on the other, then a permanent `400 item['content'] is not an array`) that this fixes.

`handleProxy` reads a conversation identifier from the request body — `prompt_cache_key` first, then `user` (both standard OpenAI fields; never a header or the client IP, since a wrong key would pin unrelated conversations together). `virtualAffinityStore` maps `(virtual name, conversation key) → (target identity, lastUsed)`, where target identity is `"endpoint:<name>"` or `"alias:<name>"` so an endpoint and a managed alias sharing a bare name can't collide. When a pin exists, `applyAffinity` moves that target to the *front* of the candidate list `candidatesForVirtual` already computed — ahead of reachability preference, not merged into it, so a 15s cache wobble can never hop an established conversation to a different backend. A pin naming a target no longer in the candidate list (removed from config) is silently ignored and normal ordering resumes. `routeVirtual` records (or refreshes) the pin only after an attempt actually succeeds — never on a failed one — including re-pinning to a *new* target when the previously-pinned one fails pre-response and a later candidate serves instead; the conversation is already contaminated by that switch, so the fix is to pin forward rather than flap back next turn. The store is bounded exactly like `ProxyRegistry`'s reachability cache: a 1-hour TTL since last use and a 1024-entry LRU cap, both swept lazily on the write path — no background goroutine. No key present (no `prompt_cache_key`, no `user`) means no pin is ever looked up or recorded — identical to pre-affinity behavior.

A virtual name always appears in `/v1/models`, unlike an endpoint model that simply disappears when its probe goes offline — the point of a stable name is that it's always there to poll and dispatch against. `status.value` is `"loaded"` only when at least one candidate came from the "currently usable" pass (`freshCount > 0`); otherwise it's `"unloaded"` with `failed: true` and an `error` explaining why, so a client polling for readiness stops instead of spinning on a name that's really configured wrong. `architecture.input_modalities` and `meta` (`n_ctx`/`n_ctx_train`) are inherited from the first attempt-order candidate — a managed alias's `ModelCatalog()` entry, or the matching `UpstreamModel` from the registry snapshot — falling back to `["text"]` with no `meta` when unknown, since a stale/offline endpoint candidate has no cached model list to read from. Never inferring `image` support here follows the same rule as the rest of the catalog: offering images to a server that can't take them fails mid-turn, which is worse than not offering.

Startup validation (`warnVirtualModelConfig` in `main.go`, sibling to `warnAliasShadowing`) warns about virtual-model dead config: a virtual name shadowed by a managed alias (dispatch checks managers first), a name containing `/` (would intercept `endpoint/id` routing), a target with only one of `endpoint`/`model` set, a target naming an endpoint or alias that doesn't exist, a virtual with zero usable targets, and two virtuals sharing a name.

## Built-in Tools

`BuiltinToolRegistry` (`builtin_tools.go`) is the generic mechanism for in-process tools that run alongside MCP tools in the `BaseChatProvider` loop (`provider_chat_base.go`) and need an `emit` progress callback MCP tools can't provide. Dispatch order in `runToolLoop()`: built-ins first (`builtinTools.Has()`), then MCP.

It currently **ships no tools**. Image generation is no longer a built-in — it is the `comfyui` MCP tool reached through relay like any other MCP (see relay ADR-006). relayLLM only *serves* the output directory: `GET /api/generated/:filename` returns images that the relay-comfyui MCP wrote to `{dataDir}/generated/`.

## API

Unix socket at `--socket` (defaults to `{data-dir}/relayllm.sock`). WebSocket at `/ws`.

### HTTP Endpoints
```
GET            /api/models         — list available models (Claude + Ollama + OpenAI endpoints + llama.cpp + MLX)
GET/POST       /api/sessions       — list/create sessions
POST           /api/sessions/:id/message — send message (sync, for HTTP clients)
POST           /api/sessions/:id/stop  — stop generation (mirrors WS stop_generation)
POST           /api/sessions/:id/delete — end + delete persisted session data
DELETE         /api/sessions/:id   — end session
PUT            /api/sessions/:id/model           — pi only: mid-session model switch
PUT            /api/sessions/:id/thinking-level  — pi only: mid-session reasoning depth
GET            /api/status         — runtime status (uptime + sessions + terminals + embedded `instances` + `mlxInstances` + `budgets` arrays). Drives relay's Service Inspector via the manifest; rows in `instances` / `mlxInstances` feed the declared `stop-llama` / `stop-mlx` actions' `{alias}` placeholder.
GET            /api/llama/instances        — list running llama-server instances
DELETE         /api/llama/instances/{alias} — stop a specific llama-server instance
GET            /api/mlx/instances          — list running mlx-serve instances
DELETE         /api/mlx/instances/{alias}  — stop a specific mlx-serve instance
GET            /api/terminal/templates     — list terminal templates (read-only)
GET            /api/terminal/templates/:id — get one template
GET/POST       /api/terminals              — list/create terminal instances (POST accepts extraArgs to append per-task argv)
DELETE         /api/terminals/:id          — close terminal
GET            /api/terminals/:id/log      — stitched head+tail of PTY's raw byte stream (works after session eviction)
POST           /api/permission     — hook binary posts here, held open until user decides
GET            /api/generated/:filename — serve generated images (ComfyUI output)
```

Project endpoints (`/api/projects/*`) and scheduler/task endpoints (`/api/tasks/*`) live in relay and relayScheduler respectively. Under relay's front-door dispatcher, Eve reaches them through relay directly — relayLLM never proxies foreign services.

### WebSocket Protocol
```
Client → Server: join_session, send_message, end_session, permission_response
Server → Client: session_joined, llm_event, stats_update, message_complete, permission_request, error

Terminal messages:
Client → Server: terminal_create, join_terminal, leave_terminal, terminal_input (base64), terminal_resize, terminal_close, terminal_list, terminal_reconnect, terminal_templates
Server → Client: terminal_created, terminal_joined (with base64 scrollback), terminal_output (base64), terminal_exit, terminal_closed, terminal_list, terminal_templates
```

The grouped `WSMsg*` constants in `ws_messages.go` are the authoritative message
set (the lists above are the common subset); `llm_event` payloads follow the
canonical contract in [`docs/event-protocol.md`](docs/event-protocol.md).

## Terminal Sessions

PTY-backed terminal sessions hosted by relayLLM. Eve proxies terminal I/O via WebSocket (base64-encoded). Terminals survive Eve restarts.

- **Templates**: live in the `pty` section of `settings.json` (relay's config editor manages them; the API is read-only). Three protected built-ins: `claude-code`, `opencode`, `shell`. `IdleTimeout` field (minutes, default 1440 = 24h).
- **Idle timeout**: When all viewers disconnect, an idle timer starts. If no viewer reconnects before it fires, the terminal is auto-closed. Configurable per template.
- **Color**: PTY spawned with `TERM=xterm-256color` and `COLORTERM=truecolor` for full 24-bit color.
- **Scrollback**: 100KB in-memory ring buffer per terminal, replayed on reconnect.
- **On-disk log**: Each session's raw byte stream is also teed to `{dataDir}/terminal_logs/{id}.head.log` (first 64KB) + `{id}.tail.log` (rolling, capped at ~960KB → 1MB total). Files survive the session being evicted from memory. The ANSI-aware "head + tail" split preserves the stream's initial mode-setting (cursor home, SGR resets) so xterm replay renders correctly even when the middle was truncated. Used by `GET /api/terminals/{id}/log`. A daily sweeper deletes files older than 30 days; bounded by a 500MB total cap as a safety net.
- **Per-task `extraArgs`**: `POST /api/terminals` accepts `extraArgs []string` that are appended to the template's argv after `${PROJECT_PATH}` / `${RELAY_TOKEN}` substitution. Same substitution applies to extras. Used by relayScheduler to schedule shell-style tasks against a shared template.

## Data

Default: `os.UserConfigDir()/relayLLM` — on macOS `~/Library/Application Support/relayLLM/`, on Linux `~/.config/relayLLM/`. Override: `--data-dir` or `RELAY_LLM_DATA`.
- `sessions/` — per-session JSON files. A daily sweeper deletes files where `headless: true` and mtime is older than 7 days; non-headless (Eve-owned) sessions are never touched.
- `pi-sessions/` — pi.dev session JSONLs (one per pi session, owned by pi via `--session-dir`). Daily sweeper deletes files whose `piSessionId` is no longer referenced by any `sessions/*.json` (with a 1h minAge cushion to avoid racing live pi processes).
- `settings.json` — unified provider config **and** the `pty` terminal-template map (preferred). Falls back to separate `openai_endpoints.json` + `llama_models.json` if absent, then `OPENAI_BASE_URL`/`OPENAI_API_KEY` env vars:
  ```json
  {
    "openai": {
      "endpoints": [
        {"name": "lmstudio", "baseURL": "http://localhost:1234/v1", "group": "LM Studio"}
      ]
    },
    "llama-server": {
      "binaryPath": "/usr/local/bin/llama-server",
      "modelDir": "~/models/",
      "basePort": 8090,
      "maxLoaded": 2,
      "maxMemoryGB": 96,
      "idleTimeoutMinutes": 30,
      "models": [{
        "alias": "qwen3-8b",
        "model": "/models/Qwen3-8B-Q4_K_M.gguf",
        "ctx-size": 131072, "n-gpu-layers": -1, "threads": 8,
        "flash-attn": true, "kv-unified": true,
        "cache-type-k": "q8_0", "cache-type-v": "q8_0",
        "temp": 0.6, "top-p": 0.95, "top-k": 20, "min-p": 0.0
      }]
    },
    "mlx-serve": {
      "binaryPath": "~/.local/mlx-serve/mlx-serve",
      "modelDir": "~/models/",
      "basePort": 9400,
      "models": [{
        "alias": "qwen3.5-4b-mlx",
        "model": "mlx-community/Qwen3.5-4B-8bit",
        "max-tokens": 8192, "temp": 0.7
      }]
    },
    "pi": {
      "binaryPath": "~/.npm-global/bin/pi",
      "extraArgs": ["--no-context-files"],
      "useRelayToken": true,
      "env_passthrough": ["ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"],
      "projectOverlay": {
        "mode": "always",
        "defaultProvider": "relay-router",
        "defaultModel": "qwen3-8b",
        "defaultThinking": "medium"
      }
    }
  }
  ```
  **Memory budget** (optional, per managed-server section; all default to off so behavior is unchanged until set — see `docs/decisions/009-managed-server-memory-budget.md`): `maxLoaded` caps concurrent instances, `maxMemoryGB` caps the sum of estimated resident memory, `idleTimeoutMinutes` reclaims instances nobody is using. Either cap evicts the least-recently-used *idle* instance; a leased instance (mid-turn) is never evicted — when everything is busy, admission waits up to `admissionTimeoutSeconds` (default 120) and then errors naming the busy aliases. Model sizes are computed, not declared: weights from the file size, KV cache from the GGUF header (`gguf.go`) honoring sliding-window attention and per-layer GQA, plus `memoryHeadroomPercent` (default 10) for compute buffers. Per-model `memoryGB` overrides the estimate; a model whose size can't be determined counts against `maxLoaded` but not `maxMemoryGB`. Current usage is reported in `/api/status` under `budgets`, and per-instance `leases` / `estimatedGB` / `idleSeconds` in `instances`.

  Each llama-server / mlx-serve model key except `alias` (and the budget-only `memoryGB`) maps 1:1 to a `--{key}` CLI flag. Value translation: `true` → `--key`, `false` → omit, number → `--key value`, string → `--key value`. Optional `port` per model overrides auto-allocation. `modelDir` (supports `~`) is prepended to relative `model` paths (for mlx-serve, `model` is an MLX model *directory*). `--openai-config` flag overrides the `openai` section. The `pi` section is optional: `binaryPath` (supports `~`) takes priority over the well-known fallback chain in `resolvePiPath` (`~/.local/bin/pi`, npm globals, `/opt/homebrew/bin/pi`, `/usr/local/bin/pi`, then `$PATH`); `extraArgs` are appended verbatim to every `pi --mode rpc` spawn (e.g. force-skip context files, add `--extension`). The relay-managed fields mirror the PTY `pidev` template's shape and route through the shared `RelayManagedSpec.Resolve()` helper in `relay_spawn.go`: `useRelayToken` (or, for terminals, a non-empty `projectID`) injects a project-scoped `RELAY_PROJECT_TOKEN` env var, resolved just-in-time from relay's bridge — never the full-access service token; `env_passthrough` copies the listed env var keys from `os.Environ()` into the spawned pi. Skill *generation* is owned by relay (see relay ADR-004 + `docs/decisions/006-skill-regen-owned-by-relay.md`); relayLLM no longer regenerates SKILL.md. Skills load from the convention `<project>/.claude/skills`: the pi `--mode rpc` provider auto-appends `--skill <project>/.claude/skills` (skipped if `extraArgs` already contains `--skill`), and PTY templates reference `${PROJECT_PATH}/.claude/skills` in their args (e.g. the `pidev` template: `"args": ["--skill", "${PROJECT_PATH}/.claude/skills"]`).

  **`projectOverlay`** (optional) writes a per-project `<projectDir>/.pi/` directory before each pi spawn (both `--mode rpc` and PTY `pi` templates) and sets `PI_CODING_AGENT_DIR` so pi reads from it. Pi's global `~/.pi/agent/` is never written to. Materialized files: `models.json` containing a single `relay-router` provider pointing at the `--router-port` listener, with its `models` array snapshotted from the router's currently-routable set at spawn time — managed-server aliases (bare, llama + mlx) plus every reachable OpenAI endpoint model (prefixed `endpoint.Name/`). Each row carries `input: ["text"]` or `["text","image"]`: pi's `openai-completions` provider does no discovery, and it gates image attachments on this array (`model-config.js`), so a vision model written as a bare `{"id": ...}` is one pi refuses to send images to ("Current model does not support images"). Managed aliases get `image` from a configured `mmproj`; endpoint models only when the upstream advertised it. Pi's ModelRegistry treats providers with an empty `models` array as override-only, so the enumeration is required; the snapshot uses `ProxyRegistry.Snapshot()` and inherits its 15 s TTL. Set `--router-port` to enable this entry; otherwise relayLLM contributes nothing to pi's models.json and the user's global providers carry through unchanged. Also writes `settings.json` with `defaultProvider`/`defaultModel`/`defaultThinkingLevel` and a `skills` array (project `.claude/skills/` + `extraSkillDirs`); `auth.json` symlinked to `~/.pi/agent/auth.json` so credentials stay centrally managed (OAuth refresh writes through). Modes: `"never"` (default — feature off), `"always"` (rewrite on every spawn), `"skipIfExists"` (write missing files only). User's global `models.json` providers and `settings.json` keys are merged underneath by default (turn off via `excludeUserProviders`/`excludeUserSettings`). Set `authStrategy: "none"` if pi credentials are managed out-of-band. `gitignore: true` opt-in appends the overlay dir to the project's `.gitignore`. Fails closed at spawn if global `auth.json` is missing while symlink strategy is active — run `pi auth login` once globally first.
- `generated/` — images written by the relay-comfyui MCP tool (served via `/api/generated/`)

## Build

```bash
go build .                          # main binary
go build ./cmd/hook                 # permission hook binary
```

## Testing

Three tiers, gated by build tag:

```bash
go test ./...                       # default: hermetic, no external deps, <2 s
go test -tags=live ./...            # opt-in: requires Ollama / LM Studio / OMLX / relay binary running locally
go test -tags=llm ./...             # opt-in: requires Qwen3.6 MoE 35 in settings.json (see below)
```

Default tier covers WS protocol, HTTP API, session lifecycle, tool-call loop, pi event translation, and manifest registration — all driven by fakes in `support_test.go` / `support_server_test.go`. No real LLM, no subprocess, no network.

The `llm` tier (`provider_llama_live_test.go`) drives the real `LlamaServerManager` against an installed model. **Prerequisite**: `Qwen3.6 MoE 35` registered in `~/Library/Application Support/relayLLM/settings.json` under `llama-server.models` with the model file present at the configured `modelDir`. Skips gracefully if absent. Validates SSE chunking, llama-server lifecycle, and mid-stream stop — the surface fakes can't reach.

The `live` tier is for legacy integration tests that depend on third-party services. Kept for manual smoke; never run in default CI.

### Pre-commit hook

Install once per clone:

```bash
git config core.hooksPath .githooks
```

Runs `go build ./...`, `go vet ./...`, and the hermetic test suite under the race detector (`go test -race ./...`) on every commit that touches Go files. ~3s total warm (~+1s for `-race`; a one-time ~+4s when the instrumented build cache is cold). Skip in emergencies with `git commit --no-verify`. The `live` and `llm` build tags are not invoked by the hook — those stay opt-in.

### Adding tests

- Need an HTTP/WS surface to drive? Use `NewTestServer(t, nil)` from `support_server_test.go`.
- Need a scripted LLM stream? `srv.SetFakeProvider()` then `fp.ScriptText(...)` / `fp.ScriptResult(...)`.
- Need deterministic timing? `NewFakeClock(t0)` + `clock.Advance(d)`. Wire via `TestServerOptions{Clock: ...}`.
- Need fake tool calls? `NewFakeMCPClient(FakeTool{Name, Handler})`.
- Need to validate the relay bridge handshake? `NewFakeBridge(t)` + `withBridgeEnv(t, ...)`.

## Ecosystem

relayLLM is one of several relay-enhanced services. It serves session/provider operations only — projects live in relay, scheduled tasks live in relayScheduler. Eve reaches every backend through relay's front door.

- `../relay/` -- macOS tray orchestrator. Hosts the front-door dispatcher that routes inbound traffic per each enhanced service's registered manifest.
- `../eve/` -- Browser-based LLM frontend. Talks to relay's frontend socket; relay dispatches `/api/sessions`, `/api/terminals`, etc. to relayLLM.
- `../relayScheduler/` -- Task scheduler. Registers its own manifest with relay; relay dispatches `/api/tasks/*` to it directly (relayLLM does not proxy).
- `../relayTelegram/` -- Telegram bot bridge.
- `../relayComfy/` -- ComfyUI service exposed as the `comfyui` MCP. relayLLM reaches image generation through relay's MCP path, not a direct HTTP call; it only serves the resulting images via `/api/generated/`.

## Releases & consumers

**Release model**: continuous from `main`. No tags, no SemVer, no release branches. Consumers build and run against whatever's on `main`.

**Consumers** — who depends on relayLLM's behavior (relay is the transport layer, not a consumer):

- **Eve** — browser frontend. Reaches relayLLM through relay's front-door dispatcher; never speaks to the socket directly.
- **relayScheduler** — schedules tasks against terminal templates via HTTP/WS.
- **Standalone CLI / scripts** — humans or scripts hitting the unix socket directly with no relay in front. Treat the documented HTTP/WS surface as a public API for this audience.

**"Done" definition** — every change that lands on `main` must:

1. Pass the hermetic test tier. Enforced by `.githooks/pre-commit` (install once with `git config core.hooksPath .githooks`). If the hook is bypassed, run `go test ./...` manually before merging.
2. Run the `live` and/or `llm` tiers manually when the change touches the relevant surface — providers (`-tags=live` for Ollama / LM Studio / OMLX / relay binary; `-tags=llm` for llama-server with a real GGUF). Note in the commit message which tiers you ran.
3. Add an ADR under [`docs/decisions/`](docs/decisions/) for architectural changes: new test seams, new protocols, new build-tag tiers, new run modes. Skip for routine refactors and library upgrades.
4. Reflect the new state in [`ROADMAP.md`](ROADMAP.md) — close shipped items into the **Closed** section with a one-line note, update **In flight**, add follow-ups you discovered.

**Breaking changes** — anything that alters the wire (WS protocol message shapes, manifest schema, HTTP route paths or payloads, terminal template fields, session settings keys) ships as a **coordinated PR across repos**. Land relayLLM and the matching changes in `../relay`, `../eve`, `../relayScheduler` together. There is no manifest version negotiation or compat shim; one person owns all the repos, so coordinate at PR time rather than building permanent backwards-compat. If a change genuinely cannot be coordinated atomically, ship the additive side first (new field / new route) and migrate consumers in a follow-up PR before removing the old surface.

## Service Manifest Integration

relayLLM detects its run mode from `RELAY_BRIDGE_SOCKET`:

- **Standalone** (env unset): binds its own listener (`--socket`, default `{data-dir}/relayllm.sock`), auto-generates a bearer token if `--token`/`RELAY_LLM_TOKEN` is unset, serves direct HTTP/WS clients.
- **Enhanced** (env set): same listener + same wire language, plus it dials the bridge socket and sends a `RegisterManifest` payload declaring its routes, status endpoint, and actions. Relay's dispatcher then forwards matching front-door requests over the internal socket using the same bearer token relayLLM declared in the manifest.

The mode switch is a deployment fact, not a code fork — one config loader, two sources. Both `manifest.go` (what relayLLM exposes) and `relay_bridge_client.go` (how it talks to relay) are small and self-contained.

See `../relay/docs/service-manifest.md` for the full protocol contract.

## Local Auth

`auth.go::bearerAuth` validates every HTTP/WS request against `--token` / `RELAY_LLM_TOKEN`. Empty token + standalone mode → auto-generated 64-char hex (not logged for security; set the env var to pin). Token comparison is constant-time via `crypto/subtle.ConstantTimeCompare`. The same token is plumbed through `SessionManager` → `ClaudeProvider` → hook child env as `RELAY_LLM_HOOK_TOKEN`; the hook binary uses it on its `/api/permission` POST.

### Relay-side token rotation

The `RELAY_PROJECT_TOKEN` env var carried into chat-provider MCP spawns, pi sessions, and project-scoped terminals is a *relay project token*, not relayLLM's local bearer. **relayLLM never stores it and never receives it from eve** — it resolves the token just-in-time from relay's bridge by `projectId` at every spawn (`resolveProjectToken` → `ResolvePtyEnv`), injects it, and discards it. This makes rotation transparent (the next spawn picks up the new token via `RotateProjectToken`) and means a relayLLM restart can't lose it (there's nothing stored to lose — the old `Session.McpToken` field is gone). If a project token can't be resolved, the child gets no token (fail closed) — relayLLM never substitutes the full-access `RELAY_SERVICE_TOKEN`. The service token is used *only* to authenticate relayLLM's own bridge calls. See `../relay/docs/decisions/007-project-token-brokering.md` and `../relay/docs/tokens.md`.
