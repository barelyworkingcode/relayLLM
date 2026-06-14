# Architecture Decision Records

Short notes on architectural decisions that aren't obvious from the code. Each one captures the *why* so a future contributor (human or AI) doesn't have to relearn it.

Format: Context → Decision → Consequences. Keep each ADR under ~300 words; ADRs that grow longer have outgrown the format and should become regular design docs.

| # | Title | Status |
|---|------|--------|
| [001](001-service-manifest-protocol.md) | Service manifest protocol (relay handshake) | Accepted |
| [002](002-three-tier-testing.md) | Three-tier testing (default / live / llm) | Accepted |
| [003](003-production-test-seams.md) | Production test seams (factory + interface pattern) | Accepted |
| [004](004-no-service-carveouts.md) | No service carveouts in relay (IoC for ecosystem integrations) | Accepted |
| [005](005-action-foreach-extension.md) | `ActionDecl.ForEach` — per-row manifest actions | Accepted |
| [006](006-skill-regen-owned-by-relay.md) | Skill regeneration owned by relay; drop the `skillPath` config | Accepted |
| [007](007-profile-driven-server-manager.md) | Profile-driven `ServerManager` (llama-server + mlx-serve) | Accepted |
| [008](008-spawn-argv-pure-builders.md) | Provider spawn argv/env as pure builders (hermetic flag-matrix seam) | Accepted |

When to write a new ADR: a decision was made that future readers might second-guess (e.g. "why did we add this seam? why this interface boundary? why this build tag instead of CI gate?"). Don't ADR routine refactors or library upgrades.
