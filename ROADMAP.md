# relayLLM Roadmap

Durable backlog for relayLLM. Survives chat session compaction; visible to any contributor (human or AI). Update as state changes — close items as they ship, add new ones as they're identified.

For *why* current architectural choices were made, see [`docs/decisions/`](docs/decisions/).

## In flight

(none — between work items. Add as you start something.)

## Open — test coverage gaps

| Priority | Item | Rough size | Notes |
|----------|------|-----------|-------|
| Low | `provider_claude.go` permission/hook integration tests | medium | Hook subprocess + permission flow. Requires installable hook binary; defer until next time the hook path is touched. |
| Low | `comfyui_client.go` + `generate_image` flow | small | External-service integration; belongs in a future cross-repo integration tier, not the relayLLM suite. |

## Open — production debt

| Priority | Item | Rough size | Notes |
|----------|------|-----------|-------|
| Low | `ServerManager.StopAll` on test cleanup may terminate orphan llama-server / mlx-serve | small | Edge case. Detect by PID provenance if it becomes a real annoyance. |
| Low | mlx-serve vision models: per-model attachment capability | small | `mmproj` detection is llama-specific; mlx configs have no equivalent flag yet. Revisit when a multimodal MLX model is configured. |

## Open — process / infra

(none open)

## Cross-repo (tracked here for visibility, owned elsewhere)

| Item | Owner | Notes |
|------|-------|-------|
| Bridge wire types duplicated across `../relay/bridge/manifest.go` and `relayLLM/manifest.go` | relay | Shared Go module or codegen from a single source. Brittle today — `ActionDecl.ForEach` was added to both sides manually in coordinated PR with the Service Inspector landing. |
| Eve + relay + relayLLM + mock-LLM end-to-end integration tier | platform | No system-level confidence today. Either nightly on a dev box or stays manual. |
| Single trace-id from Eve → relay → relayLLM → MCP | platform | Today debugging means tailing 5+ log files. Big observability win. |
| Pre-commit hook pattern in `../relay`, `../eve`, `../relayScheduler` | each repo | Apply the `.githooks/pre-commit` pattern landed here. Without it, sibling suites drift. |

## Closed (recent — for context)

