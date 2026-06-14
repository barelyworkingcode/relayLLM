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
| Low | `ServerManager.StopAll` on test cleanup may terminate orphan llama-server / mlx-serve | small | Edge case. Detect by PID provenance if it becomes a real annoyance. |
| Low | mlx-serve vision models: per-model attachment capability | small | `mmproj` detection (`server_manager.go` `ListModels`) is llama-specific; mlx configs have no equivalent flag yet. Revisit when a multimodal MLX model is configured. |

## Open — process / infra

(none open)

## Cross-repo (tracked here for visibility, owned elsewhere)

| Item | Owner | Notes |
|------|-------|-------|
| Bridge wire types duplicated across `../relay/bridge/manifest.go` and `relayLLM/manifest.go` | relay | Shared Go module or codegen from a single source. Brittle today — `ActionDecl.ForEach` was added to both sides manually in a coordinated PR with the Service Inspector landing. |
| Eve + relay + relayLLM + mock-LLM end-to-end integration tier | platform | No system-level confidence today. Either nightly on a dev box or stays manual. |
| Single trace-id from Eve → relay → relayLLM → MCP | platform | Today debugging means tailing 5+ log files. Big observability win. |
| Pre-commit hook pattern in `../relay`, `../eve`, `../relayScheduler` | each repo | Apply the `.githooks/pre-commit` pattern landed here. Without it, sibling suites drift. |

## Closed (recent — for context)

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
