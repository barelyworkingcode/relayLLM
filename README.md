# relayLLM

Standalone LLM engine service. Manages LLM providers (Claude CLI, Ollama, OpenAI-compatible, llama.cpp), sessions, projects, permissions, and terminal sessions. Exposes HTTP + WebSocket APIs for streaming and synchronous access.

## Build

```bash
go build -o relayllm .
go build -o cmd/hook/hook ./cmd/hook
```

## Running

### Standalone

```bash
./relayllm --port 3001 --data-dir ~/.config/relayLLM
```

### Via Relay

```bash
./build.sh
```

This builds both binaries and registers the service with Relay (`relay service register --name "Relay LLM" --command ./relayllm --autostart`). Env vars and port can be configured in the Relay UI after registration.

## Configuration

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--data-dir` | `RELAY_LLM_DATA` | `~/.config/relayLLM` | Data directory |
| `--socket` | `RELAY_LLM_SOCKET` | `{data-dir}/relayllm.sock` | Unix domain socket relayLLM listens on |
| `--token` | `RELAY_LLM_TOKEN` | *(auto-generated)* | Bearer token for API auth. Auto-generated 64-char hex if unset (not logged; set the env var to pin) |
| `--ollama-url` | `OLLAMA_URL` | `http://localhost:11434` | Ollama base URL |
| `--openai-config` | `OPENAI_CONFIG` | *(uses settings.json)* | Override OpenAI endpoints config file |
| `--llama-server-path` | `LLAMA_SERVER_PATH` | `llama-server` (PATH) | Path to llama-server binary |
| `--router-port` | `RELAY_ROUTER_PORT` | *(empty, disabled)* | Port for the unified OpenAI-compatible relay-router fronting llama-server + OpenAI endpoints |
| `--comfyui-url` | `COMFYUI_URL` | *(empty, disabled)* | ComfyUI base URL for image generation |

