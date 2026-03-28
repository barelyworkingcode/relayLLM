# relayLLM (Go)

Standalone LLM engine service. Manages providers (Claude CLI, Gemini CLI, LM Studio HTTP), sessions, projects, and permissions. Runs independently or as a relay-managed service.

## Architecture

```
main.go              Entry point, flag parsing, server wiring
project.go           Project CRUD + JSON file storage
session.go           Session lifecycle management
session_store.go     Session persistence to disk
provider.go          Provider interface + shared types
provider_claude.go   Claude CLI provider (stream-json, persistent process)
provider_lmstudio.go LM Studio HTTP provider (SSE streaming)
provider_settings.go Per-provider settings schema for Eve UI
response_collector.go  Headless response accumulation for HTTP clients
api.go               HTTP routes (projects, sessions, permissions)
ws.go                WebSocket server (streaming events to Eve)
permission.go        Permission request/response tracking
scheduler_client.go  HTTP client for relayScheduler
scheduler_proxy.go   Task API proxy routes (GET/POST/PUT/DELETE /api/tasks/*)
scheduler_ws.go      WebSocket forwarding of scheduler events (task_started, etc.)
cmd/hook/            Compiled PreToolUse hook binary
```

## Providers

- **Claude**: Persistent process. `claude --print --output-format stream-json --input-format stream-json --verbose --model <model>`. Resumes via `--resume <sessionId>`. Headless sessions add `--dangerously-skip-permissions --permission-mode bypassPermissions` and set `RELAY_LLM_HEADLESS=true` env var (hook auto-approves).
- **Gemini**: (planned) One-shot process per message.
- **LM Studio**: HTTP client with SSE streaming. Base URL via `--lmstudio-url` / `LM_STUDIO_URL` (default `http://localhost:1234`). Optional `LM_STUDIO_API_TOKEN` env var for Bearer auth. Per-session settings: `temperature`, `reasoning`, `contextLength`, `integrations`.

## API

HTTP on `--port` (default 3001). WebSocket at `/ws`.

### HTTP Endpoints
```
GET/POST       /api/projects       — list/create projects
GET/PUT/DELETE /api/projects/:id   — get/update/delete project
GET            /api/models         — list available models (Claude + LM Studio)
GET/POST       /api/sessions       — list/create sessions
POST           /api/sessions/:id/message — send message (sync, for HTTP clients)
DELETE         /api/sessions/:id   — end session
POST           /api/permission     — hook binary posts here, held open until user decides
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
```

## Data

Default: `~/.config/relayLLM/`. Override: `--data-dir` or `RELAY_LLM_DATA`.
- `projects.json` — project definitions
- `sessions/` — per-session JSON files

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
