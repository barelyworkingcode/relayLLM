# relayLLM Roadmap

Durable backlog for relayLLM. Survives chat session compaction; visible to any contributor (human or AI). Update as state changes — close items as they ship, add new ones as they're identified.

For *why* current architectural choices were made, see [`docs/decisions/`](docs/decisions/).

## In flight

(none — between work items. Add as you start something.)

## Open — test coverage gaps

| Priority | Item | Rough size | Notes |
|----------|------|-----------|-------|
| Low | `provider_claude.go` permission/hook integration tests | medium | Hook subprocess + permission flow (the *runtime* half — argv/env assembly is now hermetic, see ADR-008). Requires the installable hook binary (`cmd/hook/`); defer until next time the hook path is touched. |
| Low | `config.go` unified-loader fallback chain | small | settings.json → legacy json → env (`LoadConfig` / `parseUnifiedConfig`, no `config_test.go` today). Table-test the precedence + `~` expansion; regression-prone "two sources, one loader" merge. |

## Open — production debt

| Priority | Item | Rough size | Notes |
|----------|------|-----------|-------|
| Low | Relay-router `GET /models/sse` not implemented | small | llama.cpp router mode streams load/download progress here. pi's client swallows the failure and falls back to polling `/models`, so the only loss is progress UI during a load. Needs a pub/sub from `ServerManager` state changes. |
| Medium | Relay-router `/v1/models` omits Ollama | small | The router lists managed aliases + OpenAI endpoints only; `/api/models` is the complete list. Ollama is an HTTP upstream like the rest and would slot in. Claude/pi can't be proxied OpenAI-style. |
| Low | Compute-buffer term in the memory estimate is a flat headroom % | small | Real cost scales with `ubatch-size` and the graph's widest node. If the 10% default proves wrong at ubatch 4096, measure a few models and fit something better (ADR-009). |
| Low | `ServerManager.StopAll` on test cleanup may terminate orphan llama-server / mlx-serve | small | Edge case. Detect by PID provenance if it becomes a real annoyance. |
| Low | mlx-serve vision models: per-model attachment capability | small | `mmproj` detection (`server_manager.go` `ListModels`) is llama-specific; mlx configs have no equivalent flag yet. Revisit when a multimodal MLX model is configured. |

## Open — process / infra

(none open)

## Cross-repo (tracked here for visibility, owned elsewhere)

| Item | Owner | Notes |
|------|-------|-------|
| Bridge wire types duplicated across `../relay/bridge/manifest.go` and `relayLLM/manifest.go` | relay | Shared Go module or codegen from a single source. Brittle today — `ActionDecl.ForEach` was added to both sides manually in a coordinated PR with the Service Inspector landing. |
| oMLX `/v1/models` does not declare `architecture.input_modalities` | oMLX | oMLX detects VLMs internally (`model_discovery.py`) but its catalog emits only `max_model_len`. relayLLM now passes the field through when present, so adding it upstream is all that stands between an oMLX vision model and pi being willing to send it images. Until then endpoint models read as text-only. |
| Eve + relay + relayLLM + mock-LLM end-to-end integration tier | platform | No system-level confidence today. Either nightly on a dev box or stays manual. |
| Single trace-id from Eve → relay → relayLLM → MCP | platform | Today debugging means tailing 5+ log files. Big observability win. |
| Pre-commit hook pattern in `../relay`, `../eve`, `../relayScheduler` | each repo | Apply the `.githooks/pre-commit` pattern landed here. Without it, sibling suites drift. |

## Closed (recent — for context)

- Virtual LLM failover (`virtual-llms` in settings.json): a stable model name with ordered fallback targets, resolved by the relay-router. Shipped, then hardened after `vCode` returned `400 unknown model` in the field. Three defects: endpoint reachability was treated as a hard gate on a cache that is 15s stale by design, so a healthy endpoint stayed unroutable after recovering; `ProxyRegistry.probe` inherited the inbound request's context, so a client disconnect recorded a false "offline" and poisoned that cache; and failover happened only at selection time, leaving remaining targets untried when one died mid-request. Reachability is now a preference ordering — believed-usable targets first, the rest as last-resort attempts — with retry on any failure that lands before the first response byte, a 3s dial cap so a black-holing host cannot eat 30s per candidate, and 503-naming-what-was-tried instead of `unknown model` for a name that is static config. Virtual rows now always appear in `/v1/models` and inherit `input_modalities` + `meta.n_ctx` from their first candidate.