- MLX provider via [mlx-serve](https://github.com/ddalcu/mlx-serve) (native Zig + mlx-c, no Python). Generalized `llama_manager.go` into a profile-driven `server_manager.go` (`ServerManager` + `ServerProfile`; `llamaProfile` / `mlxProfile`) instead of duplicating the process-lifecycle code. New `mlx-serve` settings section (same schema as `llama-server`, `model` = MLX model directory), `mlx/{alias}` session routing, bare mlx aliases in the relay-router (llama wins collisions), `mlxInstances` status array + `stop-mlx` manifest action + `/api/mlx/instances` routes, `--mlx-serve-path` flag. Implementation delegated to piWorker (2 rounds), reviewed + verified here: hermetic suite, `-tags=llm` (real llama-server through the refactored manager, 29s), live mlx e2e conversation. See `docs/decisions/007-profile-driven-server-manager.md`.
- Retired relayLLM's spawn-time skill regeneration + the bespoke `skillPath` config. Skill *generation* is owned by relay (relay ADR-004); relayLLM regenerating `SKILL.md` on every PTY/pi spawn was redundant. Removed `autoRegenSkills` + `skillPath` from `TerminalTemplate`/`PiConfig`/`RelayManagedSpec` (+ the `${SKILL_PATH}`/`${SKILLS_ROOT}` substitutions). Skills load from the convention `<project>/.claude/skills`: pi `--mode rpc` auto-appends `--skill <project>/.claude/skills`; PTY templates use `${PROJECT_PATH}/.claude/skills` (e.g. `pidev`). Follow-up (coordinated with relay): retired the now-vestigial `regen_skills`/`skill_path` fields from the `ResolvePtyEnv` bridge contract — it's now a pure project-token+workdir resolver. relay owns every regen trigger: **relay startup** (`regenProjectSkills` for `GenerateSkill` projects), project save, MCP reconcile, and a manual per-project `POST /api/projects/{id}/regen_skill` surfaced in eve's project kebab menu ("Regenerate Skills"). See `docs/decisions/006-skill-regen-owned-by-relay.md`. Live-config migration: `pidev` template args `${SKILLS_ROOT}` → `${PROJECT_PATH}/.claude/skills`. Cross-repo: relay (`trayapp.go`, `router.go`, `bridge/types.go`) + eve (`project-panel.js`, `api-client.js`).
- Wrapped Claude lost `RELAY_TOKEN` across a relayLLM restart. `Session.McpToken` is `json:"-"` (never persisted), so a Claude session *resumed* after a restart spawned with no project token → Bash-driven `relay mcp call ...` skills failed with "RELAY_TOKEN not set". Fix: `provider_claude.go` resolves the token via `resolveMCPToken()` — prefers in-memory `McpToken`, else re-fetches from relay's bridge (`ResolvePtyEnv`, same source the terminal path uses); restart-safe + rotation-safe. The resolved token feeds both Claude's env and the `--mcp-config` child. Tests in `provider_claude_relaymcp_test.go` (session-token-wins, bridge-fallback, standalone-empty). Verified end-to-end via Playwright against a resumed session. Note: pi has the same shape but its primary path already re-resolves from the bridge when `useRelayToken` is set; only its `McpToken` fallback (non-relay-managed pi) shares the gap.
- Generated-image links clickable in Eve's xterm terminal. `generate_image` results now carry absolute `path` + `file://` `file_url` alongside the relative `image_url` (relayLLM `builtin_tools.go`, relayComfy `mcp/client.go`, pi skill doc), and Eve registers a custom xterm link provider (`terminal-manager.js → registerGeneratedImageLinks`) that linkifies `/api/generated/…` tokens into the existing fullscreen overlay (`message-renderer.js → openImageFullscreen`). Root cause: WebLinksAddon only linkifies http(s) and xterm has no inline-image protocol, so the relative URL was a dead string in the terminal — fixable client-side, not via metadata alone. Additive JSON fields; native renderer + Claude/pi paths unchanged. Coordinated across relayLLM + relayComfy + eve.
- Relay menubar status panel landed as a generic **Service Inspector** in relay's settings window. relayLLM declares an `Actions` block with `forEach: "instances"`; relay's `service_status_client.go` + `service_status_poller.go` + `ipc_service_action.go` render any registered manifest with zero hardcoded service IDs (ADR-004 honored). Wire change: `/api/status` now embeds `instances` (replaces `llamaInstances` count). `ActionDecl.ForEach` extension added to both bridge manifest mirrors (relay + relayLLM).
- WS event-type constants migration finished (`ws_messages.go`) — all 36 inbound/outbound envelope strings now go through `WSMsg*` constants in production code; tests intentionally keep literals as wire-contract pins
- CLAUDE.md: "Releases & consumers" section — release model (continuous from main), consumers (Eve, scheduler, standalone CLI), "done" definition, coordinated-PR breaking-change protocol
- Test suite overhaul: hermetic default tier, three-tier convention (ADR-002), 166+ tests
- `waitForHealth`-adopts-external-process bug fix (preflight port check)
- Event-type constants (`HandlerLLMEvent`, etc.) replacing stringly-typed dispatch
- Pre-commit hook (`.githooks/pre-commit`) — build + vet + hermetic tests
- `LlamaServerManager` unit tests (args builder, port allocator, alias lookups)
- `relay_router.go` tests (zero → full dispatch coverage)
- Ollama think-suppression test (per memory: must be explicit, not omitted)
- OpenAI SSE edge cases (multi-tool, interleaved, malformed)
- `SessionManager.SetMCPClientFactory` test seam + live tool-call round-trip via real LLM
- Claude provider live tests using Haiku (text round-trip + session resume)
- ADRs seeded (manifest protocol, three-tier testing, test seams, no-carveouts)

## How to use this file

- Open items grouped by priority within section. Lower priority = lower expected ROI right now, not "less important."
- When you start an item, move it to **In flight** and add an owner if more than one person is working on this repo.
- When you close an item, move it to **Closed** with a one-line note. Trim Closed periodically (keep the last ~10 entries for context).
- New items: add to the appropriate section. Be concrete about scope ("small/medium/large") and rationale ("why does this matter, what's the trigger?").
- Cross-repo items stay here for visibility but are explicitly owned elsewhere — don't fix them in this repo.
