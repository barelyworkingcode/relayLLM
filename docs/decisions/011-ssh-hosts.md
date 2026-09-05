# ADR-011: SSH hosts — relayLLM's half

**Status:** Accepted
**Date:** 2026-09-05

## Context

Full design and wire contract: [`../relay/docs/ssh-hosts.md`](../../../relay/docs/ssh-hosts.md).
That document is authoritative for `ssh_argv`, `HostSpec`, `RemoteCommand`'s
launcher form, and the cross-repo split between relay, relayLLM and eve. This
ADR records only the relayLLM-side decisions that document leaves to this
repo's judgment.

## Decisions

**Permissions ride Claude's own stdio, not the hook.** A host has no
PreToolUse hook binary or bridge socket. `--permission-prompt-tool stdio`
turns each tool call into a `control_request` on Claude's stdout;
`processLine`'s new `"control_request"` case answers it with a
`control_response` on stdin. A session's `Policy` (`MatchToolRule`, deny then
allow) short-circuits exactly like `/api/permission` before ever registering a
pending request. Everything past that point — the pending-request map, the
`permission_request` event pushed to viewers, resolution via
`permission_response` — is the same `PermissionManager` the hook path already
uses; a host session and a console session's decisions are indistinguishable
to Eve. `ClaudeProvider.Kill()` denies every pending request for its session
rather than letting it burn the 60s timeout, since the process that would
read the `control_response` is gone either way.

**No relay MCPs on a host in v1.** `buildClaudeArgs` never emits
`--mcp-config` for a host session, and `buildHostExec`'s child env carries
only `RELAY_LLM_SESSION_ID` — no hook socket, no hook token, no relay token of
any kind. A host session gets Claude Code's built-in tools only.

**`pi` is refused on a host project.** pi's overlay writes into the project
directory and symlinks into the console's home; neither exists on a host.
`CreateSession` checks this before ever building a provider, so the error
(`provider "pi" is not available on a host project`) is immediate and
specific rather than a confusing failure three layers down.

**History and delete run over ssh, behind a func var.** `readClaudeHistory`
and `DeleteSession` `cat`/`rm -f` a host session's JSONL transcript via
`runSSHCommand`, a package-level func var so tests stub the ssh invocation
without a real remote host. A host path is never `EvalSymlinks`'d — it can't
be resolved from the console — and any ssh failure yields empty history
rather than a failed join; a briefly-unreachable host shouldn't block opening
a session it already has metadata for.

**Host is re-resolved at every Claude spawn, not just at session create.**
`ClaudeProvider.Start` re-fetches the session's `Host` from relay's bridge
before deciding which exec path to take, falling back to the already-stored
value on any bridge failure. This is what lets a persisted host session
resume correctly after a relayLLM restart while relay is briefly
unreachable, and picks up a probe update (a new `claude_path` after
re-probing) without requiring a fresh session. A terminal's `Host`, by
contrast, is resolved once at `TerminalManager.Create` and never refreshed —
an interactive shell's whole life is usually shorter than the gap between two
probes, and there is no "respawn" concept for a terminal the way there is for
a headless CLI restart.

## Key files

- `sshhost.go` — vendored `RemoteCommand`/`RemoteShellCommand` (decision 8 in
  the design doc), pinned against the doc's Fixtures section.
- `provider_claude.go` — `buildHostExec`, `handleControlRequest` and the
  `control_response` builders.
- `claude_history.go` — `readClaudeHistoryOverSSH`, `deleteClaudeHistoryOverSSH`,
  `runSSHCommand`.
- `terminal_session.go` — `buildHostCmd`, `buildHostTerminalExec`.
- `permission.go` — `PermissionManager.WaitForDecision` / `DenyAllForSession`,
  shared by the hook and control_request paths.