When spawned by relay, relayLLM additionally observes `RELAY_BRIDGE_SOCKET`, `RELAY_SERVICE_ID`, and `RELAY_MCP_TOKEN`. Their presence triggers manifest registration with relay — see [Service Manifest Integration](#service-manifest-integration).

## HTTP API

All endpoints accept and return JSON.

### Projects

**`GET /api/projects`** -- List all projects.

Response `200`:
```json
[{"id": "uuid", "name": "my-project", "path": "/code/myapp", "model": "sonnet", "allowedTools": [], "createdAt": "2025-01-01T00:00:00Z"}]
```

**`POST /api/projects`** -- Create a project.

Request:
```json
{"name": "my-project", "path": "/code/myapp", "model": "sonnet", "allowedTools": ["Read", "Write"]}
```
`model` defaults to `"sonnet"`. `allowedTools` defaults to `[]`.

Response `201`: the created project object.

**`GET /api/projects/:id`** -- Get a project.

Response `200`: project object. `404` if not found.

**`PUT /api/projects/:id`** -- Update a project.

Request: partial object with fields to update (`name`, `path`, `model`, `allowedTools`).

Response `200`: updated project object. `404` if not found.

**`DELETE /api/projects/:id`** -- Delete a project.

(Project endpoints are owned by relay when running enhanced. When running standalone, relayLLM stores its own minimal project records under `{data-dir}/projects.json` for direct callers.)

Response `200`: `{"success": true}`. `404` if not found.

### Sessions

**`GET /api/sessions`** -- List active sessions.

Response `200`:
```json
[{"id": "uuid", "projectId": "uuid", "name": "New Session", "directory": "/code/myapp", "model": "sonnet", "active": true}]
```

**`POST /api/sessions`** -- Create a session and start the provider process.

Request:
```json
{"projectId": "uuid", "name": "my session", "model": "haiku"}
```
Either `projectId` or `directory` is required. If `projectId` is provided, the project's path and model are used as defaults. `model` defaults to `"sonnet"`. `name` defaults to `"New Session"`.

Optional `settings` object is passed through to the provider. For headless (non-interactive) sessions:
```json
{"projectId": "uuid", "settings": {"headless": true}}
```
See [Headless Sessions](#headless-sessions) below.

Response `201`:
```json
{"sessionId": "uuid", "projectId": "uuid", "directory": "/code/myapp", "model": "haiku", "name": "my session"}
```

**`POST /api/sessions/:id/message`** -- Send a message (synchronous, blocks until response).

Request:
```json
{"text": "Hello", "files": [{"name": "img.png", "mimeType": "image/png", "data": "<base64>"}]}
```
`files` is optional.

Response `200`:
```json
{"response": "Hi there!", "stats": {"inputTokens": 42, "outputTokens": 10, "cacheReadTokens": 0, "cacheCreationTokens": 0, "costUsd": 0.001}}
```
`504` on timeout (5 min). `500` on provider error.

**`DELETE /api/sessions/:id`** -- End a session and kill the provider process.

Response `200`: `{"success": true}`.

**`POST /api/sessions/:id/delete`** -- Permanently delete a session, its provider data, and persisted file.

Response `200`: `{"success": true}`.

### Models

**`GET /api/models`** -- List available models from all providers. Discovers models concurrently from Ollama (`/api/tags`) and each configured OpenAI-compatible endpoint (`/v1/models`). llama.cpp models are listed statically from the `llama-server` section of `settings.json`.

Response `200`:
```json
{
  "models": [
    {"label": "Claude Haiku", "value": "haiku", "group": "Claude", "provider": "claude"},
    {"label": "llama3.2:latest", "value": "llama3.2:latest", "group": "Ollama", "provider": "ollama"},
    {"label": "lmstudio/qwen2.5-coder-32b", "value": "lmstudio/qwen2.5-coder-32b", "group": "LM Studio", "provider": "openai"},
    {"label": "llama/qwen3-8b", "value": "llama/qwen3-8b", "group": "llama.cpp", "provider": "llama"}
  ],
  "providerSettings": { ... }
}
```

### Permissions

**`POST /api/permission`** -- Used by the hook binary. Holds the connection open until a decision is made or 60s timeout.

Request:
```json
{"sessionId": "uuid", "toolName": "Write", "toolInput": "{...}", "toolUseId": "uuid"}
```

Response `200`:
```json
{"decision": "allow", "reason": ""}
```
`decision` is `"allow"` or `"deny"`. Defaults to `"deny"` with reason `"timeout"` after 60s.

### Status

**`GET /api/status`** -- Runtime status (uptime, session count, terminal count, llama-instance count).

**`GET /api/llama/instances`** -- List currently-running llama-server instances (alias, port, pid, started-at, healthy).

**`DELETE /api/llama/instances/{alias}`** -- Gracefully stop a single llama-server instance (SIGTERM, 3 s grace, SIGKILL). Next session for that alias re-launches on demand.

## WebSocket Protocol

Connect to `/ws`. All messages are JSON with a `type` field.

### Client to Server

**`join_session`** -- Bind to a session and receive its history.
```json
{"type": "join_session", "sessionId": "uuid"}
```

**`send_message`** -- Send a message. `sessionId` optional if already joined.
```json
{"type": "send_message", "sessionId": "uuid", "text": "Hello", "files": []}
```

**`end_session`** -- End a session.
```json
{"type": "end_session", "sessionId": "uuid"}
```

**`rename_session`**
```json
{"type": "rename_session", "sessionId": "uuid", "name": "New Name"}
```

**`delete_session`** -- End and delete persisted session data.
```json
{"type": "delete_session", "sessionId": "uuid"}
```

**`clear_session`** -- Clear messages/stats and restart the provider.
```json
{"type": "clear_session", "sessionId": "uuid"}
```

**`permission_response`** -- Respond to a permission request.
```json
{"type": "permission_response", "permissionId": "uuid", "approved": true, "reason": ""}
```

### Server to Client

| Type | Fields | Description |
|------|--------|-------------|
| `session_joined` | `sessionId`, `directory`, `model`, `name`, `history`, `stats` | Response to `join_session` with full session state |
| `llm_event` | `sessionId`, `event` | Raw Claude CLI stream-json event |
| `stats_update` | `sessionId`, `stats` | Token usage and cost update |
| `message_complete` | `sessionId` | LLM finished responding |
| `permission_request` | `sessionId`, `permissionId`, `toolName`, `toolInput` | Tool needs approval |
| `session_ended` | `sessionId` | Session was deleted |
| `session_renamed` | `sessionId`, `name` | Session name changed |
| `clear_messages` | `sessionId` | Messages were cleared |
| `process_exited` | `sessionId` | Provider process died |
| `raw_output` | `sessionId`, `text` | Non-JSON provider output |
| `system_message` | `sessionId`, `message` | System notification |
| `error` | `message` | Error message |

Task lifecycle events (`task_started`, `task_completed`, etc.) are emitted by relayScheduler and reach Eve through relay's front-door dispatcher directly — relayLLM no longer proxies them.

## Permission Flow

1. Claude CLI invokes the PreToolUse hook binary (`cmd/hook/hook`) before each tool use
2. The hook binary POSTs to `/api/permission` on the relayLLM server
3. The server holds the HTTP connection open and sends a `permission_request` to the WebSocket client
4. The client sends a `permission_response` back via WebSocket
5. The server resolves the held HTTP connection with the decision
6. The hook binary returns the decision to Claude CLI

The hook binary is automatically registered in `.claude/settings.local.json` when a session starts. It uses `RELAY_LLM_HOOK_URL` and `RELAY_LLM_SESSION_ID` env vars set by the provider.

### Headless Sessions

When a session is created with `settings: {"headless": true}`, the Claude provider:

1. Adds `--dangerously-skip-permissions --permission-mode bypassPermissions` to CLI args
2. Sets `RELAY_LLM_HEADLESS=true` env var on the Claude process

The hook binary inherits the env var and exits 0 immediately (auto-approve) instead of POSTing to `/api/permission`. This prevents headless sessions (e.g. from relayScheduler) from stalling on permission prompts with no human to respond.

Used by relayScheduler for scheduled tasks. Interactive sessions from Eve don't set this flag.

## Provider Configuration

All provider configuration lives in a single `{data-dir}/settings.json`:

```json
{
  "openai": {
    "endpoints": [
      {
        "name": "lmstudio",
        "baseURL": "http://localhost:1234/v1",
        "apiKey": "",
        "group": "LM Studio"
      }
    ]
  },
  "llama-server": {
    "binaryPath": "/usr/local/bin/llama-server",
    "basePort": 8090,
    "models": [
      {
        "alias": "qwen3-8b",
        "model": "/models/Qwen3-8B-Q4_K_M.gguf",
        "ctx-size": 131072,
        "n-gpu-layers": -1,
        "threads": 8,
        "flash-attn": true,
        "fit": true,
        "kv-unified": true,
        "cache-type-k": "q8_0",
        "cache-type-v": "q8_0",
        "temp": 0.6,
        "top-p": 0.95,
        "top-k": 20,
        "min-p": 0.0
      }
    ]
  }
}
```

Both sections are optional. If `settings.json` is absent, falls back to separate `openai_endpoints.json` + `llama_models.json` files, then `OPENAI_BASE_URL`/`OPENAI_API_KEY` env vars.

### OpenAI-compatible Endpoints

The `openai` section configures OpenAI-compatible servers (LM Studio, Ollama /v1, OMLX, etc.). Each endpoint's `name` is the routing prefix — users select models as `{name}/{model-id}` (e.g. `lmstudio/qwen2.5-coder-32b`).

Per-endpoint fields:

| Field | Required | Description |
|---|---|---|
| `name` | yes | Routing prefix used in model IDs (`{name}/{model}`) |
| `baseURL` | yes | Endpoint base URL, e.g. `http://localhost:1234/v1`. Trailing slash is stripped |
| `apiKey` | no | Sent as `Authorization: Bearer ...`. Omit for unauthenticated servers (local Ollama, llama.cpp) |
| `group` | no | Display group in the model picker. Defaults to `name` |
| `strict` | no | When `true`, omits non-standard request fields (`stream_options`, `top_k`, `min_p`, `repetition_penalty`). Set for OpenAI proper, Azure OpenAI, and stricter gateways that 400 on unknown body fields. Defaults to `false` for compatibility with LM Studio, Ollama /v1, oMLX, llama.cpp, which accept these fields silently |

Reachability is verified at session start via `GET /models`. Any 2xx response is healthy; 404 is treated as "endpoint up but `/models` not implemented" so servers without a model-listing endpoint remain usable. 401/403 fail fast.

### llama.cpp / llama-server

The `llama-server` section configures managed llama-server processes. Every key in a model entry except `alias` maps directly to a `--{key}` llama-server CLI flag. Boolean `true` emits the flag, `false` omits it, numbers and strings become `--key value`. Any llama-server flag works without code changes — `mmproj`, `mlock`, `cont-batching`, future flags, etc.

Top-level `llama-server` fields:
- `binaryPath` — path to the llama-server binary (override: `--llama-server-path` / `LLAMA_SERVER_PATH`, default: `llama-server` on PATH)
- `modelDir` — base directory for relative model paths (supports `~` expansion, e.g. `"~/models/"`). Relative `model` paths in each entry are resolved against this directory; absolute paths are left as-is.
- `basePort` — starting port for auto-allocation (default: 8090). Models with an explicit `port` key skip auto-allocation.

- **Model selection**: `llama/{alias}` (e.g. `llama/qwen3-8b`) in Eve or the sessions API
- **On-demand launch**: First request for a model starts llama-server on an auto-allocated port (or explicit `"port"` from config), polls `/health` until ready (up to 120s)
- **Instance sharing**: Multiple sessions using the same model share one llama-server process
- **Crash recovery**: Dead processes are relaunched on the next request

### Relay Router

A unified OpenAI-compatible router fronting both llama-server and every configured OpenAI endpoint. Enable with `--router-port`:

```bash
./relayllm --router-port 8080
```

Any OpenAI client can point at `http://localhost:8080/v1` and use either a llama alias (bare) or an OpenAI endpoint model (prefixed `endpoint.Name/`):

```bash
# Llama branch
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "qwen3-8b", "messages": [{"role": "user", "content": "Hello"}], "stream": true}'

# OpenAI-endpoint branch (e.g. an OMLX endpoint named "omlx")
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "omlx/Qwen3.5-27B", "messages": [{"role": "user", "content": "Hello"}], "stream": true}'
```

The router reads the `model` field, launches or reuses the right llama-server (or rewrites the body's `model` to the bare upstream id and reverse-proxies to the matching OpenAI endpoint with its API key), and streams SSE responses back. `GET /v1/models` lists every llama alias plus every reachable OpenAI endpoint model. Endpoint reachability is probed on demand with a 15 s TTL cache; offline endpoints disappear from the listing until the next probe succeeds.

## Data Storage

Default is `os.UserConfigDir()/relayLLM`:
- macOS: `~/Library/Application Support/relayLLM/`
- Linux: `~/.config/relayLLM/`

Override with `--data-dir` or `RELAY_LLM_DATA`.

- `settings.json` -- unified provider config (see [Provider Configuration](#provider-configuration))
- `projects.json` -- project definitions
- `sessions/<id>.json` -- per-session state (messages, stats, provider state)
- `terminals/templates.json` -- custom terminal templates
- `generated/` -- images produced by the generate_image tool

Sessions are persisted on message completion and session end. Restored on server startup.

## Testing

Three tiers, gated by build tag:

```bash
go test ./...                       # default: hermetic, no external deps, ~1.3 s
go test -tags=live ./...            # legacy integration; requires Ollama/LM Studio/OMLX/relay running
go test -tags=llm ./...             # real llama.cpp against an installed model (see below)
```

The default suite covers the WebSocket protocol, HTTP API, session lifecycle, tool-call loop, pi event translation, and relay manifest registration — all driven by fakes in `support_test.go` / `support_server_test.go`. No LLM calls, no subprocesses, no network.

The `llm` tier (`provider_llama_live_test.go`) requires `Qwen3.6 MoE 35` registered in `~/Library/Application Support/relayLLM/settings.json` under `llama-server.models`, with the model file at the configured `modelDir`. Skips gracefully if absent. Validates SSE chunking, llama-server lifecycle, mid-stream stop — the surface fakes can't reach.

The `live` tier is for legacy integration tests that depend on third-party services; run only on demand.

## Service Manifest Integration

relayLLM is one of several relay-enhanced services. It detects its run mode from `RELAY_BRIDGE_SOCKET`:

- **Standalone** (env unset): binds its own Unix socket, auto-generates a bearer token if `--token` is unset, serves direct HTTP/WS clients.
- **Enhanced** (env set): same listener and same wire language, plus it dials relay's bridge with a `RegisterManifest` payload declaring the routes it serves, its internal socket + token, and its status endpoint. Relay's front-door dispatcher then forwards matching front-door traffic over that socket.

The mode switch is a deployment fact, not a code fork. Same config loader, two sources. Tasks (`/api/tasks/*`) are served by relayScheduler — Eve reaches them through relay's dispatcher directly; relayLLM no longer carries foreign routes.

See [`../relay/plans/service-manifest-spec.md`](../relay/plans/service-manifest-spec.md) for the protocol contract.

## Ecosystem

- **[Relay](https://github.com/barelyworkingcode/relay)** -- Orchestrator and front-door dispatcher. Spawns relayLLM, routes inbound traffic per registered manifests.
- **[Eve](https://github.com/barelyworkingcode/eve)** -- Browser-based LLM frontend. Dials relay's frontend socket; relay dispatches `/api/sessions`, `/api/terminals`, etc. to relayLLM.
- **[relayScheduler](https://github.com/barelyworkingcode/relayScheduler)** -- Task scheduler. Registers its own manifest with relay; dispatched directly.
- **[relayTelegram](https://github.com/barelyworkingcode/relayTelegram)** -- Telegram bot bridge to relayLLM sessions.

## License

[MIT](./LICENSE)
