# ADR-006: Skill Regeneration Owned by Relay; Drop the `skillPath` Config

**Status:** Accepted
**Date:** 2026-06-09

## Context

relayLLM's relay-managed spawn path (`RelayManagedSpec.Resolve()` in `relay_spawn.go`, used by both
PTY terminal templates and the pi `--mode rpc` provider) used to ask relay to **regenerate** a
project's `SKILL.md` on every spawn. Two config fields drove it:

- `autoRegenSkills` (`always`/`skipIfExists`/`never`) — the regen trigger, sent over relay's bridge
  `ResolvePtyEnv` as `RegenSkills`.
- `skillPath` (e.g. `${project.path}/.claude/skills/relay`) — told relay *where* to write `SKILL.md`,
  echoed back to derive a `--skill <dir>` argv for pi (via `${SKILL_PATH}`/`${SKILLS_ROOT}`).

Both were redundant. Relay already owns skill generation outright (relay ADR-004: *"Skill generation
already lives in relay … RelayLLM only consumes the generated `SKILL.md`."*) and regenerates on its
own triggers, so the spawn-time call duplicated work. And `skillPath` was more knob than the problem
needs: the skills directory is *always* `<project-root>/.claude/skills` by convention, and relayLLM
already exposes the project root via the `${PROJECT_PATH}` substitution.

## Decision

1. **Remove `autoRegenSkills`** from `TerminalTemplate`, `PiConfig`, `RelayManagedSpec`, and the
   settings schema (`manifest.go`). relayLLM never triggers skill regeneration; relay owns it.
2. **Remove `skillPath`** (and the `${SKILL_PATH}`/`${SKILLS_ROOT}` substitutions, and
   `SpawnSubs.SkillPath`). Skills load from the convention `<project-root>/.claude/skills`:
   - pi `--mode rpc` auto-appends `--skill <project-root>/.claude/skills` when the dir exists and the
     user didn't already wire `--skill` via `extraArgs` (`provider_pi.go`).
   - PTY templates reference `${PROJECT_PATH}/.claude/skills` in their args (e.g. the `pidev` template).
3. `RelayManagedSpec.Resolve()` now contacts relay's bridge purely to fetch the **project-scoped
   token** and resolved project path. The `ResolvePtyEnv` contract dropped its now-dead
   `regen_skills`/`skill_path` request fields and `skill_path` response field in a coordinated
   relayLLM + relay change (`relay_bridge_client.go` + `../relay/bridge/types.go`); it is now a pure
   token+workdir resolver.

## Where relay regenerates now

Relay owns every trigger: on relay startup for each `GenerateSkill: true` project
(`trayapp.go` → `regenProjectSkills`), on project create/update, after MCP reconcile, and on an
explicit per-project regen (`POST /api/projects/{id}/regen_skill`, surfaced in eve's project kebab as
"Regenerate Skills").

## Consequences

- **Good:** One less thing relayLLM does per spawn, and one fewer place skill-generation policy lives —
  relay is the single owner. No bespoke `skillPath` knob to misconfigure.
- **Migration:** A PTY template whose args used `${SKILLS_ROOT}` must switch to
  `${PROJECT_PATH}/.claude/skills` — `${SKILLS_ROOT}` no longer resolves and would reach pi as a
  literal. The inert `autoRegenSkills`/`skillPath` keys can be deleted from `settings.json` at leisure;
  as unknown fields they're silently ignored on unmarshal. No sibling repo consumed the dropped
  substitution tokens.
- **Neutral:** A project that keeps skills elsewhere wires `--skill <dir>` explicitly via `extraArgs`
  (pi) or template args (PTY); the auto-append backs off when `--skill` is already present.

## See also

- relay ADR-004 — project management + skill generation ownership in relay
- `relay_spawn.go::RelayManagedSpec.Resolve` — token-only bridge call
- `provider_pi.go` — pi `--skill` auto-append off the project root
