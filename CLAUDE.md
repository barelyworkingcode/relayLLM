# relayLLM (Go)

Standalone LLM engine service. Manages providers (Claude CLI, pi.dev CLI, Ollama HTTP, OpenAI-compatible HTTP, llama.cpp managed processes), sessions, projects, permissions, and terminal sessions (PTY). Runs independently or as a relay-managed service.

## Architecture

```
main.go              Entry point, flag parsing, server wiring
project.go           Project CRUD + JSON file storage
session.go           Session lifecycle management
session_store.go     Session persistence to disk
provider.go          Provider interface + shared types + extractTextContent
provider_claude.go   Claude CLI provider (stream-json, persistent process)
provider_pi.go       pi.dev CLI provider (--mode rpc, JSONL → canonical Claude stream-json)
pi_models.go         pi model discovery via `pi --list-models` (cached)
provider_ollama.go   Ollama HTTP provider (NDJSON streaming)
provider_openai.go   OpenAI-compatible HTTP provider (SSE streaming)
provider_chat_base.go  Base provider: tool-calling loop, MCP + built-in tool dispatch
provider_settings.go Per-provider settings schema for Eve UI
response_collector.go  Headless response accumulation for HTTP clients
config.go            Unified config loader (settings.json → OpenAI + llama-server configs)
llama_manager.go     llama-server process manager (launch, health check, port allocation)
llama_proxy.go       OpenAI-compatible reverse proxy routing to llama-server instances
comfyui_client.go    ComfyUI HTTP client (queue, poll, fetch, workflow builder)
builtin_tools.go     Built-in tool registry (generate_image) + dynamic schema
terminal_template.go Terminal template types + JSON file store (built-in + custom)
terminal_session.go  Terminal session with PTY management (creack/pty)
relay_spawn.go       Shared relay-managed spawn prep (skill regen + token + ${SUB} expansion) used by both PTY and pi LLM
terminal_manager.go  Terminal CRUD + lifecycle management
terminal_log.go      On-disk head/tail log files for PTY replay after eviction
api.go               HTTP routes (projects, sessions, terminals, permissions, generated images)
ws.go                WebSocket server (streaming events to Eve, terminal I/O)
permission.go        Permission request/response tracking
scheduler_client.go  HTTP client for relayScheduler
scheduler_proxy.go   Task API proxy routes (GET/POST/PUT/DELETE /api/tasks/*)
scheduler_ws.go      WebSocket forwarding of scheduler events (task_started, etc.)
cmd/hook/            Compiled PreToolUse hook binary
```

## Providers

