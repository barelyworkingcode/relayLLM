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
response_collector.go  Headless response accumulation for HTTP clients
api.go               HTTP routes (projects, sessions, permissions)
ws.go                WebSocket server (streaming events to Eve)
permission.go        Permission request/response tracking
cmd/hook/            Compiled PreToolUse hook binary
```

## Providers

- **Claude**: Persistent process. `claude --print --output-format stream-json --input-format stream-json --verbose --model <model>`. Resumes via `--resume <sessionId>`.
- **Gemini**: (planned) One-shot process per message.
- **LM Studio**: (planned) HTTP client with SSE streaming.

## API

HTTP on `--port` (default 3001). WebSocket at `/ws`.

### HTTP Endpoints
```
GET/POST       /api/projects       — list/create projects
GET/PUT/DELETE /api/projects/:id   — get/update/delete project
GET/POST       /api/sessions       — list/create sessions
POST           /api/sessions/:id/message — send message (sync, for HTTP clients)
DELETE         /api/sessions/:id   — end session
POST           /api/permission     — hook binary posts here, held open until user decides
```

### WebSocket Protocol
```
Client → Server: join_session, send_message, end_session, permission_response
Server → Client: session_joined, llm_event, stats_update, message_complete, permission_request, error
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
