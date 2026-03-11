# relayLLM

Standalone LLM engine service. Manages providers, sessions, projects, and permissions. Exposes HTTP + WebSocket APIs for streaming and synchronous access.

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

### Models

**`GET /api/models`** -- List available models.

Response `200`:
```json
[{"label": "Claude Haiku", "value": "haiku", "group": "Claude", "provider": "claude"}]
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

## Permission Flow

1. Claude CLI invokes the PreToolUse hook binary (`cmd/hook/hook`) before each tool use
2. The hook binary POSTs to `/api/permission` on the relayLLM server
3. The server holds the HTTP connection open and sends a `permission_request` to the WebSocket client
4. The client sends a `permission_response` back via WebSocket
5. The server resolves the held HTTP connection with the decision
6. The hook binary returns the decision to Claude CLI

The hook binary is automatically registered in `.claude/settings.local.json` when a session starts. It uses `RELAY_LLM_HOOK_URL` and `RELAY_LLM_SESSION_ID` env vars set by the provider.

## Data Storage

Default: `~/.config/relayLLM/`. Override with `--data-dir` or `RELAY_LLM_DATA`.

- `projects.json` -- project definitions
- `sessions/<id>.json` -- per-session state (messages, stats, provider state)

Sessions are persisted on message completion and session end. Restored on server startup.

## Testing

```bash
go test -short ./...     # unit + integration tests that don't need claude CLI
go test ./...            # all tests (requires claude CLI + API key)
./smoketest.sh           # end-to-end against a running instance (default: localhost:3001)
./smoketest.sh http://localhost:4000  # custom URL
```