- Vision capability now reaches pi. Two independent gaps: the pi project overlay wrote bare `{"id": ...}` rows, and pi's `openai-completions` provider does no discovery — it gates image attachments on the model's `input` array (`model-config.js`), so every model read as text-only and pi answered "Current model does not support images" even for a model launched with an `mmproj`. The overlay now states `input: ["text"]` / `["text","image"]` explicitly. Separately, endpoint-backed rows in the router catalog carried no `architecture` key at all, which a client reading it unconditionally would choke on; they now do, populated from the upstream's own `architecture.input_modalities` when it declares one (llama.cpp router mode does). We never infer vision — an upstream that stays quiet reads as text, because sending images to a server that cannot take them fails mid-turn.

- Managed-server memory budget: `maxLoaded` + `maxMemoryGB` caps with LRU eviction, per-turn leases so eviction never lands mid-generation, and an idle reaper (`idleTimeoutMinutes`). Model sizes are computed from GGUF headers / MLX `config.json` — including sliding-window attention and per-layer GQA, without which Gemma 4 12B over-estimates ~25x. Session transports now resolve their backend per turn (`BackendResolver`), which also fixes sessions being pinned to a dead port after a server crash. New `gguf.go` + `server_memory.go`; budgets surfaced in `/api/status`. See ADR-009.

- `mcp_client.go` `MCPManager` 0% → ~88% hermetic via the SDK's `NewInMemoryTransports` (real in-process MCP server). Extracted `newMCPClient()` so the test drives the production progress-notification handler. Subprocess `Start()` stays live-tier (`mcp_integration_test.go`). `mcp_client_test.go`.
- Pre-commit hook now runs the hermetic suite under the race detector (`go test -race ./...`). ~+1s steady-state. Caught the `TestWS_RenameSession_UpdatesName` race.
- Hermetic regression coverage for provider spawn + the auth boundary. Argv/env assembly extracted into pure builders (`buildClaudeArgs`/`buildClaudeEnv`/`buildPiArgs`/`resolveSkillDir`); the `bypassPermissions` / headless escape hatch is now asserted on the default tier. New `provider_claude_spawn_test.go`, `provider_pi_spawn_test.go`, `security_regression_test.go` (`TestSec_*`). See ADR-008.
- MLX provider via [mlx-serve](https://github.com/ddalcu/mlx-serve). Generalized `llama_manager.go` into the profile-driven `server_manager.go` (`ServerManager` + `ServerProfile`). `mlx/{alias}` routing, `mlxInstances` status array, `stop-mlx` action, `/api/mlx/instances` routes. See ADR-007.
- Retired relayLLM's spawn-time skill regeneration + the `skillPath` config — regeneration is owned by relay now. Skills load from the convention `<project>/.claude/skills`. See ADR-006. Coordinated with relay + eve.
- Wrapped Claude lost `RELAY_TOKEN` across a relayLLM restart. `provider_claude.go` now resolves the project token just-in-time via `resolveMCPToken()` → `resolveProjectToken` (the `Session.McpToken` field was retired). `provider_claude_relaymcp_test.go`.
- Relay menubar status panel landed as a generic **Service Inspector**. `/api/status` embeds `instances` (replaced the old `llamaInstances` count); relay renders any registered manifest with zero hardcoded service IDs. `ActionDecl.ForEach` added to both bridge mirrors. See ADR-005.
- WS event-type constants migration finished (`ws_messages.go`) — all inbound/outbound envelope strings go through `WSMsg*` constants in production code; tests keep literals as wire-contract pins.
- Test suite overhaul: hermetic default tier, three-tier convention. See ADR-002.
- ADRs seeded (manifest protocol, three-tier testing, test seams, no-carveouts). ADR-001 through ADR-004.

## How to use this file

- Open items are grouped by priority within each section. Lower priority = lower expected ROI right now, not "less important."
- When you start an item, move it to **In flight**; add an owner if more than one person is working this repo.
- When you close an item, move it to **Closed** with a one-line note + ADR link for detail. Trim Closed periodically (keep ~10 entries).
- New items: state scope ("small/medium/large") and rationale (why it matters, what's the trigger).
- Cross-repo items stay here for visibility but are owned elsewhere — don't fix them in this repo.
