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
| `--port` | `RELAY_LLM_PORT` | `3001` | HTTP/WebSocket listen port |
| `--data-dir` | `RELAY_LLM_DATA` | `~/.config/relayLLM` | Data directory |
| `--ollama-url` | `OLLAMA_URL` | `http://localhost:11434` | Ollama base URL |
| `--openai-config` | `OPENAI_CONFIG` | *(none, uses config.json)* | Override OpenAI endpoints config file |
| `--llama-server-path` | `LLAMA_SERVER_PATH` | `llama-server` (PATH) | Path to llama-server binary |
| `--llama-proxy-port` | `LLAMA_PROXY_PORT` | *(empty, disabled)* | Port for OpenAI-compatible llama proxy |
| `--token` | `RELAY_LLM_TOKEN` | *(empty, no auth)* | Bearer token for API auth |
| `--socket` | `RELAY_LLM_SOCKET` | *(empty, disabled)* | Unix domain socket path |
| `--scheduler-url` | `RELAY_SCHEDULER_URL` | `http://localhost:3002` | relayScheduler URL for task proxy |
| `--comfyui-url` | `COMFYUI_URL` | *(empty, disabled)* | ComfyUI base URL for image generation |

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

**`DELETE /api/projects/:id`** -- Delete a project. Also deletes any associated tasks in relayScheduler (via scheduler proxy).

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

**`GET /api/models`** -- List available models from all providers. Discovers models concurrently from Ollama (`/api/tags`) and each configured OpenAI-compatible endpoint (`/v1/models`). llama.cpp models are listed statically from the `llama-server` section of `config.json`.

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

### Tasks (Scheduler Proxy)

All task endpoints proxy to relayScheduler. Returns `502` if relayScheduler is unavailable.

**`GET /api/tasks`** -- List all tasks.

**`POST /api/tasks`** -- Create a task.

**`GET /api/tasks/:id`** -- Get a task.

**`PUT /api/tasks/:id`** -- Update a task.

**`DELETE /api/tasks/:id`** -- Delete a task.

**`POST /api/tasks/:id/run`** -- Trigger immediate execution of a task.

**`GET /api/tasks/:id/history`** -- Get execution history for a task.

**`GET /api/tasks/project/:projectId`** -- List tasks for a specific project.

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
| `task_started` | `taskId`, `projectId` | Task execution started (forwarded from relayScheduler) |
| `task_completed` | `taskId`, `projectId`, `result` | Task execution completed (forwarded from relayScheduler) |
| `task_error` | `taskId`, `projectId`, `error` | Task execution failed (forwarded from relayScheduler) |
| `task_status` | `taskId`, `projectId`, `status` | Task status changed (forwarded from relayScheduler) |
| `error` | `message` | Error message |

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

All provider configuration lives in a single `{data-dir}/config.json`:

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

Both sections are optional. If `config.json` is absent, falls back to separate `openai_endpoints.json` + `llama_models.json` files, then `OPENAI_BASE_URL`/`OPENAI_API_KEY` env vars.

### OpenAI-compatible Endpoints

The `openai` section configures OpenAI-compatible servers (LM Studio, Ollama /v1, OMLX, etc.). Each endpoint's `name` is the routing prefix — users select models as `{name}/{model-id}` (e.g. `lmstudio/qwen2.5-coder-32b`).

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

### llama Proxy

An optional OpenAI-compatible reverse proxy for external tools. Enable with `--llama-proxy-port`:

```bash
./relayllm --llama-proxy-port 8080
```

Any OpenAI client can point at `http://localhost:8080/v1` and use the model alias directly:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "qwen3-8b", "messages": [{"role": "user", "content": "Hello"}], "stream": true}'
```

The proxy reads the `model` field, launches or reuses the right llama-server, and proxies the request with SSE streaming. `GET /v1/models` lists all configured aliases.

## Data Storage

Default: `~/.config/relayLLM/`. Override with `--data-dir` or `RELAY_LLM_DATA`.

- `config.json` -- unified provider config (see [Provider Configuration](#provider-configuration))
- `projects.json` -- project definitions
- `sessions/<id>.json` -- per-session state (messages, stats, provider state)
- `terminals/templates.json` -- custom terminal templates
- `generated/` -- images produced by the generate_image tool

Sessions are persisted on message completion and session end. Restored on server startup.

## Testing

```bash
go test -short ./...     # unit + integration tests that don't need claude CLI
go test ./...            # all tests (requires claude CLI + API key)
./smoketest.sh           # end-to-end against a running instance (default: localhost:3001)
./smoketest.sh http://localhost:4000  # custom URL
```

## Ecosystem

relayLLM is the LLM engine for the Relay ecosystem. It serves as the single backend for Eve and relayTelegram, and proxies task operations to relayScheduler. Eve, relayScheduler, and relayTelegram all connect to its HTTP/WS API.

- **[Relay](https://github.com/barelyworkingcode/relay)** -- MCP orchestrator. Manages relayLLM as a background service.
- **[Eve](https://github.com/barelyworkingcode/eve)** -- Browser-based LLM frontend. Single-backend client to relayLLM for all operations including tasks.
- **[relayScheduler](https://github.com/barelyworkingcode/relayScheduler)** -- Task scheduler. Runs LLM prompts on schedule. relayLLM proxies task API and forwards scheduler WebSocket events.
- **[relayTelegram](https://github.com/barelyworkingcode/relayTelegram)** -- Telegram bot bridge to relayLLM sessions.

## License

[MIT](./LICENSE)