- **Claude**: Persistent process. `claude --print --output-format stream-json --input-format stream-json --verbose --model <model>`. Resumes via `--resume <sessionId>`. Headless sessions add `--dangerously-skip-permissions --permission-mode bypassPermissions` and set `RELAY_LLM_HEADLESS=true` env var (hook auto-approves).
- **pi (pi.dev coding agent)**: Persistent process spawned as `pi --mode rpc --provider <upstream> --model <id> --session-dir {dataDir}/pi-sessions [--thinking <level>] [--session <piSessionId>]`. Model identifier convention: `pi/<provider>/<modelId>` (e.g. `pi/anthropic/claude-sonnet-4-20250514`). Resume via `--session <piSessionId>` — pi owns its session JSONL under `{dataDir}/pi-sessions/`. No PreToolUse hook: pi runs in its no-permission-popups default and auto-executes tools; `CapabilitiesForProvider("pi")` therefore omits `SupportsPermissions`. Auth (API keys, OAuth tokens) is inherited from the user's environment / `~/.pi/agent/auth.json`. `PI_OFFLINE=1` + `PI_SKIP_VERSION_CHECK=1` are set at spawn to suppress pi's startup network calls. Pi's RPC event stream (`message_update` with `assistantMessageEvent`, `tool_execution_*`, `agent_end`) is canonicalized into the Claude `content_block_start`/`delta`/`stop` + `result` envelope inside `provider_pi.go:translate`, so Eve renders pi sessions with the existing Claude renderer. Mid-session capabilities: `PUT /api/sessions/:id/model` (sends `set_model` RPC), `PUT /api/sessions/:id/thinking-level` (sends `set_thinking_level`). `StopGeneration` uses pi's in-band `abort` RPC instead of process kill, so the subprocess survives stop/resume cycles. Per-session settings: `thinkingLevel` (off/minimal/low/medium/high/xhigh).
- **Ollama**: HTTP client with NDJSON streaming. Base URL via `--ollama-url` / `OLLAMA_URL` (default `http://localhost:11434`). Sends full conversation history per request; relies on Ollama's automatic KV cache prefix reuse. Per-session settings: `temperature`, `top_p`, `top_k`, `min_p`, `think` (bool), `num_ctx`. Explicitly sends `think: false` to suppress built-in reasoning on thinking models (e.g. Gemma 4). Supports image attachments via base64.
- **OpenAI-compatible**: HTTP client with SSE streaming. Configured via `settings.json` `openai` section (or legacy `openai_endpoints.json` / `OPENAI_BASE_URL`/`OPENAI_API_KEY`). Model selection: `prefix/model-id` (e.g. `omlx/Qwen3.5-27B`). Supports tool calling.
- **llama.cpp**: Managed llama-server processes via `LlamaServerManager`. Configured via `settings.json` `llama-server` section (or legacy `llama_models.json`). Model selection: `llama/{alias}` (e.g. `llama/qwen3-8b`). Launches llama-server on demand with configured GGUF model and flags, reuses running instances across sessions. Communicates via OpenAI-compatible API (reuses `OpenAIChatTransport`). Binary path: `--llama-server-path` / `LLAMA_SERVER_PATH` / config `binaryPath` / `llama-server` on PATH. Config keys in each model entry map 1:1 to llama-server CLI flags (except `alias` which is the routing name). Per-model locking: launches of different models proceed concurrently; concurrent requests for the same model wait on a shared `ready` channel. Per-session settings: same as OpenAI (temperature, top_p, top_k, min_p, etc.) — override server-level defaults set in the config.
  - **Proxy** (`llama_proxy.go`): Optional OpenAI-compatible reverse proxy (`--llama-proxy-port` / `LLAMA_PROXY_PORT`). Listens on a single port, reads the `model` field from the request body, launches/reuses the right llama-server via `GetOrLaunch()`, and proxies the full request with `httputil.ReverseProxy` (SSE streaming via `FlushInterval: -1`). Endpoints: `GET /v1/models` (list configured aliases), `GET /health`, all other requests routed by model name. External clients use the alias directly (e.g. `"model": "qwen3-8b"`) without the `llama/` prefix.

## Built-in Tools

Built-in tools coexist with MCP tools in the `BaseChatProvider` tool loop (`provider_chat_base.go`). Unlike MCP tools, built-in tool handlers receive an `emit` callback for progress events during long-running operations.

Tool dispatch order in `runToolLoop()`: built-in tools are checked first (`builtinTools.Has()`), then MCP (`mcpManager.CallTool()`). Tool definitions from both sources are merged into a single list sent to the LLM.

- **`generate_image`**: Text-to-image via ComfyUI (`--comfyui-url` / `COMFYUI_URL`). Queues a workflow, polls for completion with progress events, fetches the output image, saves to `{dataDir}/generated/`, returns a URL. Supports checkpoint selection, LoRA style adapters, and sampler/scheduler tuning. Available checkpoints and LoRAs are discovered from ComfyUI at startup and exposed as `enum` values in the tool schema. Only available to Ollama/OpenAI sessions (Claude provider has its own tool mechanism).

## API

HTTP on `--port` (default 3001). WebSocket at `/ws`.

