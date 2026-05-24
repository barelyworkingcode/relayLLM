# ADR-002: Three-Tier Testing (default / live / llm)

**Status:** Accepted
**Date:** 2026-05-23

## Context

The original test suite mixed three kinds of tests: pure unit tests, integration tests that needed running services (LM Studio, OMLX, Ollama, relay), and what we now call "live LLM" tests that needed real model weights. `go test ./...` would skip-or-fail unpredictably depending on which services happened to be running on the dev's machine. CI was impossible; local runs were noisy.

## Decision

Three tiers gated by Go build tags:

| Command | What runs | Requirements |
|---------|-----------|--------------|
| `go test ./...` | Hermetic suite | None — pure Go, no network, no subprocess. ~1.5s. |
| `go test -tags=live ./...` | Legacy integration | Ollama / LM Studio / OMLX / relay binary running locally. Manual smoke only. |
| `go test -tags=llm ./...` | Real LLM execution | `Qwen3.6 MoE 35` in user's `settings.json` (llama tests) + `claude` CLI on PATH (claude tests). |

The hermetic suite is the only one gated by the pre-commit hook (`.githooks/pre-commit`). The other two are opt-in: heavyweight, slower, environment-dependent.

## Consequences

- **Good:** `go test ./...` is reliable and fast — every dev / agent / hook can rely on it.
- **Good:** Live integration tests aren't lost — quarantined behind a tag instead of deleted.
- **Good:** Real-LLM tests prove the things fakes can't (SSE chunking, llama-server lifecycle, real tool-call quality) without bleeding into the default suite.
- **Bad:** Three tags means three things to remember when adding tests. Mitigated by the "Adding tests" section in CLAUDE.md.
- **Trade-off:** Default suite uses fakes (FakeProvider, FakeMCPClient, FakeClock). When fakes drift from production behavior, tests pass but production breaks — caught by the `llm` tier when run periodically.

## See also

- `.githooks/pre-commit` — enforcement for the default suite
- `support_test.go`, `support_server_test.go` — testsupport infrastructure
- `provider_llama_live_test.go`, `provider_claude_live_test.go` — `-tags=llm`
- `integration_test.go`, `provider_openai_integration_test.go`, `stop_generation_test.go`, `mcp_integration_test.go`, `benchmark_test.go` — `-tags=live`
- ADR-003 — test seam patterns this enables
