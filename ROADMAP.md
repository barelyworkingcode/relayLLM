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
| Low | `LlamaServerManager.StopAll` on test cleanup may terminate orphan llama-server | small | Edge case. Detect by PID provenance if it becomes a real annoyance. |

## Open — process / infra

(none open)

## Cross-repo (tracked here for visibility, owned elsewhere)

| Item | Owner | Notes |
|------|-------|-------|
| Bridge wire types duplicated across `../relay/bridge/manifest.go` and `relayLLM/manifest.go` | relay | Shared Go module or codegen from a single source. Brittle today. |
| Relay menubar status panel (consumes relayLLM manifest's status/actions) | relay | Original feature request that drove the manifest refactor. Spec lives in relayLLM git history at commit `02312ef`. UI must consume the manifest, not hardcode "relayllm" (see [ADR-004](docs/decisions/004-no-service-carveouts.md)). |
| Eve + relay + relayLLM + mock-LLM end-to-end integration tier | platform | No system-level confidence today. Either nightly on a dev box or stays manual. |
| Single trace-id from Eve → relay → relayLLM → MCP | platform | Today debugging means tailing 5+ log files. Big observability win. |
| Pre-commit hook pattern in `../relay`, `../eve`, `../relayScheduler` | each repo | Apply the `.githooks/pre-commit` pattern landed here. Without it, sibling suites drift. |

## Closed (recent — for context)

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
