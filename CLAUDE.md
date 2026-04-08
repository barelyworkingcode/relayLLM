# relayLLM (Go)

Standalone LLM engine service. Manages providers (Claude CLI, Ollama HTTP), sessions, projects, permissions, and terminal sessions (PTY). Runs independently or as a relay-managed service.

## Architecture

```
main.go              Entry point, flag parsing, server wiring
project.go           Project CRUD + JSON file storage
session.go           Session lifecycle management
session_store.go     Session persistence to disk
provider.go          Provider interface + shared types + extractTextContent
provider_claude.go   Claude CLI provider (stream-json, persistent process)
provider_ollama.go   Ollama HTTP provider (NDJSON streaming)
provider_settings.go Per-provider settings schema for Eve UI
response_collector.go  Headless response accumulation for HTTP clients
terminal_template.go Terminal template types + JSON file store (built-in + custom)
terminal_session.go  Terminal session with PTY management (creack/pty)
terminal_manager.go  Terminal CRUD + lifecycle management
api.go               HTTP routes (projects, sessions, terminals, permissions)
ws.go                WebSocket server (streaming events to Eve, terminal I/O)
permission.go        Permission request/response tracking
scheduler_client.go  HTTP client for relayScheduler
scheduler_proxy.go   Task API proxy routes (GET/POST/PUT/DELETE /api/tasks/*)
scheduler_ws.go      WebSocket forwarding of scheduler events (task_started, etc.)
cmd/hook/            Compiled PreToolUse hook binary
```

## Providers

- **Claude**: Persistent process. `claude --print --output-format stream-json --input-format stream-json --verbose --model <model>`. Resumes via `--resume <sessionId>`. Headless sessions add `--dangerously-skip-permissions --permission-mode bypassPermissions` and set `RELAY_LLM_HEADLESS=true` env var (hook auto-approves).
- **Ollama**: HTTP client with NDJSON streaming. Base URL via `--ollama-url` / `OLLAMA_URL` (default `http://localhost:11434`). Sends full conversation history per request; relies on Ollama's automatic KV cache prefix reuse. Per-session settings: `temperature`, `top_p`, `top_k`, `min_p`, `think` (bool), `num_ctx`. Explicitly sends `think: false` to suppress built-in reasoning on thinking models (e.g. Gemma 4). Supports image attachments via base64.

## API

HTTP on `--port` (default 3001). WebSocket at `/ws`.

### HTTP Endpoints
```
GET/POST       /api/projects       — list/create projects
GET/PUT/DELETE /api/projects/:id   — get/update/delete project
GET            /api/models         — list available models (Claude + Ollama)
GET/POST       /api/sessions       — list/create sessions
POST           /api/sessions/:id/message — send message (sync, for HTTP clients)
DELETE         /api/sessions/:id   — end session
GET/POST       /api/terminal/templates     — list/create terminal templates
GET/PUT/DELETE /api/terminal/templates/:id — get/update/delete custom template
GET/POST       /api/terminals              — list/create terminal instances
DELETE         /api/terminals/:id          — close terminal
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

Terminal messages:
Client → Server: terminal_create, join_terminal, leave_terminal, terminal_input (base64), terminal_resize, terminal_close, terminal_list, terminal_reconnect, terminal_templates
Server → Client: terminal_created, terminal_joined (with base64 scrollback), terminal_output (base64), terminal_exit, terminal_closed, terminal_list, terminal_templates
```

## Terminal Sessions

PTY-backed terminal sessions hosted by relayLLM. Eve proxies terminal I/O via WebSocket (base64-encoded). Terminals survive Eve restarts.

- **Templates**: Built-in (Claude Code, OpenCode, Shell) + custom via API. `IdleTimeout` field (minutes, default 1440 = 24h).
- **Idle timeout**: When all viewers disconnect, an idle timer starts. If no viewer reconnects before it fires, the terminal is auto-closed. Configurable per template.
- **Color**: PTY spawned with `TERM=xterm-256color` and `COLORTERM=truecolor` for full 24-bit color.
- **Scrollback**: 100KB ring buffer per terminal, replayed on reconnect.

## Data

Default: `~/.config/relayLLM/`. Override: `--data-dir` or `RELAY_LLM_DATA`.
- `projects.json` — project definitions
- `sessions/` — per-session JSON files
- `terminals/templates.json` — custom terminal templates

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
