# relayLLM

The LLM execution engine of the [relay](https://github.com/barelyworkingcode/relay)
ecosystem. A Go service that runs LLM sessions across multiple providers, streams
results, manages PTY-backed terminals, and exposes an HTTP + WebSocket API over a
Unix socket. It runs standalone or as a relay-enhanced service.

relayLLM is **project-unaware**: projects live in relay, scheduled tasks live in
relayScheduler. relayLLM serves sessions, models, permissions, terminals, and
status — nothing else.

## Build

```bash
go build -o relayllm .
go build -o cmd/hook/hook ./cmd/hook    # PreToolUse permission hook
```

## Run

### Standalone

```bash
./relayllm --data-dir ~/.config/relayLLM
```

relayLLM listens on a **Unix domain socket** (`{data-dir}/relayllm.sock` by
default; override with `--socket`) — there is no TCP API port. In standalone mode
it auto-generates a bearer token if `--token` is unset (printed nowhere; set the
env var to pin it).

### Via relay

```bash
./build.sh
```

Builds both binaries and registers the service with relay. When relay spawns it,
relayLLM sees `RELAY_BRIDGE_SOCKET` + `RELAY_SERVICE_ID` + `RELAY_SERVICE_TOKEN`
and registers a manifest (see [Service manifest](#service-manifest)).

## Configuration

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--data-dir` | `RELAY_LLM_DATA` | `~/Library/Application Support/relayLLM` (macOS) | Data directory |
| `--socket` | `RELAY_LLM_SOCKET` | `{data-dir}/relayllm.sock` | Unix socket the API listens on |
| `--token` | `RELAY_LLM_TOKEN` | *(auto 64-char hex)* | Bearer token for API auth |
| `--ollama-url` | `OLLAMA_URL` | `http://localhost:11434` | Ollama base URL |
| `--openai-config` | `OPENAI_CONFIG` | *(settings.json)* | Override OpenAI endpoints config |
| `--llama-server-path` | `LLAMA_SERVER_PATH` | `llama-server` (PATH) | llama-server binary |
| `--mlx-serve-path` | `MLX_SERVE_PATH` | `mlx-serve` (PATH) | mlx-serve binary |
| `--router-port` | `RELAY_ROUTER_PORT` | *(disabled)* | Port for the unified OpenAI-compatible router |

Provider configuration lives in `{data-dir}/settings.json`. See
[Providers](#providers) and the inline schema in `config.go`.

## Providers

relayLLM normalizes every provider into one canonical event stream (see
[`docs/event-protocol.md`](docs/event-protocol.md)), so the frontend renders all
of them identically.

- **Claude** — the `claude` CLI as a persistent `stream-json` process; resumes via `--resume`. Honors a PreToolUse permission hook (below).
- **pi** — the `pi` coding agent (`--mode rpc`); model ids are `pi/<provider>/<model>`. Its RPC stream is translated into the canonical Claude envelope. No permission hook (pi auto-executes tools).
- **Ollama** — HTTP/NDJSON. Base URL via `--ollama-url`.
- **OpenAI-compatible** — HTTP/SSE for LM Studio, Ollama `/v1`, oMLX, etc. Configured under `settings.json` `openai`; model ids are `{endpoint}/{model}`.
- **llama.cpp** — managed `llama-server` processes (`settings.json` `llama-server`); model ids `llama/{alias}`. Launched on demand, reused across sessions.
- **MLX** — managed [mlx-serve](https://github.com/ddalcu/mlx-serve) processes (`settings.json` `mlx-serve`, same schema as llama-server); model ids `mlx/{alias}`.

Each model entry's keys map 1:1 to the server's CLI flags, so any current or
future flag works without code changes. Managed servers (llama + MLX) launch on
first use, poll `/health` until ready, and are shared across sessions.

### Relay-router (optional)

`--router-port N` exposes a single OpenAI-compatible endpoint
(`http://localhost:N/v1`) that fronts every managed-server alias and every
reachable OpenAI endpoint, so any OpenAI client can reach all local models
through one URL. Details in [CLAUDE.md](CLAUDE.md#relay-router-relay_routergo).

Configure `virtual-llms` in `settings.json` to expose a stable model name that
uses the first reachable target, in order. An endpoint target reuses a name
from `openai.endpoints` and its `/models` health check is cached for 15
seconds; an `alias` target selects a local managed llama.cpp or MLX model.

```json
"virtual-llms": {
  "models": [{
    "name": "vCode",
    "targets": [
      {"endpoint": "remote-llama", "model": "code"},
      {"alias": "local-code"}
    ]
  }]
}
```

## API

The API is a Unix-socket HTTP + WebSocket surface. Treat it as a public API for
direct (standalone) callers; through relay, Eve reaches it via the front-door
dispatcher. The authoritative wire contract is
[`docs/event-protocol.md`](docs/event-protocol.md) and the route/message
constants in `api.go` / `ws_messages.go`.

**HTTP** (all JSON):

- `GET/POST /api/sessions`, `POST /api/sessions/:id/message` (sync), `DELETE /api/sessions/:id`, `POST /api/sessions/:id/delete`, `POST /api/sessions/:id/stop`; pi-only `PUT /api/sessions/:id/model` and `…/thinking-level`.
- `GET /api/models` — models from Claude + Ollama + OpenAI endpoints + llama.cpp + MLX.
- `GET/POST /api/terminals`, `DELETE /api/terminals/:id`, `GET /api/terminals/:id/log`; `GET /api/terminal/templates` (read-mostly).
- `GET /api/status`; `GET+DELETE /api/llama/instances[/{alias}]`; `GET+DELETE /api/mlx/instances[/{alias}]`.
- `POST /api/permission` — the hook posts here; held open until the user decides (60s timeout → deny).
- `GET /api/generated/:filename` — serves images written by the relay-comfyui MCP tool.

**WebSocket** (`/ws`): client sends `join_session` / `send_message` /
`permission_response` / terminal ops; the server streams `llm_event` (a
**canonical, provider-agnostic** stream event — not raw Claude output),
`stats_update`, `message_complete`, `permission_request`, terminal frames, etc.
Full catalog: `ws_messages.go` + [`docs/event-protocol.md`](docs/event-protocol.md).

### Permission flow

The `claude` CLI runs the PreToolUse hook binary before each tool use; the hook
dials relayLLM's Unix socket (`RELAY_LLM_HOOK_SOCKET`, with `RELAY_LLM_SESSION_ID`
+ `RELAY_LLM_HOOK_TOKEN`), which holds the request open and asks the WebSocket
client to approve/deny. Headless sessions (`settings: {"headless": true}`, used by
relayScheduler) set `RELAY_LLM_HEADLESS=true` so the hook auto-approves.

## Data

Default `os.UserConfigDir()/relayLLM` (macOS `~/Library/Application Support/relayLLM/`,
Linux `~/.config/relayLLM/`); override with `--data-dir`.

- `settings.json` — unified provider config (falls back to `openai_endpoints.json` + `llama_models.json`, then env).
- `sessions/<id>.json` — per-session state (a daily sweeper removes headless sessions older than 7 days).
- `pi-sessions/` — pi session JSONLs.
- `generated/` — images written by the relay-comfyui MCP tool, served via `/api/generated/`.

Terminal templates live in the `pty` section of `settings.json` (seeded with
built-ins on first run; relay's config editor manages them).

## Testing

```bash
go test ./...              # hermetic (no external deps, no subprocess, no network)
go test -tags=live ./...   # opt-in: needs Ollama / LM Studio / oMLX / relay running
go test -tags=llm ./...    # opt-in: real llama-server against an installed GGUF
```

The hermetic tier covers the WS protocol, HTTP API, session lifecycle, tool-call
loop, pi event translation, and manifest registration via fakes
(`support_test.go` / `support_server_test.go`). Install the pre-commit hook once:
`git config core.hooksPath .githooks`.

## Service manifest

relayLLM detects its run mode from `RELAY_BRIDGE_SOCKET`:

- **Standalone** (unset) — binds its own socket, serves direct clients.
- **Enhanced** (set) — same listener and wire language, plus it dials relay's
  bridge with a `RegisterManifest` payload declaring its routes, internal socket
  + token, status endpoint, and actions. relay's dispatcher forwards matching
  front-door traffic over that socket.

The mode switch is a deployment fact, not a code fork — one config loader, two
sources. Protocol contract: [`../relay/docs/service-manifest.md`](../relay/docs/service-manifest.md).

## Ecosystem

- **[relay](https://github.com/barelyworkingcode/relay)** — orchestrator + front-door dispatcher; spawns relayLLM and routes traffic per registered manifests.
- **[Eve](https://github.com/barelyworkingcode/eve)** — browser frontend; reaches relayLLM through relay's front door.
- **[relayScheduler](https://github.com/barelyworkingcode/relayScheduler)** — runs scheduled tasks against terminal templates.
- **[relayTelegram](https://github.com/barelyworkingcode/relayTelegram)** — Telegram bot bridge.
- **[relayComfy](https://github.com/barelyworkingcode/relayComfy)** — ComfyUI service; image generation reaches it as the `comfyui` MCP tool through relay.

## License

[MIT](./LICENSE)
