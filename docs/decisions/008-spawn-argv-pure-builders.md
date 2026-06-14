# 008 — Provider spawn argv/env as pure builders

Status: Accepted

## Context

The Claude and pi providers construct a subprocess command line (and child
environment) from session state: permission mode, headless flag, resume id,
policy tools, model routing, skill mounts, project token. This logic was inline
in each provider's `Start()`, immediately before `exec.Command(...)`. The only
way to exercise it was to actually spawn the binary, so it lived exclusively in
the `//go:build live` / `//go:build llm` tiers and was absent from the hermetic
suite (`go test ./...`, the pre-commit gate).

That left the repo's most security-sensitive branch — the `bypassPermissions` /
`--dangerously-skip-permissions` / `RELAY_LLM_HEADLESS=true` escape hatch, which
makes the hook auto-approve every tool call — with no default-tier regression
guard. A refactor that leaked headless mode to an interactive session, or
dropped `--model`, would ship green.

Two ways to make it hermetic: (a) fake the `exec` boundary, or (b) extract the
assembly into pure functions.

## Decision

Extract the assembly into pure methods that take inputs and return data:
`buildClaudeArgs(mcpCfg) []string`, `buildClaudeEnv(base, mcpToken) []string`,
`effectivePermissionMode()`, `buildPiArgs(subs, sessionDir, skillDir)`, plus
`resolveSkillDir()` to isolate the one filesystem probe. `Start()` calls them,
then spawns. No `exec` fake, no interface, no production test seam exposed
(consistent with ADR-003's "factory + interface" only-when-needed stance — here
a plain function is enough).

## Consequences

- The spawn flag matrix is asserted in the hermetic tier
  (`provider_claude_spawn_test.go`, `provider_pi_spawn_test.go`); the security
  invariants are re-pinned in `security_regression_test.go` (relay's
  `TestSec_*` convention). A mutation that leaks headless to a default session
  now fails the default suite.
- `effectivePermissionMode()` is the single source of truth gating both the flag
  and the env var, so the escape hatch can no longer be half-applied.
- Process spawn, I/O wiring, and lifecycle remain live-only — the seam stops at
  argv/env assembly, which is where the regressions actually were.
