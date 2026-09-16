# relayLLM

The model-hosting engine of the [relay](https://github.com/barelyworkingcode/relay)
ecosystem. A Go service that launches and manages local model servers
(llama.cpp, MLX) and fronts them — plus configured OpenAI-compatible
endpoints and virtual models — behind one OpenAI-compatible router. It
exposes a small HTTP status/diagnostics API over a Unix socket. It runs
standalone or as a relay-enhanced service.

relayLLM is **project- and session-unaware**: sessions, terminals, and
permission hooks live in relay-sessions; projects live in relay; scheduled
tasks live in relayScheduler. relayLLM hosts models and status — nothing
else.

## Build

```bash
go build -o relayllm ./cmd/relayllm
```

## Run

### Standalone

```bash
./relayllm --data-dir ~/.config/relayLLM
```

relayLLM listens on a **Unix domain socket** (`{data-dir}/relayllm.sock` by
default; override with `--socket`) — there is no TCP API port for the
authenticated surface. In standalone mode it auto-generates a bearer token
if `--token` is unset (printed nowhere; set the env var to pin it).

### Via relay

```bash
./build.sh
```

Builds the binary and registers the service with relay. When relay spawns
it, relayLLM sees `RELAY_BRIDGE_SOCKET` + `RELAY_SERVICE_ID` +
`RELAY_LAUNCH_FD`, reads a one-time launch secret from that fd, authenticates
with a `Hello`, and registers a manifest (see
[Service manifest](#service-manifest)). No relay credential is ever held in
the environment.

## Configuration

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--data-dir` | `RELAY_LLM_DATA` | `~/Library/Application Support/relayLLM` (macOS) | Data directory |
| `--socket` | `RELAY_LLM_SOCKET` | `{data-dir}/relayllm.sock` | Unix socket the status API listens on |
| `--token` | `RELAY_LLM_TOKEN` | *(auto 64-char hex)* | Bearer token for API auth |
| `--openai-config` | `OPENAI_CONFIG` | *(settings.json)* | Override OpenAI endpoints config |
| `--llama-server-path` | `LLAMA_SERVER_PATH` | `llama-server` (PATH) | llama-server binary |
| `--mlx-serve-path` | `MLX_SERVE_PATH` | `mlx-serve` (PATH) | mlx-serve binary |
| `--router-port` | `RELAY_ROUTER_PORT` | *(disabled)* | Port for the unified OpenAI-compatible router |
| `--router-bind` | `RELAY_ROUTER_BIND` | `127.0.0.1` | Comma-separated bind addresses for the relay-router TCP listener, one per interface (e.g. `127.0.0.1,192.168.64.1`); include `0.0.0.0` to accept connections from other hosts |
| `--router-tls-cert` / `--router-tls-key` | `RELAY_LLM_ROUTER_TLS_CERT` / `RELAY_LLM_ROUTER_TLS_KEY` | *(plain http)* | TLS pair for the relay-router TCP listener |
| `--router-socket` | `RELAY_ROUTER_SOCKET` | `{data-dir}/router.sock` | Relay's private, tokenless path into the relay-router. Only opened when launched by relay; ignored standalone |
| `--http-port` | `RELAY_LLM_HTTP_PORT` | *(disabled)* | Port for an additional, unauthenticated TCP listener serving only the read-only `/status` diagnostics dashboard |
| `--http-bind` | `RELAY_LLM_HTTP_BIND` | `127.0.0.1` | Comma-separated bind addresses for the `--http-port` listener |
| `--http-tls-cert` / `--http-tls-key` | `RELAY_LLM_HTTP_TLS_CERT` / `RELAY_LLM_HTTP_TLS_KEY` | *(plain http)* | TLS pair for the `--http-port` listener |

Provider configuration lives in `{data-dir}/settings.json`. See
[Managed servers and endpoints](#managed-servers-and-endpoints) and the
inline schema in `internal/config/config.go`.

## Managed servers and endpoints

- **llama.cpp** — managed `llama-server` processes (`settings.json` `llama-server`); model ids `llama/{alias}`. Launched on demand, reused across sessions.
- **MLX** — managed [mlx-serve](https://github.com/ddalcu/mlx-serve) processes (`settings.json` `mlx-serve`, same schema as llama-server); model ids `mlx/{alias}`.
- **OpenAI-compatible** — HTTP/SSE for LM Studio, Ollama `/v1`, oMLX, etc. Configured under `settings.json` `openai`; model ids are `{endpoint}/{model}`.

Each model entry's keys map 1:1 to the server's CLI flags, so any current or
future flag works without code changes. Managed servers (llama + MLX) launch
on first use, poll `/health` until ready, and are shared across callers.

### Relay-router (optional)

`--router-port N` exposes a single OpenAI-compatible endpoint
(`http://127.0.0.1:N/v1` by default — see `--router-bind` above) that fronts
every managed-server alias and every reachable OpenAI endpoint, so any OpenAI
client can reach all local models through one URL. Details in
[CLAUDE.md](CLAUDE.md#relay-router-internalrouterroutergo).

**router.sock** (launched mode only): when relay launches relayLLM, this same
router is also served on a Unix socket (`--router-socket`, default
`{data-dir}/router.sock`) that admits exactly one peer — relay itself,
identified by the kernel audit token relayLLM captured off its own launch
`Hello` handshake, not a bearer. relayLLM registers this socket with relay
(`RegisterModelHost`) whenever it is launched, independent of whether its
front-door manifest registration succeeded (the two are separate relay
capabilities); relay refusing the model-host registration (the service
record lacks the `model_host` capability) is fatal — relayLLM exits 78
rather than run believing it is relay's model broker upstream when relay
disagrees. The socket serves the same dispatch as `--router-port` minus the
two route families that would forward a caller's own upstream credential off
this box: the `/api/` Claude Code OAuth passthrough and any configured
`router.passthrough` `/<name>/` route both 404 here, and `/v1/messages` only
serves `router.anthropic.modelMap` targets (anything else 404s rather than
reaching the real Anthropic API). See `internal/router/router_socket.go` and
[`../relay/docs/model-endpoint.md`](../relay/docs/model-endpoint.md).

**Behavior change**: every managed-server, endpoint, and virtual-model
dispatch (`--router-port` and router.sock alike — NOT the `/api/` Anthropic
passthrough or a `router.passthrough` `/<name>/` route, which still forward
a caller's real credential byte-for-byte on purpose) now strips an inbound
`X-Api-Key` header, and any `X-Relay-*` header in either direction, before
talking to the actual backend — see CLAUDE.md's Relay-router section for
why. Nothing in this repo relies on forwarding `X-Api-Key` to a managed
server or OpenAI endpoint, but a standalone client that previously got away
with sending its own upstream API key that way will need to configure it in
`settings.json`'s `openai.endpoints[].apiKey` instead.

Configure `virtual-llms` in `settings.json` to expose a stable model name backed
by an ordered list of fallback targets. An endpoint target reuses a name from
`openai.endpoints` and its `/models` health check is cached for 15 seconds; an
`alias` target selects a local managed llama.cpp or MLX model.

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

Declared order is a *preference*, not a hard gate: targets currently believed
reachable are tried first, but every configured target is still attempted as a
last resort, and a request retries the next target on any failure that occurs
before a response byte is sent to the client. A request for a configured
virtual name only fails (503) when every target genuinely fails; it never
reads as "unknown model". The name always appears in `/v1/models`, with
`status`/`meta`/`architecture`/`context_length` inherited from whichever
target would be tried first — even when every target is currently offline
(`status.value` reports `"unloaded"` with `failed: true` in that case).

A conversation sticks to whichever target first serves it: once a target has
answered, the router pins the conversation to it for as long as the client
keeps sending the same `prompt_cache_key` (or `user`, if that's absent), and
a pinned target always wins over the preference ordering above — a
reachability wobble must not hop an established conversation to a different
backend, since two backends encode reasoning differently and cannot share a
transcript (see [CLAUDE.md](CLAUDE.md#relay-router-internalrouterroutergo) for the
full incompatibility matrix behind this).
No client identifier means no pin — same behavior as before this existed. A
pin expires after an hour of disuse, or falls back to the remaining
candidates immediately if its target has since been removed from config.
Details in [CLAUDE.md](CLAUDE.md#relay-router-internalrouterroutergo).

Configure `router.reasoningEffortMap` in `settings.json` to rewrite a
`reasoning_effort` value before it reaches a backend, e.g. `{"minimal":
"none"}` for a llama.cpp server that 500s on `"minimal"` but treats `"none"`
as off. Absent or empty (the default) disables rewriting entirely. Mapping a
value to `""` removes the field instead of sending it empty. Applies to
every proxied path — managed alias, endpoint, and virtual model alike.

That value swap only fixes backends that interpret `reasoning_effort`
server-side (llama.cpp). oMLX instead forwards it verbatim into the model's
chat template, so turning reasoning off there needs a different field:
`router.reasoningEffortTemplateKwargs`, e.g. `{"minimal": {"enable_thinking":
false}}`, merges that object into the body's top-level
`chat_template_kwargs` when the request's original `reasoning_effort` value
matches a configured key, without clobbering any key the client's own body
already sets. Configure both knobs together for a value that reliably turns
reasoning off across both backend families. Also absent/empty by default.
Details in [CLAUDE.md](CLAUDE.md#relay-router-internalrouterroutergo).

Configure `router.anthropic` to let Claude Code (the `claude` CLI) point at
the router via `ANTHROPIC_BASE_URL` — real Claude models proxy through to
`api.anthropic.com` byte-for-byte (headers, body, streaming), and
`modelMap` optionally redirects specific model ids to a local
managed/virtual/endpoint model, translated through a full Anthropic↔OpenAI
compatibility layer. Absent by default (zero behavior change). Example:

```json
"router": {
  "anthropic": {
    "modelMap": {"vCode": "host/vCode"}
  }
}
```

```bash
# real Claude, unmodified
ANTHROPIC_BASE_URL=http://127.0.0.1:8180 claude -p "hi"
# redirected to the local "vCode" target above
ANTHROPIC_BASE_URL=http://127.0.0.1:8180 claude --model vCode -p "hi"
```

See `internal/router/router_anthropic_translate.go`'s file header for what's
translated and what's deliberately out of scope, and
[CLAUDE.md](CLAUDE.md#relay-router-internalrouterroutergo) for the full config
reference including the `ANTHROPIC_CUSTOM_MODEL_OPTION` client-side setting
needed to make a redirected model selectable in Claude Code's own `/model`
picker.

## API

The authenticated API is a small Unix-socket HTTP surface, bearer-protected
by `--token`/`RELAY_LLM_TOKEN`. Treat it as a public API for direct
(standalone) callers; through relay, the front-door dispatcher reaches it
per the registered manifest. Route constants live in `internal/api/api.go`.

The optional `--http-port` TCP listener is a *different, much smaller*
surface: a read-only diagnostics allowlist (`GET /status`,
`/status/status.css`, `/status/status.js`, `/api/status`,
`/api/status/detailed`) for the `/status` dashboard, served anonymously —
see `internal/api/tcp_diagnostics.go`. Everything else 404s there.

- `GET /api/status` — model-manager summary.
- `GET /api/status/detailed` — full JSON status payload backing the `/status` dashboard.
- `GET+DELETE /api/llama/instances[/{alias}]`; `GET+DELETE /api/mlx/instances[/{alias}]` — inspect/stop managed model-server instances.

The relay-router's own OpenAI-compatible surface (`/v1/...`) is a separate
listener, reachable via `--router-port`/`--router-socket` — see
[Relay-router](#relay-router-optional) above, not the bearer-authenticated
socket described here.

## Data

Default `os.UserConfigDir()/relayLLM` (macOS `~/Library/Application Support/relayLLM/`,
Linux `~/.config/relayLLM/`); override with `--data-dir`.

- `settings.json` — unified provider config (falls back to `openai_endpoints.json` + `llama_models.json`, then env).

Terminal templates (relay-sessions' PTY launch shapes, not spawned by
relayLLM itself) live in the `pty` section of `settings.json`, retained only
as an editable on-disk format; relay's config editor manages them.

## Testing

```bash
go test ./...              # hermetic (no external deps, no subprocess, no network)
go test -tags=live ./...   # opt-in: needs Ollama / LM Studio / oMLX / relay running
go test -tags=llm ./...    # opt-in: real llama-server against an installed GGUF
```

The hermetic tier covers the HTTP status API, managed-server lifecycle,
relay-router dispatch, and manifest registration via fakes
(`internal/testutil` / `internal/api/testserver_test.go`). Install the
pre-commit hook once: `git config core.hooksPath .githooks`.

## Service manifest

relayLLM detects its run mode from `RELAY_BRIDGE_SOCKET`:

- **Standalone** (unset) — binds its own socket, serves direct clients.
- **Enhanced** (set) — same listener and wire language, plus it dials relay's
  bridge with a `RegisterManifest` payload declaring its routes, internal socket
  + relayLLM's own bearer token, status endpoint, and actions. Bridge requests
  carry no relay credential: relay recognises relayLLM by the launch identity
  bound at `Hello`. relay's dispatcher forwards matching
  front-door traffic over that socket.

The mode switch is a deployment fact, not a code fork — one config loader, two
sources. Protocol contract: [`../relay/docs/service-manifest.md`](../relay/docs/service-manifest.md).

## Ecosystem

- **[relay](https://github.com/barelyworkingcode/relay)** — orchestrator + front-door dispatcher; spawns relayLLM and routes traffic per registered manifests.
- **relay-sessions** — hosts LLM sessions, terminals, and the permission hook; the surface this repo used to serve before narrowing to model-hosting only.
- **[relayScheduler](https://github.com/barelyworkingcode/relayScheduler)** — runs scheduled tasks against terminal templates.
- **[relayTelegram](https://github.com/barelyworkingcode/relayTelegram)** — Telegram bot bridge.
- **[relayComfy](https://github.com/barelyworkingcode/relayComfy)** — ComfyUI service; image generation reaches it as the `comfyui` MCP tool through relay.

## License

[MIT](./LICENSE)