### HTTP Endpoints
```
GET/POST       /api/projects       — list/create projects
GET/PUT/DELETE /api/projects/:id   — get/update/delete project
GET            /api/models         — list available models (Claude + Ollama + OpenAI endpoints + llama.cpp)
GET/POST       /api/sessions       — list/create sessions
POST           /api/sessions/:id/message — send message (sync, for HTTP clients)
DELETE         /api/sessions/:id   — end session
PUT            /api/sessions/:id/model           — pi only: mid-session model switch
PUT            /api/sessions/:id/thinking-level  — pi only: mid-session reasoning depth
GET/POST       /api/terminal/templates     — list/create terminal templates
GET/PUT/DELETE /api/terminal/templates/:id — get/update/delete custom template
GET/POST       /api/terminals              — list/create terminal instances (POST accepts extraArgs to append per-task argv)
DELETE         /api/terminals/:id          — close terminal
GET            /api/terminals/:id/log      — stitched head+tail of PTY's raw byte stream (works after session eviction)
POST           /api/permission     — hook binary posts here, held open until user decides
GET            /api/generated/:filename — serve generated images (ComfyUI output)
GET/POST       /api/tasks          — list/create tasks (proxy to relayScheduler)
GET/PUT/DELETE /api/tasks/:id      — get/update/delete task (proxy to relayScheduler)
POST           /api/tasks/:id/run  — trigger task execution (proxy to relayScheduler)
GET            /api/tasks/:id/history — task execution history (proxy to relayScheduler)
GET            /api/tasks/project/:projectId — tasks by project (proxy to relayScheduler)
```

### WebSocket Protocol
```
Client → Server: join_session, send_message, end_session, permission_response
Server → Client: session_joined, llm_event, stats_update, message_complete, permission_request, task_started, task_completed, task_error, task_status, error

Terminal messages:
Client → Server: terminal_create, join_terminal, leave_terminal, terminal_input (base64), terminal_resize, terminal_close, terminal_list, terminal_reconnect, terminal_templates
Server → Client: terminal_created, terminal_joined (with base64 scrollback), terminal_output (base64), terminal_exit, terminal_closed, terminal_list, terminal_templates
```

## Terminal Sessions

PTY-backed terminal sessions hosted by relayLLM. Eve proxies terminal I/O via WebSocket (base64-encoded). Terminals survive Eve restarts.

- **Templates**: Built-in (Claude Code, OpenCode, Shell) + custom via API. `IdleTimeout` field (minutes, default 1440 = 24h).
- **Idle timeout**: When all viewers disconnect, an idle timer starts. If no viewer reconnects before it fires, the terminal is auto-closed. Configurable per template.
- **Color**: PTY spawned with `TERM=xterm-256color` and `COLORTERM=truecolor` for full 24-bit color.
- **Scrollback**: 100KB in-memory ring buffer per terminal, replayed on reconnect.
- **On-disk log**: Each session's raw byte stream is also teed to `{dataDir}/terminal_logs/{id}.head.log` (first 64KB) + `{id}.tail.log` (rolling, capped at ~960KB → 1MB total). Files survive the session being evicted from memory. The ANSI-aware "head + tail" split preserves the stream's initial mode-setting (cursor home, SGR resets) so xterm replay renders correctly even when the middle was truncated. Used by `GET /api/terminals/{id}/log`. A daily sweeper deletes files older than 30 days; bounded by a 500MB total cap as a safety net.
- **Per-task `extraArgs`**: `POST /api/terminals` accepts `extraArgs []string` that are appended to the template's argv after `${PROJECT_PATH}` / `${RELAY_TOKEN}` / `${SKILL_PATH}` substitution. Same substitution applies to extras. Used by relayScheduler to schedule shell-style tasks against a shared template.

## Data

