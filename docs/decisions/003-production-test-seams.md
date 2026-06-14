# ADR-003: Production Test Seams

**Status:** Accepted
**Date:** 2026-05-23

## Context

The default (hermetic) test tier from ADR-002 must substitute the LLM provider, the MCP client, and the clock — without a dependency-injection framework and without leaking test machinery into the production API. The relay bridge is handled separately (faked via env wiring + `FakeBridge` in `manifest_test.go`), not through an injected seam.

## Decision

Three narrow, intentionally test-only seams plus one interface, each with a single consumer:

1. **`SessionManager.SetProviderFactory(f)`** (`session.go`) — short-circuits the built-in switch in `initProvider`. Tests inject a `FakeProvider` that scripts canonical events.
2. **`SessionManager.SetMCPClientFactory(f)`** (`session.go`) — overrides the MCP client `BaseChatProvider` would build from session settings. Tests inject `FakeMCPClient` to script tool-call responses.
3. **`PermissionManager.SetClock(c)`** (`permission.go`) — swaps the clock behind the 60s permission timeout (`api.go`, `clock.After(60 * time.Second)`). Tests use `FakeClock` to advance time deterministically.
4. **`MCPClient` interface** (`mcp_client.go`) — `BaseChatProvider.mcpManager` is typed as the interface, not `*MCPManager`, so production (`MCPManager`) and `FakeMCPClient` both satisfy it.

The setters carry "test-only" doc comments and are never called in production code.

### Deliberately rejected

- **No `Clock` injection at TTFT/TPS sites.** Those are metrics-only; no test gates on them.
- **No `exec.Command` factory for subprocess spawn.** The live tier exercises real spawn; faking it would add production complexity for marginal hermetic value.
- **No `PTYSpawner` abstraction.** Terminal tests spawn a real shell — fast enough.
- **No `http.Client` injection beyond what already existed.**

## Consequences

- **Good:** Production code stays close to its non-test shape; seams are obvious (named `Set*`, commented test-only).
- **Good:** Tests call a setter and run the code path — no framework knowledge.
- **Good:** Each seam has exactly one consumer (the four above are exhaustive).
- **Bad:** Easy to forget a seam, letting a real `ServerManager` or `claude` subprocess spawn in a unit test. Mitigated by `NewTestServer` (`support_server_test.go`), which wires the common case.
- **Trade-off:** A nullable `providerFactory` / `mcpClientFactory` field rides on `SessionManager`, nil in production — one branch per init, cost negligible.

## When to add a new seam

All three must hold:

1. A planned default-tier test needs to control this piece.
2. The seam is a small interface or setter, not a DI rewrite.
3. Production shape doesn't get worse — the seam is invisible unless invoked.

If you can't meet all three, the test belongs in the `live` or `llm` tier.

## See also

- ADR-002 — the three test tiers this serves.
- ADR-007 — `ServerManager` (formerly the llama-only manager).
- `support_test.go` / `support_server_test.go` — `FakeProvider`, `FakeMCPClient`, `FakeClock`, `NewTestServer`.
