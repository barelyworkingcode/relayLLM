# relayLLM (Go)

Standalone LLM engine service. Manages providers (Claude CLI, pi.dev CLI, Ollama HTTP, OpenAI-compatible HTTP, llama.cpp + MLX managed processes), sessions, projects, permissions, and terminal sessions (PTY). Runs independently or as a relay-enhanced service.

Under relay, relayLLM registers a [service manifest](../relay/docs/service-manifest.md) describing the routes it serves; relay's front-door dispatcher forwards matching traffic. relayLLM does not know about projects, tasks, or any sibling service — it stays focused on session/provider execution.

## For open-ended / lead-developer requests

When the user gives an open-ended ask (e.g. "what's next", "act as lead developer", "improve this codebase") — not a specific task — start by reading:

1. [`ROADMAP.md`](ROADMAP.md) — prioritized backlog, survives session compaction.
2. [Releases & consumers](#releases--consumers) below — release model, who depends on relayLLM, "done" definition, breaking-change protocol.

The *why* behind non-obvious architecture lives as comments at the point of the code, not in a separate decisions log — grep the relevant file first.

Propose a plan before executing. For specific tasks ("fix this bug", "add X"), do the task — don't read ROADMAP first.

## Architecture

Standalone service binary, so almost everything lives under `internal/`
(not meant to be imported by another module) with a thin `cmd/relayllm`
entry point. Package boundaries follow a dependency DAG from leaves
(`types`, `clock`, `config`, `events`, ...) up through `provider` ->
`session`/`terminal`/`router` -> `api` -> `app`; `internal/testutil` holds
the fakes every other package's tests import.

```
cmd/relayllm/main.go              Entry point: six lines, calls app.Main()
cmd/hook/                         Compiled PreToolUse hook binary (separate module)

internal/app/app.go               Flag parsing, server wiring, manifest registration, startup
                                   validation (warnAliasShadowing, warnVirtualModelConfig,
                                   warnAnthropicModelMap), graceful shutdown
internal/app/model_host.go        C9 router.sock/RegisterModelHost wiring, factored out of Main
                                   so it's testable without flag.Parse/os.Exit
internal/api/                     HTTP routes, WS hub, status dashboard
  api.go                            Session/terminal/permission/model/generated-image routes
  api_status.go                     GET /api/status, GET+DELETE /api/llama/instances[/{alias}],
                                     GET+DELETE /api/mlx/instances[/{alias}]
  api_status_detailed.go            GET /api/status/detailed + embedded GET /status dashboard
  auth.go                           bearerAuth, GenerateBearerToken
  tcp_diagnostics.go                TCPDiagnosticsOnly: the read-only allowlist wrapping the
                                     anonymous --http-port front (socket front unaffected)
  ws.go                             WebSocket server (streaming events to Eve, terminal I/O)
  status/                           Embedded dashboard HTML/CSS/JS (go:embed)
internal/session/                 Session lifecycle management
  session.go                        SessionManager: lifecycle, provider wiring, sweep
  session_store.go                  Session persistence to disk
  response_collector.go             Headless response accumulation for HTTP clients
internal/provider/                 All chat providers + shared tool-calling loop
  chat_base.go                       Base provider: tool-calling loop, MCP + built-in tool dispatch
  claude.go                          Claude CLI provider (stream-json, persistent process)
  claude_history.go                  Claude CLI history JSONL read/delete, including over SSH
  pi.go                              pi.dev CLI provider (--mode rpc, JSONL -> canonical Claude stream-json)
  pi_models.go                       pi model discovery via `pi --list-models` (cached)
  ollama.go                          Ollama HTTP provider (NDJSON streaming)
  openai.go                          OpenAI-compatible HTTP provider (SSE streaming)
  settings.go                        Per-provider settings schema for Eve UI
internal/pioverlay/pioverlay.go   Per-project .pi/ overlay (models.json, settings.json, auth.json symlink)
internal/terminal/                 PTY-backed terminal sessions
  terminal_template.go               Terminal template types + store in settings.json's pty map
  terminal_session.go                Terminal session with PTY management (creack/pty)
  terminal_manager.go                Terminal CRUD + lifecycle management
  terminal_log.go                    On-disk head/tail log files for PTY replay after eviction
internal/router/                  Unified OpenAI-compatible router
  router.go                          Lifecycle, core dispatch, StartRelayRouter
  router_models.go                   /v1/models catalog + virtual-row building
  router_virtual.go                  Virtual-model target resolution/candidates + failover routing
  router_virtual_affinity.go         Per-conversation target pinning for virtual-model failover
  router_audio.go                    /v1/audio/transcriptions multipart handling
  router_rewrite.go                  reasoning_effort / chat_template_kwargs body rewrites
  router_anthropic.go                Anthropic Messages API compatibility: routes, passthrough, redirect dispatch
  router_anthropic_translate.go      Pure Anthropic<->OpenAI request/stream translation
  router_passthrough.go              router.passthrough: /<name>/ byte-for-byte proxy, client credential included
  router_socket.go                   router.sock (C9, launched mode): peer-identity admission + the
                                      socket mux (TCP mux minus /api/ and /<name>/ passthroughs)
  status_metrics.go                  Proxy connection state machine feeding the status dashboard
internal/registry/registry.go     Reachability + model-list cache for configured OpenAI endpoints (15s TTL)
internal/servermanager/           llama.cpp / mlx-serve managed-process lifecycle
  server_manager.go                  Profile-driven managed-server process manager (launch, health check,
                                      port allocation, per-alias stop, instance listing, memory budget +
                                      leases + idle reaper). One ServerManager per profile:
                                      llama-server (LlamaProfile) + mlx-serve (MlxProfile)
  gguf.go                            GGUF metadata-header reader + KV-cache size math (SWA + per-layer GQA)
  server_memory.go                   Per-model resident-memory estimation (GGUF weights+KV, MLX dir+config.json)
internal/config/config.go         Unified config loader (settings.json -> OpenAI + llama-server + mlx-serve
                                   configs) + every provider/router/server schema struct
internal/relay/                   Bridge socket transport + manifest
  bridge_client.go                   Transport (SendBridgeRequest, tokenless) + PtyEnv resolution +
                                      RegisterModelHost/RegisterModelHostOrExit (router.sock upstream).
  launch.go                          Launch identity: RELAY_LAUNCH_FD secret read + Hello (captures
                                      relay's own peer audit token, RelayIdentity()); Launched()
  manifest.go                        Service manifest declaration + MaybeRegisterManifest
internal/peertoken/                Reads a Unix socket peer's kernel audit token (LOCAL_PEERTOKEN),
                                    a leaf package copied from relay's own — used by launch.go (the
                                    Hello dial) and internal/router's router.sock (accepted connections).
internal/spawn/spawn.go           Shared relay-managed spawn prep (project-token resolution + ${SUB} expansion)
internal/permission/permission.go Permission request/response tracking
internal/sshhost/sshhost.go       Vendored RemoteCommand/RemoteShellCommand launcher construction for SSH hosts
internal/mcp/mcp.go               MCP client manager
internal/tools/tools.go           Generic in-process tool registry (emit-capable; ships no tools today)
internal/events/                  Canonical llm_event protocol + WS message vocabulary
internal/types/                   Shared data types (Session, Provider, Message, ...) with zero
                                   package dependencies beyond clock/events
internal/clock/clock.go           Clock interface + DefaultClock
internal/netutil/netutil.go       Bind-list parsing + multi-listener helpers
internal/testutil/                Shared test fakes (FakeClock, FakeProvider, FakeMCPClient, FakeBridge, TLS helpers)
```

## Providers

- **Claude**: Persistent process. `claude --print --output-format stream-json --input-format stream-json --verbose --model <model>`. Resumes via `--resume <sessionId>`. Headless sessions add `--dangerously-skip-permissions --permission-mode bypassPermissions` and set `RELAY_LLM_HEADLESS=true` env var (hook auto-approves). **On an SSH host** (`session.Host != nil`, see [`../relay/docs/ssh-hosts.md`](../relay/docs/ssh-hosts.md) for the wire contract and cross-repo split): `Start()` execs `buildHostExec`'s argv — relay's `ssh_argv` + `-T --` + `internal/sshhost/sshhost.go`'s `RemoteCommand` launcher wrapping `host.ClaudePath <args>` — instead of a local subprocess; `--permission-prompt-tool stdio` replaces `--mcp-config` (which is never passed on a host) and the child env carries only `RELAY_LLM_SESSION_ID`. Host permission prompts arrive as Claude's own `control_request`/`control_response` stdio protocol (`processLine`'s `"control_request"` case) instead of the hook, resolved through the same `PermissionManager` a `permission_response` from Eve already uses. `ensureHookConfig`, the `<dir>/CLAUDE.md` read, and `resolveClaudePath` are all skipped for a host session; `pi` is refused outright on a host project.
- **pi (pi.dev coding agent)**: Persistent process spawned as `pi --mode rpc --provider <upstream> --model <id> --session-dir {dataDir}/pi-sessions [--thinking <level>] [--session <piSessionId>]`. Model identifier convention: `pi/<provider>/<modelId>` (e.g. `pi/anthropic/claude-sonnet-4-20250514`). Resume via `--session <piSessionId>` — pi owns its session JSONL under `{dataDir}/pi-sessions/`. No PreToolUse hook: pi runs in its no-permission-popups default and auto-executes tools; `CapabilitiesForProvider("pi")` therefore omits `SupportsPermissions`. Auth (API keys, OAuth tokens) is inherited from the user's environment / `~/.pi/agent/auth.json`. `PI_OFFLINE=1` + `PI_SKIP_VERSION_CHECK=1` are set at spawn to suppress pi's startup network calls. Pi's RPC event stream (`message_update` with `assistantMessageEvent`, `tool_execution_*`, `agent_end`) is canonicalized into the Claude `content_block_start`/`delta`/`stop` + `result` envelope inside `provider_pi.go:translate`, so Eve renders pi sessions with the existing Claude renderer. Mid-session capabilities: `PUT /api/sessions/:id/model` (sends `set_model` RPC), `PUT /api/sessions/:id/thinking-level` (sends `set_thinking_level`). `StopGeneration` uses pi's in-band `abort` RPC instead of process kill, so the subprocess survives stop/resume cycles. Per-session settings: `thinkingLevel` (off/minimal/low/medium/high/xhigh).
- **Ollama**: HTTP client with NDJSON streaming. Base URL via `--ollama-url` / `OLLAMA_URL` (default `http://localhost:11434`). Sends full conversation history per request; relies on Ollama's automatic KV cache prefix reuse. Per-session settings: `temperature`, `top_p`, `top_k`, `min_p`, `think` (bool), `num_ctx`. Explicitly sends `think: false` to suppress built-in reasoning on thinking models (e.g. Gemma 4). Supports image attachments via base64.
- **OpenAI-compatible**: HTTP client with SSE streaming. Configured via `settings.json` `openai` section (or legacy `openai_endpoints.json` / `OPENAI_BASE_URL`/`OPENAI_API_KEY`). Model selection: `prefix/model-id` (e.g. `omlx/Qwen3.5-27B`). Supports tool calling.
- **llama.cpp**: Managed llama-server processes via `ServerManager` (`llamaProfile`). Configured via `settings.json` `llama-server` section (or legacy `llama_models.json`). Model selection: `llama/{alias}` (e.g. `llama/qwen3-8b`). Launches llama-server on demand with configured GGUF model and flags, reuses running instances across sessions. Communicates via OpenAI-compatible API (reuses `OpenAIChatTransport`). Binary path: `--llama-server-path` / `LLAMA_SERVER_PATH` / config `binaryPath` / `llama-server` on PATH. Config keys in each model entry map 1:1 to llama-server CLI flags (except `alias` which is the routing name). Per-model locking: launches of different models proceed concurrently; concurrent requests for the same model wait on a shared `ready` channel. Per-session settings: same as OpenAI (temperature, top_p, top_k, min_p, etc.) — override server-level defaults set in the config.
- **MLX (mlx-serve)**: Managed [mlx-serve](https://github.com/ddalcu/mlx-serve) processes via `ServerManager` (`mlxProfile`) — native Zig + mlx-c, no Python. Same shape as llama.cpp in every way: `settings.json` `mlx-serve` section (identical schema to `llama-server`), model selection `mlx/{alias}`, on-demand launch + `/health` poll + instance reuse, OpenAI transport. Differences: the `model` config key is an **MLX model directory** (e.g. an `mlx-community/*` HF snapshot), the manager always appends `--serve`, base port defaults to 9400, and binary resolution is `--mlx-serve-path` / `MLX_SERVE_PATH` / config `binaryPath` / `mlx-serve` on PATH. Useful per-model flags: `ctx-size`, `temp`, `max-tokens`, `kv-quant`, `reasoning-budget`, `no-vision`. mlx-serve (Zig + mlx-c, zero Python) was chosen over `mlx_lm.server` (needs a pip/uv-managed environment) and SwiftLM (no Homebrew distribution — builds from source and pins a minimum Xcode a beta-OS machine may not satisfy); see the comment above `mlxProfile` in `internal/servermanager/server_manager.go` for the full comparison.

## Relay-router (`internal/router/router.go`)

Optional unified OpenAI-compatible router (`--router-port` / `RELAY_ROUTER_PORT`, bound to `--router-bind` / `RELAY_ROUTER_BIND`, default `127.0.0.1`; comma-separated to bind more than one interface, e.g. `127.0.0.1,192.168.64.1` — see `RelayRouter.Listen`/`Serve` in `internal/router/router.go`). One shared `*http.Server`, one listener per configured bind, that together aggregate every locally-routable model behind one endpoint:

- `GET /v1/models` and `GET /models` — lists managed-server aliases (bare, e.g. `qwen3-8b` — llama-server and mlx-serve alike), configured virtual names, and every reachable OpenAI-endpoint model prefixed with the endpoint name (e.g. `omlx/Qwen3.5-27B`). Rows are built in that same order — managed, then virtual, then endpoint — deduplicated against one shared `seen` set, because it's also `handleProxy`'s dispatch priority: a name colliding across two categories (e.g. a virtual literally named `<endpoint>/<id>`) must list as whichever one dispatch will actually invoke, or the catalog lies about what a request for that id will do.
- `POST /models/load` / `POST /models/unload` — `{"model": "<alias>"}`, managed aliases only (endpoint-prefixed models 400). Load is **asynchronous**: it starts the launch and returns immediately, because llama.cpp-compatible clients put a short timeout on the request itself (pi uses 15s) and a cold 40GB model would otherwise abort it. Poll `/models` for `status.value` to reach `loaded`. Unload is idempotent.
- `GET /health` — liveness.

**llama.cpp router-mode compatibility.** Catalog rows carry llama.cpp's extra fields alongside the OpenAI ones — `status: {value: "loaded"|"loading"|"unloaded", failed?, error?}`, `meta.n_ctx` / `meta.n_ctx_train`, `architecture.input_modalities` (`image` when `mmproj` is configured). Endpoint-backed rows carry all three too — a client that reads `architecture.input_modalities` unconditionally would reject a catalog where only managed rows had it. Their modalities come from the upstream's own `architecture.input_modalities` when it declares one (llama.cpp router mode does); plain OpenAI `/v1/models` has no modality field, so a quiet upstream reads as text-only. Vision is never inferred — offering images to a server that cannot take them fails mid-turn, which is worse than not offering. **`loaded` means usable, not resident**: we launch on demand, so any configured alias serves a request immediately. Reporting residency would make models disappear from a client's picker every time the idle reaper ran, because clients filter their model list to `loaded`. `loading` is reported while a launch is genuinely in flight, and `unloaded` + `failed` when an explicit load failed and nothing is running. Real residency lives in `/api/status` `instances` (leases, memory, idle time). Context: `n_ctx` comes from the model's configured `ctx-size`, `n_ctx_train` from the GGUF header (or MLX `config.json`); clients read `n_ctx ?? n_ctx_train`, so an unpinned model still reports a real window. OpenAI-endpoint models get `n_ctx` from whatever the upstream advertises (`max_model_len`, `max_context_length`, `context_length`, `context_window`, or `meta.n_ctx`). Every row with a known context window also carries it as a top-level `context_length`, alongside — not instead of — `meta`. That's not redundant: a client doing plain OpenAI `/v1/models` discovery (LM Studio-, vLLM-, OpenRouter-style — Oh My Pi's `openai-models-list` branch is a confirmed example) never looks inside `meta` at all; it reads a flat field and, finding nothing, quietly substitutes a hardcoded default (128K) instead of erroring. That's the dangerous case: a model actually pinned to, say, 32K reports as 128K-capable, the client builds a request sized for 128K, and the request fails mid-turn against a server that never claimed to support it. `context_length` collapses `n_ctx`/`n_ctx_train` into the single number a correct client would have picked anyway (pinned wins, trained context is the fallback) — the flat field has no room for two numbers the way `meta` does. It is omitted entirely, never `0` or `null`, whenever neither number is known, exactly mirroring `meta`'s omission — a client chaining `?? default` must fall through cleanly, and a literal `0` would read as a real (if absurd) context window rather than "unknown". Endpoint rows follow the identical rule with their own single number: present when the upstream advertised one, omitted when it stayed quiet. This is load-bearing, not decorative: clients written against llama.cpp router mode validate that *every* row has a string `status.value` and reject the entire catalog otherwise. pi ships a hidden built-in `llama.cpp` extension that does exactly this, so the fields are what let pi enumerate our models live instead of from a hand-maintained `models.json` array. The fields are additive, so plain OpenAI clients ignore them. `status.failed` matters too — a client polling for `loaded` after a load spins until its own timeout without it. Not implemented: `GET /models/sse` (progress events; clients fall back to polling) and `POST /models` (Hugging Face download).
- Everything else — dispatched by reading the request body's `model` field:
  - If the model matches a configured managed-server alias, `GetOrLaunch()` brings up the right server and the request is reverse-proxied to it (SSE flushed via `FlushInterval: -1`). Managers are checked in priority order — llama first, then mlx — so llama wins alias collisions (logged at startup; shadowed aliases are also dropped from `/v1/models` and the pi overlay).
  - Otherwise, if the model matches a configured virtual name (`virtual-llms`), the router attempts its ordered candidates — see **Virtual LLM failover** below.
  - Otherwise the model is parsed as `endpoint.Name/upstreamID`; the registry resolves the endpoint, the body's `model` field is rewritten to bare `upstreamID`, the inbound `Authorization` is replaced with the endpoint's API key, and the request is reverse-proxied to the endpoint's baseURL. Bare managed aliases and `endpoint.Name/id` model ids occupy distinct namespaces, so an endpoint name equal to a managed alias collides with nothing; only an alias that itself contains `/` can intercept an endpoint model id (warned at startup).
  - Unknown / not-currently-online models 400.

Reachability of OpenAI endpoints is tracked by `ProxyRegistry` (`internal/registry/registry.go`) with a 15 s natural-expiry TTL — no background goroutine. The first `/v1/models` request after expiry triggers parallel probes (single-flighted per endpoint) of upstream `/v1/models` — and so does `LookupModel` (the direct `endpoint/id` chat-routing path above), on a per-endpoint cache miss. It has to: `LookupModel` used to only ever read the cache, so on a freshly started relayLLM nothing had populated it yet, and the first chat request naming a live endpoint model 400'd with a misleading "unknown model" until something else (a `/v1/models` catalog request) happened to warm it. `LookupModel` probes only a *configured* endpoint whose status is missing or stale — an endpoint name that isn't in config at all is an immediate miss with no network call, so an unrecognized model id can never trigger one. Offline endpoints disappear from the router's listing until the next probe succeeds; rejecting routing into a known-down endpoint avoids surfacing confusing upstream errors to clients — this is unchanged: a believed-offline entry that is still fresh stays a miss without probing, from either call path. Probe results — including failures — re-stamp `LastChecked = now`, so a dead upstream isn't re-probed every request. A probe's own network call runs on a context detached via `context.WithoutCancel` from the inbound request that triggered it (`ProxyRegistry.probe`): the probe is shared across every concurrent caller of `Snapshot()` or `LookupModel()` (single-flighted per endpoint), so one caller hanging up mid-probe is not evidence the upstream is down, and cancelling on that basis would poison the 15s cache with a false "offline" for a healthy endpoint. `FetchOpenAIModels`'s own `modelsFetchTimeout` (10s) still bounds it. That budget must stay above the worst-case `/models` latency of an upstream that is itself a relayLLM router: such an upstream answers only after probing its own endpoints, so a cold cache costs it a full probe timeout first. Setting this to that same timeout makes a router-in-front-of-a-router flap permanently — every probe loses by milliseconds, the endpoint records offline, and `/v1/models` returns empty forever.

External clients use either the bare managed alias (`"model": "qwen3-8b"`) or the endpoint-prefixed id (`"model": "omlx/Qwen3.5-27B"`) — no `llama/`, `mlx/`, or `openai/` prefix.

**Virtual LLM failover** (`virtual-llms` config, `resolvedVirtualTarget` / `virtualCandidates` / `routeVirtual`). A virtual name (e.g. `vCode`) maps to an ordered list of targets — each either an `{endpoint, model}` pair or a local managed-server `{alias}`. Declared order is a *preference*, not a hard gate: `candidatesForVirtual` walks the targets twice — pass one collects everything currently believed usable (an alias present in some manager, or an endpoint the registry's last probe found online), pass two appends the rest of the endpoint targets (configured, just currently believed offline) as last-resort attempts, still in declared order. Treating "online" as a hard gate would make a healthy endpoint unroutable for up to 15s after it recovers, and a dead one look routable for up to 15s after it drops (the registry's cache is 15s stale by design); preferring-but-still-attempting means the virtual name works whenever *any* target actually works, not only when the cache agrees with reality.

`routeVirtual` attempts candidates in that order, advancing to the next one only on a **pre-response failure** — a dial/connection error or a managed-server `Acquire` error, i.e. nothing has reached the client's socket yet. It detects this with a small `http.ResponseWriter` wrapper (`virtualResponseRecorder`) that tracks whether `WriteHeader`/`Write` ever ran, and an `onError` hook threaded into `newUpstreamProxy` so a retryable attempt's `ErrorHandler` never gets to write its default 502 (which would otherwise beat the retry to the client). Once a response byte has reached the client — including mid-SSE-stream — the exchange is committed and the router never retries; it surfaces whatever the upstream gave, because retrying could duplicate a side-effecting request onto a second backend or splice two responses together. The retry path's transport (`virtualDialTransport`) clones `http.DefaultTransport` with a 3s `DialContext` timeout so a target that black-holes packets (as opposed to actively refusing the connection) can't eat the default ~30s dial timeout per candidate before failover even starts; `ResponseHeaderTimeout` is deliberately left unset since generation can legitimately be slow. Only the virtual retry path uses this bounded transport — direct managed/endpoint routes keep the default.

When every candidate for a *configured* virtual name fails, the router answers **503**, naming the model and every target tried plus its error (`virtual model "vCode": no target reachable (endpoint "europa": dial tcp ...; endpoint "omlx": connection refused)`) — never the 400 "unknown model" response, since a configured virtual name is never actually unknown and that error sends debugging in the wrong direction. 400 "unknown model" is reserved for names matching nothing at all (checked via `p.virtual.Find` before falling through).

**Conversation affinity** (`internal/router/router_virtual_affinity.go`, `virtualAffinityStore` / `applyAffinity` / `resolvedVirtualTarget.identity`). Reachability-preferred ordering is the wrong default once a conversation is already underway: two backends encode reasoning differently (llama.cpp's `content` array + `encrypted_content` vs. oMLX's `summary`-only), a client that replays reasoning history on every turn (Oh My Pi does) will have that history rejected outright by a backend that didn't produce it, and there is no way to translate between the two — `encrypted_content` is opaque and valid only for the model that emitted it. A mid-conversation failover-and-failback is therefore not a hiccup, it's a wedge: every retry replays the same poisoned history forever. This is not a hypothetical: a production conversation had 97 turns served by a llama.cpp endpoint and 8 by an oMLX endpoint, interleaved, after a spurious "endpoint offline" reading caused a mid-conversation failover and then a failback, ending in a permanent `400 item['content'] is not an array` on every subsequent retry. See `internal/router/router_virtual_affinity.go`'s `virtualAffinityStore` doc comment for the full one-directional incompatibility matrix and the alternative (sanitizing inbound reasoning items) that was considered and rejected.

`handleProxy` reads a conversation identifier from the request body — `prompt_cache_key` first, then `user` (both standard OpenAI fields; never a header or the client IP, since a wrong key would pin unrelated conversations together). `virtualAffinityStore` maps `(virtual name, conversation key) → (target identity, lastUsed)`, where target identity is `"alias:<name>"` for a managed alias, or `"endpoint:<name>/<model>"` (each half escaped so `endpoint "a"`/`model "b/c"` and `endpoint "a/b"`/`model "c"` can't collide) for an endpoint target — so an endpoint and a managed alias sharing a bare name can't collide, and neither can two targets on the same endpoint with different models (a big-then-small fallback pair): dropping the model id from the identity let `applyAffinity` match whichever of them came first and permanently re-pin a conversation onto the wrong model. When a pin exists, `applyAffinity` moves that target to the *front* of the candidate list `candidatesForVirtual` already computed — ahead of reachability preference, not merged into it, so a 15s cache wobble can never hop an established conversation to a different backend. A pin naming a target no longer in the candidate list (removed from config) is silently ignored and normal ordering resumes. `routeVirtual` records (or refreshes) the pin only after an attempt actually succeeds *and* answers with a status below 500 — never on a failed attempt, and never on a 5xx (the backend answered, but pinning a 500 would lock the conversation onto the backend that just failed instead of leaving it able to fail over next turn; a 4xx still pins, since the backend itself is fine) — including re-pinning to a *new* target when the previously-pinned one fails pre-response and a later candidate serves instead; the conversation is already contaminated by that switch, so the fix is to pin forward rather than flap back next turn. The store is bounded exactly like `ProxyRegistry`'s reachability cache: a 1-hour TTL since last use and a 1024-entry LRU cap, both swept lazily on the write path — no background goroutine. No key present (no `prompt_cache_key`, no `user`) means no pin is ever looked up or recorded — identical to pre-affinity behavior.

A virtual name always appears in `/v1/models`, unlike an endpoint model that simply disappears when its probe goes offline — the point of a stable name is that it's always there to poll and dispatch against. `status.value` is `"loaded"` only when at least one candidate came from the "currently usable" pass (`freshCount > 0`); otherwise it's `"unloaded"` with `failed: true` and an `error` explaining why, so a client polling for readiness stops instead of spinning on a name that's really configured wrong. `architecture.input_modalities`, `meta` (`n_ctx`/`n_ctx_train`), and the top-level `context_length` are all inherited from the first attempt-order candidate — a managed alias's `ModelCatalog()` entry, or the matching `UpstreamModel` from the registry snapshot — falling back to `["text"]` with no `meta`/`context_length` when unknown, since a stale/offline endpoint candidate has no cached model list to read from. Never inferring `image` support here follows the same rule as the rest of the catalog: offering images to a server that can't take them fails mid-turn, which is worse than not offering.

Startup validation (`warnVirtualModelConfig` in `internal/app/app.go`, sibling to `warnAliasShadowing`) warns about virtual-model dead config: a virtual name shadowed by a managed alias (dispatch checks managers first), a name containing `/` (would intercept `endpoint/id` routing), a target with only one of `endpoint`/`model` set, a target naming an endpoint or alias that doesn't exist, a virtual with zero usable targets, and two virtuals sharing a name.

**Reasoning-effort rewrite** (`router.reasoningEffortMap` config, `RouterConfig` / `rewriteProxyBody` / `applyReasoningEffortMap`). Clients and backends do not agree on what "reasoning off" looks like on the wire. Measured against a real llama.cpp server (`/v1/chat/completions`, Qwen3.8-27B). This was `europa` before it was rebuilt on ExLlamaV3 (see the exl3 table below); it still describes the managed `llama-server` profile and any other llama.cpp backend:

| `reasoning_effort` sent | result |
|---|---|
| `none` | accepted, **zero reasoning returned** |
| `low` / `medium` / `high` / `xhigh` | accepted, reasoning returned |
| `minimal` | **500 server error**: `Unexpected reasoning effort minimal. Supported types are xhigh (default), medium, and low.` |

Oh My Pi has no wire value meaning "off" at all: its `--thinking off` clamps to the *lowest entry in the model's configured `efforts` list* and sends that verbatim. Captured twice against the same prompt, only the list changed: `efforts: [low, medium, xhigh]` → sends `"low"`; `efforts: [medium, xhigh]` → sends `"medium"`. So a client can be configured to send `minimal` (by giving it an `efforts` list whose lowest entry is `minimal`) but never `none` — there is no client-side way to ask for the value that actually works. When a client does send `minimal`, the 500 above sent one production client into a tight retry loop (150 identical requests observed from a single prompt).

`router.reasoningEffortMap` closes that gap by rewriting the value in flight: `{"minimal": "none"}` turns the lowest value a stuck client can be made to send into the value the backend actually treats as off. It's a router-level, opt-in body rewrite — absent or empty (the default) means zero behavior change, not even a JSON round-trip on the managed-alias path (see `rewriteProxyBody`'s short-circuit). Keys and values are free-form strings on purpose, not a fixed vocabulary: the table above is this backend's answer, not a general truth, and hardcoding one would just move the problem to the next backend that disagrees. Only a top-level string `reasoning_effort` field with an exact, case-sensitive key match is rewritten; a mapped value of `""` removes the key entirely rather than sending an empty string, since some backends reject that too. Applies uniformly to every proxied path — managed-alias, endpoint, and virtual-model routes all funnel through the same rewrite so a client hitting a bare managed alias gets the same fix as one hitting an endpoint.

That value swap fixes llama.cpp, which interprets `reasoning_effort` server-side. It does not fix oMLX. Measured directly against oMLX's source (`omlx/server.py:3594`): oMLX merges `request.reasoning_effort` verbatim into `chat_template_kwargs` and hands it to the model's Jinja chat template — there is no server-side meaning to rewrite. The MLX build of the measured model (`CodeFast`) uses the older Qwen convention, `enable_thinking`, not `reasoning_effort`, so the template never reads the field a value swap targets at all — no VALUE of `reasoning_effort` can turn its reasoning off; only a different field entirely can. Measured reasoning-output length against oMLX `CodeFast`:

| request | reasoning returned |
|---|---|
| baseline | 101 chars |
| `reasoning_effort: "none"` | 94 chars — no effect |
| `chat_template_kwargs: {"enable_thinking": false}` | **0 chars — off** |

And against llama.cpp (`europa`, when it still ran llama.cpp), that same `chat_template_kwargs: {"enable_thinking": false}` also yields 0 chars — so each backend tolerates the other's mechanism harmlessly (llama.cpp ignores an unrecognized `chat_template_kwargs` key; oMLX ignores `reasoning_effort` once its template doesn't reference it). `router.reasoningEffortTemplateKwargs` is the sibling knob this requires: `{"minimal": {"enable_thinking": false}}` merges that object into the body's top-level `chat_template_kwargs` (creating it if absent) whenever `reasoning_effort` matches a configured key, filling in only keys the client's own body doesn't already set — mirroring oMLX's own `merged.setdefault(...)` server-side, so an explicit client choice always wins over ours. Values are arbitrary JSON (bool, string, number, …), not just bools, since oMLX forwards whatever it's given straight to the template untyped. Configuring both knobs together is what makes "turn reasoning off" portable across both backends from one client-side value.

**Neither knob reaches ExLlamaV3.** `europa` was rebuilt on an exl3 server (`owned_by: "exl3"`, model id `qwen3.8-27b-exl3-4.0bpw`) and no longer speaks llama.cpp's dialect. It is not TabbyAPI either — no `/docs`, no `/openapi.json`, no `/v1/model`. Measured at `temperature: 0` on one prompt, so every row is directly comparable:

| request | reasoning returned |
|---|---|
| baseline | 157 chars |
| `reasoning_effort: "none"` | 157 chars |
| `reasoning_effort: "minimal"` | 157 chars |
| `reasoning_effort: "low"` | 157 chars |
| `chat_template_kwargs: {"enable_thinking": false}` | 157 chars |
| `template_vars: {"enable_thinking": false}` (TabbyAPI's spelling) | 157 chars |
| top-level `enable_thinking: false` / `thinking: false` | 157 chars |

Byte-identical across every variant: the server drops all of these fields before the chat template, so there is no wire mechanism to turn reasoning off. It always reasons, at whatever the model's own default is. Both knobs are therefore **inert against europa** — not broken, just ignored. One thing did improve: `minimal` returns 200 rather than the 500 above, so the tight retry loop that motivated `reasoningEffortMap` cannot happen on this backend anymore.

Keep both knobs configured regardless. `reasoningEffortTemplateKwargs` is still load-bearing for oMLX (re-measured after the rebuild: `enable_thinking: false` still gives 0 chars there), and `reasoningEffortMap` still matters for the managed `llama-server` profile. A rewrite a backend ignores costs one JSON round-trip and changes nothing.

Consequence for `vCode`: europa is its first-preference target, so a conversation pinned there by affinity gets reasoning that cannot be suppressed. That is a deliberate trade — exl3 is faster. Flip the target order if reasoning-off ever matters more than latency for that virtual.

Both knobs match against the request's **ORIGINAL** `reasoning_effort` value, captured before `reasoningEffortMap`'s swap runs — not after. This is load-bearing, not incidental: the two knobs describe *one* inbound client value triggering *two* independent rewrites. `{"minimal": "none"}` (the value map) and `{"minimal": {"enable_thinking": false}}` (the template-kwargs map) are both keyed on the client's actual `"minimal"`; matching post-swap would require the template-kwargs map's keys to track whatever the value map happens to rewrite `"minimal"` *into* (`"none"`) rather than what the client sent, coupling the two maps together for no reason and breaking silently the moment either is reconfigured on its own. `rewriteProxyBody` reads the field once via `reasoningEffortValue` before either rewrite mutates it, and both `applyReasoningEffortMap` and `applyReasoningEffortTemplateKwargs` share that single decode/encode pass — see `TestReasoningEffortTemplateKwargs_BothKnobsFireTogether`'s comment for the regression this ordering guards against.

**Anthropic Messages API compatibility** (`router.anthropic` config, `internal/router/router_anthropic.go` / `internal/router/router_anthropic_translate.go`). Lets the `claude` CLI (Claude Code) point at the router via `ANTHROPIC_BASE_URL` and get real Anthropic models transparently proxied through to `api.anthropic.com`, plus an optional config-driven redirect of specific model ids to a local managed/virtual/endpoint model. Absent `router.anthropic` section → `/v1/messages`, `/v1/messages/count_tokens`, and the `/api/` passthrough prefix all 404 — zero behavior change, same off-by-default shape as `reasoningEffortMap` above.

- **Passthrough** is byte-for-byte: headers (including the client's real OAuth bearer / API key, `anthropic-version`, `anthropic-beta`), body, and SSE streaming all forward to `upstream` (default `https://api.anthropic.com`) untouched, using `httputil.ReverseProxy`'s `Rewrite` hook — never `newUpstreamProxy`, which deliberately strips `Authorization` (correct for every other route here, wrong for this one: the inbound credential *is* the upstream credential). The `/api/` prefix passthrough is required, not optional — Claude Code's OAuth-login mode hits `GET /api/claude_cli/bootstrap` and `HEAD /api/hello` under a custom base URL, and both need to reach the real API for its own model picker/entitlements to behave normally.
- **`modelMap`** (free-form string keys, exact match against the request body's `model` field — same convention as `reasoningEffortMap`) redirects specific ids to a managed alias, virtual model name, or `endpoint/model` id. Resolved once at the top of `handleProxy` (`internal/router/router.go`), before the existing managed/virtual/endpoint dispatch, so a mapped key is a router-wide alias reachable from a plain OpenAI client too and gets a row in the ordinary `/v1/models` catalog. A redirected request is translated Anthropic→OpenAI (system field, typed content blocks, tool_use/tool_result, tool_choice — see `internal/router/router_anthropic_translate.go`'s header for exact scope and what's deliberately dropped: extended thinking, prompt caching, documents, structured outputs, server-side Anthropic tools) and re-enters `handleProxy` as a synthesized request carrying **no** client headers — the real Anthropic credential never reaches a local backend, by construction rather than by stripping it. The response is translated back OpenAI→Anthropic through a streaming state machine that streams text live and buffers tool-call blocks to emit complete at the end (Anthropic's content blocks are strictly sequential; OpenAI's aren't). A ping SSE event (`pingIntervalSeconds`, default 15) covers Claude Code's 300s idle-byte watchdog during a slow local generation — starting only once the backend's headers arrive, since nothing has reached the client before that to inject a ping into.
- **Claude Code's model-list visibility does not come from a spoofed `/v1/models`.** Measured against the installed CLI: gateway model discovery is off by default and is explicitly skipped for a subscription-OAuth login (the setup this was built against) even when enabled — any model id is accepted unvalidated under a non-first-party base URL regardless. To make a `modelMap` key selectable in Claude Code's own `/model` picker, set `ANTHROPIC_CUSTOM_MODEL_OPTION=<key>` (optionally `_NAME`/`_DESCRIPTION`) in the environment Claude Code runs in, or add a `modelPicker` entry to Claude Code's own settings — not a relayLLM-side change.
- **Context window mismatch**: mapping a real Claude id (e.g. `claude-haiku-4-5`) to a local model makes Claude Code assume a Claude-sized context window for it, so a smaller local model overflows sooner than expected. The router normalizes a backend's own overflow wording ("exceeds the available context size", "maximum context length is N") into the exact phrase Claude Code's auto-compact recognizes (`400 "prompt is too long: <original>"`), turning that into a compact instead of a dead session — but a key that doesn't start with `claude-` (e.g. `relay/coder`) avoids the mismatch entirely.
- Startup validation (`warnAnthropicModelMap`, main.go) warns about a `modelMap` key colliding with an existing managed alias or endpoint-prefixed id (it would silently shadow that route, since the map is resolved before every other dispatch check) and a target that resolves to nothing configured. `router.anthropic` or `router.passthrough` configured with any non-loopback `--router-bind` entry and no `--router-tls-cert` fails relayLLM startup outright. Passthrough forwards the client's real credential on every request, so one plaintext, off-box listener would leak it regardless of how many other configured binds are loopback.

**Credential passthrough** (`router.passthrough` config, `internal/router/router_passthrough.go`). Each entry mounts `/<name>/` on the router and forwards everything under it byte-for-byte to `upstream`: headers (the client's own `Authorization` included), body, SSE, and WebSocket upgrades. `/chatgpt/codex/responses` becomes `<upstream>/codex/responses`. relayLLM stores no credential, the same model as the Anthropic passthrough. It exists for OpenAI's two surfaces: ChatGPT-subscription OAuth clients (Oh My Pi's `openai-codex` provider, the Codex CLI) talk to `https://chatgpt.com/backend-api`, and API-key clients talk to `https://api.openai.com`. OMP points at it with `providers.openai-codex.baseUrl: http://127.0.0.1:8180/chatgpt` in `~/.omp/agent/models.yml`; OMP officially supports a streaming-proxy base URL and keeps its `/usage` calls direct to chatgpt.com. Routing is by path prefix, never by the body's `model` field: OpenAI's routes are the same paths the router serves for local models, and model dispatch would send a mistyped local model name, prompt included, to a cloud provider. There is no `modelMap`, since OMP and Codex already reach local models through their own provider config. Invalid entries (reserved names `v1`/`api`/`models`/`health`, a non-http(s) upstream, plain `http` to a non-loopback host) are logged and skipped. A WebSocket held open between turns reports `idle` on the dashboard, never `stalled`, and its frames are metered through `meteredResponseWriter.Hijack`.

**router.sock** (`internal/router/router_socket.go`, `internal/relay/launch.go`, `internal/relay/bridge_client.go`, `internal/app/model_host.go` — launched mode only; `plan-broker-and-sessions.md` §2 C9). When relay launches relayLLM, `Hello` (`internal/relay/launch.go`) captures relay's own identity off that same connection: on the *connecting* end of a Unix stream socket, `LOCAL_PEERTOKEN` names the *accepting* process, not the caller's own — confirmed empirically (spike SP1, `spikes/SP1.md`) rather than assumed, since it is the mirror image of the well-known accepting-side use for `LOCAL_PEERCRED`/Hello and isn't obvious without checking. Hello cross-checks that token's pid against the `Hello` OK reply's `relay_pid` and stores the resulting `(pid, pidversion)` (`relay.RelayIdentity()`); any failure to capture or a pid mismatch fails Hello closed, exactly like every other launch failure. `--router-socket` (default `{data-dir}/router.sock`, mode 0600, created under a temporarily restrictive umask so the file never briefly exists world-readable) then serves the SAME router on a Unix socket that admits only that one peer — every accepted connection's kernel audit token is checked against `RelayIdentity()` once, at accept (`RelayRouter.ListenSocket`'s `ConnContext`), not per request, comparing the FULL `(pid, pidversion)` pair — and refuses everyone else with `401 {"error":{"message":"unauthorized","type":"authentication_error"}}`. `internal/app`'s `ensureModelHostRouter` builds this router even when `--router-port` is unset (`StartRelayRouter` returned nil): router.sock must exist whenever relay launched this process, independent of whether the TCP listener is also configured. `RelayRouter.SocketHandler()` builds the socket's mux: identical to the TCP mux except `/api/` (the Claude Code OAuth bootstrap passthrough to `api.anthropic.com`) and every configured `router.passthrough` `/<name>/` route both 404 outright — those routes forward whatever credential the *caller* presented, but the caller here is relay's model broker acting on a project/service grant, never a holder of its own Anthropic/OpenAI credential. `/v1/messages` serves only `router.anthropic.modelMap` targets (404, Anthropic-shaped `not_found_error`, for anything else); `/v1/messages/count_tokens` has its OWN socket handler (`handleAnthropicCountTokensSocket`) that answers a mapped model with the same byte-based estimate `handleAnthropicCountTokens` uses, and 404s an unmapped one — it must never share `/v1/messages`'s redirect handler, which would silently turn a token-count probe into a real upstream generation call. Every proxied response — direct dispatch or the Anthropic-modelMap redirect alike — carries `X-Relay-Model-Target` (a bare managed alias, or `endpoint/upstreamID`; `router.go`'s `setModelTargetHeader` walks a chain of `Unwrap() http.ResponseWriter` wrappers so it reaches `translatingResponseWriter`'s real writer even through the Anthropic-redirect re-entrant dispatch, since that writer buffers headers separately and never copies them onto the real response on its own); `newUpstreamProxy` also strips any `X-Relay-*` header an upstream response tries to add, and any `Authorization`/`X-Api-Key`/`X-Relay-*` header on the OUTBOUND request, so neither a caller's credential nor a spoofed upstream signal can ride along. `/v1/models` rows with `owned_by: "anthropic-map"` carry a `"target"` field naming what the key resolves to. `internal/app`'s `registerWithRelay` always attempts `RegisterModelHost` (`internal/relay/bridge_client.go`; tokenless, JSON `service_id`/`router_socket`, an absolute path) when launched, REGARDLESS of whether `RegisterManifest` succeeded — the two are independent relay capabilities, and gating one on the other would silently strand the model broker on a merely transient manifest hiccup; a `RegisterModelHost` refusal itself still calls `os.Exit(78)` (`relay.RegisterModelHostOrExit`): relayLLM must never believe router.sock is relay's registered upstream when relay actually disagrees. `--router-port` continues to work unchanged alongside router.sock in this unit (P1); a later unit closes it in launched mode.

## Built-in Tools

`BuiltinToolRegistry` (`internal/tools/tools.go`) is the generic mechanism for in-process tools that run alongside MCP tools in the `BaseChatProvider` loop (`internal/provider/chat_base.go`) and need an `emit` progress callback MCP tools can't provide. Dispatch order in `runToolLoop()`: built-ins first (`builtinTools.Has()`), then MCP.

It currently **ships no tools**. Image generation is no longer a built-in — it is the `comfyui` MCP tool reached through relay like any other MCP (see relay ADR-006). relayLLM only *serves* the output directory: `GET /api/generated/:filename` returns images that the relay-comfyui MCP wrote to `{dataDir}/generated/`.

## API

Unix socket at `--socket` (defaults to `{data-dir}/relayllm.sock`). WebSocket at `/ws`.

Optionally also on TCP: `--http-port` / `RELAY_LLM_HTTP_PORT` (empty = disabled, the default) bound to `--http-bind` / `RELAY_LLM_HTTP_BIND` (default `127.0.0.1`; comma-separated to bind more than one interface, e.g. `127.0.0.1,192.168.64.1`), with `--http-tls-cert` / `--http-tls-key` optionally available for transport encryption. It carries no bearer token — anonymous, exactly like `--router-port`, protected only by which `--http-bind` addresses actually got bound — so it does **not** get the socket's full route table: `internal/api.TCPDiagnosticsOnly` (`tcp_diagnostics.go`) wraps it in a read-only allowlist (`GET /status`, `/status/status.css`, `/status/status.js`, `/api/status`, `/api/status/detailed`); every other route, including every mutating route and `/ws`, answers a plain 404 there, identical to an unregistered path. The Unix socket's handler is untouched and keeps the full table behind `bearerAuth`. It exists because the `/status` dashboard is a browser page and nothing else reaches it: relay's front door is a Unix socket only Eve dials, and Eve proxies only the specific `/api/*` + `/ws` routes its own code knows about. Each `--http-bind` address binds best-effort (`listenAll` in `internal/app/app.go`): one that can't be bound (e.g. a gateway IP not locally assignable in this deployment) is logged and skipped rather than taking every other bind down with it; only a total failure — every configured address rejected — is fatal.

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

The grouped `WSMsg*` constants in `internal/events/ws_messages.go` are the authoritative message
set (the lists above are the common subset); `llm_event` payloads follow the
canonical contract in [`docs/event-protocol.md`](docs/event-protocol.md).

## Terminal Sessions

PTY-backed terminal sessions hosted by relayLLM. Eve proxies terminal I/O via WebSocket (base64-encoded). Terminals survive Eve restarts.

- **Templates**: live in the `pty` section of `settings.json` (relay's config editor manages them; the API is read-only). Three protected built-ins: `claude-code`, `opencode`, `shell`. `IdleTimeout` field (minutes, default 1440 = 24h).
- **Idle timeout**: When all viewers disconnect, an idle timer starts. If no viewer reconnects before it fires, the terminal is auto-closed. Configurable per template.
- **Color**: PTY spawned with `TERM=xterm-256color` and `COLORTERM=truecolor` for full 24-bit color.
- **Scrollback**: 100KB in-memory ring buffer per terminal, replayed on reconnect.
- **On-disk log**: Each session's raw byte stream is also teed to `{dataDir}/terminal_logs/{id}.head.log` (first 64KB) + `{id}.tail.log` (rolling, capped at ~960KB → 1MB total). Files survive the session being evicted from memory. The ANSI-aware "head + tail" split preserves the stream's initial mode-setting (cursor home, SGR resets) so xterm replay renders correctly even when the middle was truncated. Used by `GET /api/terminals/{id}/log`. A daily sweeper deletes files older than 30 days; bounded by a 500MB total cap as a safety net.
- **SSH hosts**: a terminal whose project resolves to a host (`TerminalManager.Create` → `resolveTerminalHost`) execs `ssh_argv + ["-tt", "--", <remote>]` under the local pty instead of a local command — see [`../relay/docs/ssh-hosts.md`](../relay/docs/ssh-hosts.md) for the wire contract. `Host` is resolved once at create time (never refreshed mid-life, unlike a chat session's). No relay-managed substitution, project token, or pi overlay applies. `terminal_created`/`terminal_joined`/both terminal-list shapes carry a `{id, name}` host chip.
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
    "router": {
      "reasoningEffortMap": {"minimal": "none"},
      "reasoningEffortTemplateKwargs": {"minimal": {"enable_thinking": false}},
      "anthropic": {
        "modelMap": {"relay/coder": "host/Qwen3.8 27B Code xhigh"},
        "pingIntervalSeconds": 15
      },
      "passthrough": {
        "chatgpt": {"upstream": "https://chatgpt.com/backend-api"},
        "openai": {"upstream": "https://api.openai.com"}
      }
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
  **`router`** (optional; empty/absent means no rewriting, byte-identical to a settings.json with no `router` section at all): `reasoningEffortMap` rewrites a top-level string `reasoning_effort` field on every proxied request body — see the Relay-router section above for why. Free-form string keys/values, not a fixed vocabulary: map a value to `""` to remove the field entirely instead of sending it as an empty string. `reasoningEffortTemplateKwargs` is its sibling knob for backends (oMLX) that forward `reasoning_effort` straight into the chat template instead of interpreting it server-side: it merges an object into the body's top-level `chat_template_kwargs` when the request's *original* `reasoning_effort` value (matched before `reasoningEffortMap` rewrites it) matches a configured key, without overwriting any key the client's own body already sets there. Also empty/absent by default; see the Relay-router section above for the measured table and why both knobs match against the original value.

  **Endpoint TLS** (optional, per entry in `openai.endpoints`; see `internal/config/endpoint_tls.go`'s file-level comment and `normalizePin`'s doc comment for the fingerprint-vs-SPKI reasoning): `caFile` (a PEM bundle, becoming the *only* trust anchor for that endpoint) and `pinSHA256` (SHA-256 hex fingerprints of the DER leaf certificate, colons/case ignored) pin the relayLLM-to-upstream hop, validated at config load. A non-loopback `http` `baseURL` fails to load unless top-level `"allowPlaintextEndpoints": true` acknowledges the hop is unencrypted (logged per endpoint; never affects `https` verification — no skip-verify knob exists). The router's own `--router-port` listener gets TLS via `--router-tls-cert`/`--router-tls-key` (env `RELAY_LLM_ROUTER_TLS_CERT`/`RELAY_LLM_ROUTER_TLS_KEY`); both or neither must be set.

  **Memory budget** (optional, per managed-server section; all default to off so behavior is unchanged until set — see the comment above `fitsLocked` in `internal/servermanager/server_manager.go` for why this is built here rather than delegated to llama.cpp's own router mode): `maxLoaded` caps concurrent instances, `maxMemoryGB` caps the sum of estimated resident memory, `idleTimeoutMinutes` reclaims instances nobody is using. Either cap evicts the least-recently-used *idle* instance; a leased instance (mid-turn) is never evicted — when everything is busy, admission waits up to `admissionTimeoutSeconds` (default 120) and then errors naming the busy aliases. Model sizes are computed, not declared: weights from the file size, KV cache from the GGUF header (`internal/servermanager/gguf.go`) honoring sliding-window attention and per-layer GQA, plus `memoryHeadroomPercent` (default 10) for compute buffers. Per-model `memoryGB` overrides the estimate; a model whose size can't be determined counts against `maxLoaded` but not `maxMemoryGB`. Current usage is reported in `/api/status` under `budgets`, and per-instance `leases` / `estimatedGB` / `idleSeconds` in `instances`.

  Each llama-server / mlx-serve model key except `alias` (and the budget-only `memoryGB`) maps 1:1 to a `--{key}` CLI flag. Value translation: `true` → `--key`, `false` → omit, number → `--key value`, string → `--key value`. Optional `port` per model overrides auto-allocation. `modelDir` (supports `~`) is prepended to relative `model` paths (for mlx-serve, `model` is an MLX model *directory*). `--openai-config` flag overrides the `openai` section. The `pi` section is optional: `binaryPath` (supports `~`) takes priority over the well-known fallback chain in `resolvePiPath` (`~/.local/bin/pi`, npm globals, `/opt/homebrew/bin/pi`, `/usr/local/bin/pi`, then `$PATH`); `extraArgs` are appended verbatim to every `pi --mode rpc` spawn (e.g. force-skip context files, add `--extension`). The relay-managed fields mirror the PTY `pidev` template's shape and route through the shared `RelayManagedSpec.Resolve()` helper in `internal/spawn/spawn.go`: `useRelayToken` (or, for terminals, a non-empty `projectID`) injects a project-scoped `RELAY_PROJECT_TOKEN` env var, resolved just-in-time from relay's bridge — a child never holds more than that project token; `env_passthrough` copies the listed env var keys from `os.Environ()` into the spawned pi. Skill *generation* is owned by relay entirely (see relay ADR-004); relayLLM never regenerates SKILL.md. Skills load from the convention `<project>/.claude/skills`: the pi `--mode rpc` provider auto-appends `--skill <project>/.claude/skills` (skipped if `extraArgs` already contains `--skill`), and PTY templates reference `${PROJECT_PATH}/.claude/skills` in their args (e.g. the `pidev` template: `"args": ["--skill", "${PROJECT_PATH}/.claude/skills"]`).

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

Default tier covers WS protocol, HTTP API, session lifecycle, tool-call loop, pi event translation, and manifest registration — all driven by fakes in `internal/testutil` / `internal/api/testserver_test.go`. No real LLM, no subprocess, no network.

The `llm` tier (`internal/api/provider_llama_live_test.go`) drives the real `LlamaServerManager` against an installed model. **Prerequisite**: `Qwen3.6 MoE 35` registered in `~/Library/Application Support/relayLLM/settings.json` under `llama-server.models` with the model file present at the configured `modelDir`. Skips gracefully if absent. Validates SSE chunking, llama-server lifecycle, and mid-stream stop — the surface fakes can't reach.

The `live` tier is for legacy integration tests that depend on third-party services. Kept for manual smoke; never run in default CI.

### Pre-commit hook

Install once per clone:

```bash
git config core.hooksPath .githooks
```

Runs `go build ./...`, `go vet ./...`, and the hermetic test suite under the race detector (`go test -race ./...`) on every commit that touches Go files. ~3s total warm (~+1s for `-race`; a one-time ~+4s when the instrumented build cache is cold). Skip in emergencies with `git commit --no-verify`. The `live` and `llm` build tags are not invoked by the hook — those stay opt-in.

### Adding tests

- Need an HTTP/WS surface to drive? Use `NewTestServer(t, nil)` from `internal/api/testserver_test.go`.
- Need a scripted LLM stream? `srv.SetFakeProvider()` then `fp.ScriptText(...)` / `fp.ScriptResult(...)`.
- Need deterministic timing? `NewFakeClock(t0)` + `clock.Advance(d)`. Wire via `TestServerOptions{Clock: ...}`.
- Need fake tool calls? `NewFakeMCPClient(FakeTool{Name, Handler})`.
- Need relay bridge features in a test? `NewFakeBridge(t)` + `LaunchViaBridge(t, fb, serviceID)` (real pipe, real Hello). `WithBridgeEnv` sets the env without launching, i.e. standalone.

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
3. Document a non-obvious architectural decision (new test seam, new protocol, new build-tag tier, new run mode) as a comment at the point of the code it constrains — the hidden invariant, the rejected alternative, the measured tradeoff. Skip for routine refactors and library upgrades. There is no separate decisions log to update; a comment that lives next to the code it explains can't drift out of sync with it the way a standalone doc can.
4. Reflect the new state in [`ROADMAP.md`](ROADMAP.md) — close shipped items into the **Closed** section with a one-line note, update **In flight**, add follow-ups you discovered.

**Breaking changes** — anything that alters the wire (WS protocol message shapes, manifest schema, HTTP route paths or payloads, terminal template fields, session settings keys) ships as a **coordinated PR across repos**. Land relayLLM and the matching changes in `../relay`, `../eve`, `../relayScheduler` together. There is no manifest version negotiation or compat shim; one person owns all the repos, so coordinate at PR time rather than building permanent backwards-compat. If a change genuinely cannot be coordinated atomically, ship the additive side first (new field / new route) and migrate consumers in a follow-up PR before removing the old surface.

## Service Manifest Integration

relayLLM detects its run mode from `RELAY_LAUNCH_FD`:

- **Standalone** (env unset): binds its own listener (`--socket`, default `{data-dir}/relayllm.sock`), auto-generates a bearer token if `--token`/`RELAY_LLM_TOKEN` is unset, serves direct HTTP/WS clients. No bridge call is ever made.
- **Enhanced** (env set): the first thing `app.Main` does — before anything can spawn a child — is `relay.BootstrapLaunch`: read the 64-hex launch secret from that fd to EOF, close it, unset `RELAY_LAUNCH_FD`, and send `Hello` (`name` = `RELAY_SERVICE_ID`) over `RELAY_BRIDGE_SOCKET`. Relay binds this process's peer audit token as the service identity, and `Hello` also captures *relay's* own peer audit token off that connection (see the router.sock paragraph above) for router.sock's later admission check. Any failure exits non-zero — never a silent fall back to standalone. After that, same listener + same wire language, plus a `RegisterManifest` declaring routes, status endpoint, and actions; relay's dispatcher forwards matching front-door requests over the internal socket using the bearer token relayLLM declared in the manifest. Once that succeeds, `RegisterModelHost` registers `--router-socket` as relay's model-broker upstream — a refusal there is fatal (`os.Exit(78)`), unlike a `RegisterManifest` failure.

`relay.Launched()` (Hello succeeded) is the only "relay bridge available" signal. Every bridge request carries an **empty token**; relay authenticates it by the peer identity. relayLLM holds no relay credential in its environment: `RELAY_SERVICE_TOKEN`, `RELAY_MCP_TOKEN` and `RELAY_FRONTEND_TOKEN` are never read, and are scrubbed from its own env and every child's. Contract: `../spec-launch-identity.md`.

The mode switch is a deployment fact, not a code fork — one config loader, two sources. Both `internal/relay/manifest.go` (what relayLLM exposes) and `internal/relay/bridge_client.go` (how it talks to relay) are small and self-contained.

See `../relay/docs/service-manifest.md` for the full protocol contract.

## Local Auth

`auth.go::HookScopedBearerAuth` validates every request against `--token` / `RELAY_LLM_TOKEN`, but only on the Unix socket (`--socket`). Empty token + standalone mode → auto-generated 64-char hex (not logged for security; set the env var to pin). Token comparison is constant-time via `crypto/subtle.ConstantTimeCompare`. The one carrier accepted is `Authorization: Bearer <token>` — every caller of the socket is a machine client (relay's dispatcher, the permission hook, a direct socket client), never a browser, so there's no cookie or query-param fallback to bootstrap.

The permission hook never holds this internal bearer — it would let any same-user process read it out of the hook's own environment (`KERN_PROCARGS2`) and drive every relayLLM route, not just permission decisions. Instead, `permission.PermissionManager.MintHookToken` generates a random 32-byte per-session credential when `ClaudeProvider` is constructed (`NewClaudeProvider`, `internal/provider/claude.go`); only its SHA-256 hash is kept, bound to the session id. `buildClaudeEnv` puts the plaintext in the hook child's env under the same `RELAY_LLM_HOOK_TOKEN` name as before, and the hook binary is unchanged — it still just forwards whatever that variable holds as its bearer. `HookScopedBearerAuth` accepts this credential in place of the internal bearer on exactly one route, `POST /api/permission`; every other route, and the `/ws` upgrade, reject it exactly like a wrong bearer (401). `RegisterPermissionRoutes`'s handler additionally checks the token's bound session id against the request body's `sessionId` (403 on mismatch), since a validated token only proves "bound to some session" — the route-level check can't see which one the caller claims. `ClaudeProvider.Kill()` revokes the token (`PermissionManager.RevokeHookToken`) whenever the session ends, is deleted, or its provider is replaced on resume, and the whole table lives only in memory, so a relayLLM restart revokes every outstanding token for free.

`--http-port`'s TCP front (see "Optionally also on TCP" above) is **not** wrapped in `bearerAuth` at all — it's anonymous, gated only by `--http-bind`, the same posture `--router-port` has always had. An `Authorization` header sent to it is simply never inspected. Because it carries no credential, it is additionally restricted to a read-only diagnostics allowlist (`TCPDiagnosticsOnly`, `internal/api/tcp_diagnostics.go`) rather than serving the socket's full route table — see "Optionally also on TCP" above for the exact allowlist.

### Relay-side token rotation

The `RELAY_PROJECT_TOKEN` env var carried into chat-provider MCP spawns, pi sessions, and project-scoped terminals is a *relay project token*, not relayLLM's local bearer. **relayLLM never stores it and never receives it from eve** — it resolves the token just-in-time from relay's bridge by `projectId` at every spawn (`resolveProjectToken` → `ResolvePtyEnv`), injects it, and discards it. This makes rotation transparent (the next spawn picks up the new token via `RotateProjectToken`) and means a relayLLM restart can't lose it (there's nothing stored to lose — the old `Session.McpToken` field is gone). If a project token can't be resolved, or relay did not launch this process, the child gets no token (fail closed). relayLLM has no service token to substitute: its own bridge calls are authenticated by launch identity. See `../relay/docs/decisions/007-project-token-brokering.md` and `../relay/docs/tokens.md`.