Default: `os.UserConfigDir()/relayLLM` — on macOS `~/Library/Application Support/relayLLM/`, on Linux `~/.config/relayLLM/`. Override: `--data-dir` or `RELAY_LLM_DATA`.
- `projects.json` — project definitions
- `sessions/` — per-session JSON files. A daily sweeper deletes files where `headless: true` and mtime is older than 7 days; non-headless (Eve-owned) sessions are never touched.
- `pi-sessions/` — pi.dev session JSONLs (one per pi session, owned by pi via `--session-dir`). Daily sweeper deletes files whose `piSessionId` is no longer referenced by any `sessions/*.json` (with a 1h minAge cushion to avoid racing live pi processes).
- `terminals/templates.json` — custom terminal templates
- `settings.json` — unified provider config (preferred). Falls back to separate `openai_endpoints.json` + `llama_models.json` if absent, then `OPENAI_BASE_URL`/`OPENAI_API_KEY` env vars:
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
      "models": [{
        "alias": "qwen3-8b",
        "model": "/models/Qwen3-8B-Q4_K_M.gguf",
        "ctx-size": 131072, "n-gpu-layers": -1, "threads": 8,
        "flash-attn": true, "kv-unified": true,
        "cache-type-k": "q8_0", "cache-type-v": "q8_0",
        "temp": 0.6, "top-p": 0.95, "top-k": 20, "min-p": 0.0
      }]
    },
    "pi": {
      "binaryPath": "~/.npm-global/bin/pi",
      "extraArgs": ["--no-context-files"],
      "useRelayToken": true,
      "autoRegenSkills": "always",
      "skillPath": "${project.path}/.claude/skills/relay",
      "env_passthrough": ["ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"],
      "projectOverlay": {
        "mode": "always",
        "defaultProvider": "relay-llama",
        "defaultModel": "qwen3-8b",
        "defaultThinking": "medium"
      }
    }
  }
  ```
  Each llama-server model key except `alias` maps 1:1 to a `--{key}` CLI flag. Value translation: `true` → `--key`, `false` → omit, number → `--key value`, string → `--key value`. Optional `port` per model overrides auto-allocation. `modelDir` (supports `~`) is prepended to relative `model` paths. `--openai-config` flag overrides the `openai` section. The `pi` section is optional: `binaryPath` (supports `~`) takes priority over the well-known fallback chain in `resolvePiPath` (`~/.local/bin/pi`, npm globals, `/opt/homebrew/bin/pi`, `/usr/local/bin/pi`, then `$PATH`); `extraArgs` are appended verbatim to every `pi --mode rpc` spawn (e.g. force-skip context files, add `--extension`). The relay-managed fields mirror the PTY `pidev` template's shape and route through the shared `RelayManagedSpec.Resolve()` helper in `relay_spawn.go`: `useRelayToken` injects a project-scoped `RELAY_TOKEN` env var; `autoRegenSkills` (`"always"`/`"skipIfExists"`/`"never"`) regenerates the project skill via relay's bridge; `skillPath` (supports `${project.path}`) tells relay where to write SKILL.md; `env_passthrough` copies the listed env var keys from `os.Environ()` into the spawned pi. When `skillPath` resolves to a real path, relayLLM auto-appends `--skill <path>` to argv (skipped if `extraArgs` already contains `--skill`). Fail-closed: `autoRegenSkills != "never"` with empty `skillPath` errors before spawn.

  **`projectOverlay`** (optional) writes a per-project `<projectDir>/.pi/` directory before each pi spawn (both `--mode rpc` and PTY `pi` templates) and sets `PI_CODING_AGENT_DIR` so pi reads from it. Pi's global `~/.pi/agent/` is never written to. Materialized files: `models.json` containing relayLLM's curated providers (`relay-llama` pointing at the `--llama-proxy-port` proxy + one entry per configured `openai.endpoints[]`); `settings.json` with `defaultProvider`/`defaultModel`/`defaultThinkingLevel` and a `skills` array (project `.claude/skills/` + `extraSkillDirs`); `auth.json` symlinked to `~/.pi/agent/auth.json` so credentials stay centrally managed (OAuth refresh writes through). Modes: `"never"` (default — feature off), `"always"` (rewrite on every spawn), `"skipIfExists"` (write missing files only). User's global `models.json` providers and `settings.json` keys are merged underneath by default (turn off via `excludeUserProviders`/`excludeUserSettings`). Set `authStrategy: "none"` if pi credentials are managed out-of-band. `gitignore: true` opt-in appends the overlay dir to the project's `.gitignore`. Fails closed at spawn if global `auth.json` is missing while symlink strategy is active — run `pi auth login` once globally first.
- `generated/` — images produced by the generate_image tool (served via `/api/generated/`)

## Build

```bash
go build .                          # main binary
go build ./cmd/hook                 # permission hook binary
```

## Ecosystem

relayLLM is the LLM engine for the Relay ecosystem. It proxies task API and WebSocket events from relayScheduler. Eve connects only to relayLLM as its single backend for all operations including tasks.

- `../relay/` -- MCP orchestrator. Manages relayLLM as a background service.
- `../eve/` -- Browser-based LLM frontend. Single-backend client to relayLLM for all operations including tasks.
- `../relayScheduler/` -- Task scheduler. Runs LLM prompts on schedule. relayLLM proxies its task API and forwards its WebSocket events.
- `../relayTelegram/` -- Telegram bot. Bridges messages to relayLLM sessions.
- `../relayComfy/` -- ComfyUI service. Manages ComfyUI as a subprocess for image/video generation. relayLLM talks to its HTTP API on port 8188.

## Eve ↔ relayLLM Channel Security

relayLLM authenticates every inbound connection (HTTP and WebSocket) with a bearer token and optionally serves through a `0600` Unix domain socket. This ensures Eve is the only client that can reach relayLLM:

**New listener (Unix socket mode — preferred).** When `RELAY_LLM_SOCKET` is set in the environment (injected by the `relay` orchestrator at spawn time), relayLLM binds a Unix domain socket at that path with mode `0600` in addition to (or instead of) its TCP listener. Both HTTP routes and the `/ws` WebSocket upgrade are served from this socket. The orchestrator unlinks the socket on graceful shutdown.

**Bearer token.** `RELAY_LLM_TOKEN` is read from the environment at startup and compared (constant-time) against the `Authorization: Bearer <token>` header on **every** HTTP request and on the WebSocket upgrade. Missing or mismatched tokens return `401` and the WS upgrade is rejected before protocol-switching — no half-open session allocation. This validation runs in both socket mode (as defense-in-depth on top of FS permissions) and TCP mode.

**TCP mode** (split-host fallback only): the TCP listener must be wrapped in TLS (operator provides cert + key), and the same bearer-token check applies. Plain `http://` off loopback must be refused at startup with a clear error.

