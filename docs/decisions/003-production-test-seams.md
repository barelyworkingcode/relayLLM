# ADR-003: Production Test Seams

**Status:** Accepted
**Date:** 2026-05-23

## Context

The default test tier (ADR-002) needs to substitute the LLM provider, MCP client, clock, and bridge — without bringing in heavy dependency-injection frameworks and without exposing test machinery in the production API.

## Decision

Four seam patterns, all narrow and intentionally test-only:

1. **`SessionManager.SetProviderFactory(f)`** — replaces the built-in switch in `initProvider`. Tests inject a `FakeProvider` that scripts canonical events.
2. **`SessionManager.SetMCPClientFactory(f)`** — replaces the MCP client `BaseChatProvider` would build from session settings. Tests inject `FakeMCPClient` to script tool-call responses.
3. **`PermissionManager.SetClock(c)`** — swaps the clock used for the 60s permission timeout. Tests use `FakeClock` to advance time deterministically.
4. **`MCPClient` interface** — `BaseChatProvider.mcpManager` is an interface, not `*MCPManager`. Both production and fake implementations satisfy it.

What we deliberately did NOT do:

- No `Clock` injection in TTFT/TPS sites. Those are metrics-only; no test gates on them.
- No `exec.Command` factory for spawning subprocesses. The live tier exercises real spawn; refactoring would add production complexity for marginal hermetic-test value.
- No `PTYSpawner` abstraction. Terminal tests spawn a real shell — fine, fast enough.
- No `http.Client` injection beyond what already existed.

## Consequences

- **Good:** Production code stays close to its non-test shape. The seams are obvious (Set* setters with "test-only" comments).
- **Good:** Tests don't need framework knowledge — just call a setter, run the code path.
- **Good:** Each seam has a single, narrow consumer (the four bullets above are exhaustive).
- **Bad:** Easy to forget to set a seam, leading to a real `LlamaServerManager` or `claude` subprocess spawning in a unit test. Mitigated by the testsupport package providing `TestServer` that wires the common case.
- **Trade-off:** Some production code carries a nullable `providerFactory` / `mcpClientFactory` field that's nil in production. Adds one branch per init; cost negligible.

## When to add a new seam

Three criteria, all must hold:

1. A planned test in the default tier needs to control this piece.
2. The seam can be a small interface or a setter, not a full DI rewrite.
3. The production code shape doesn't get worse — the seam is invisible unless invoked.

If you can't meet all three, the test belongs in the `live` or `llm` tier instead.

## See also

- ADR-002 — the three test tiers this serves
- `session.go` — `SetProviderFactory`, `SetMCPClientFactory`
- `permission.go` + `clock.go` — Clock seam
- `mcp_client.go` — `MCPClient` interface + `FakeMCPClient`
- `support_test.go`, `support_server_test.go` — testsupport infrastructure