**Scope.** `RELAY_LLM_TOKEN` is **not** the same token as `RELAY_MCP_TOKEN`. The MCP bridge socket (`relay.sock`) and the Eve↔relayLLM channel are separate trust boundaries and must not share credentials — multiplexing them means leaking either token grants access to both.

**Implementation:**
- `auth.go` — `bearerAuth(token, next)` middleware uses `crypto/subtle.ConstantTimeCompare`. No-op pass-through when token is empty (dev mode, with a startup warning).
- `main.go` — reads `--token` / `RELAY_LLM_TOKEN` and `--socket` / `RELAY_LLM_SOCKET`. Wraps the mux with `bearerAuth(recoverMiddleware(mux))` so HTTP and the `/ws` upgrade share the same auth check (unauthenticated WS upgrades are rejected with 401 before protocol-switching). When `RELAY_LLM_SOCKET` is set, creates the parent directory `0o700`, binds `net.Listen("unix", path)`, `os.Chmod(path, 0o600)`, and serves via `server.Serve(ln)` alongside the TCP listener. Socket is unlinked on SIGTERM/SIGINT and post-`ListenAndServe`.
- `session.go`, `provider_claude.go`, `cmd/hook/main.go` — token is plumbed through `SessionManager` → `ClaudeProvider` → hook child env as `RELAY_LLM_HOOK_TOKEN`. The hook binary reads it and adds `Authorization: Bearer <token>` to its `/api/permission` POST.

See `../relay/CLAUDE.md` ("Eve ↔ relayLLM internal channel") for the orchestrator side of the contract and `../eve/plans/cozy-honking-toast.md` for the full design rationale and end-to-end verification results.
