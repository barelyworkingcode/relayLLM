# ADR-006: Skill Regeneration Owned by Relay; Drop the `skillPath` Config

**Status:** Accepted
**Date:** 2026-06-09

## Context

relayLLM's relay-managed spawn path (`RelayManagedSpec.Resolve()` in `relay_spawn.go`, used by both
PTY terminal templates and the pi `--mode rpc` provider) used to ask relay to **regenerate** a
project's `SKILL.md` on every spawn. Two config fields drove it:

- `autoRegenSkills` (`"always"`/`"skipIfExists"`/`"never"`) — the regen trigger, sent over relay's
  bridge `ResolvePtyEnv` as `RegenSkills`.
- `skillPath` (e.g. `${project.path}/.claude/skills/relay`) — told relay *where* to write `SKILL.md`,
  and was echoed back to derive a `--skill <dir>` argv for pi (via the `${SKILL_PATH}`/`${SKILLS_ROOT}`
  substitutions).

This duplicated work relay already does. Relay owns skill generation outright (see relay
`docs/decisions/004-project-mgmt-in-relay.md`: *"Skill generation already lives in relay … RelayLLM
only consumes the generated `SKILL.md`."*) and regenerates independently — on project create/update
(when `GenerateSkill` is on), after MCP reconcile, and via the relay UI's "Regenerate Now". The
spawn-time regen call was redundant.

`skillPath` was also more configuration than the problem needs: the skills directory is *always*
`<project-root>/.claude/skills` by convention. relayLLM already exposes the project root to templates
via the `${PROJECT_PATH}` substitution, so a configurable skill path earned nothing.

## Decision

1. **Remove `autoRegenSkills`** from `TerminalTemplate`, `PiConfig`, `RelayManagedSpec`, and the
   settings schema (`manifest.go`). relayLLM never triggers skill regeneration; relay owns it.
2. **Remove `skillPath`** (and the `${SKILL_PATH}`/`${SKILLS_ROOT}` substitutions, and
   `SpawnSubs.SkillPath`). Skills load from the convention `<project-root>/.claude/skills`:
   - pi `--mode rpc` auto-appends `--skill <project-root>/.claude/skills` (when the dir exists and the
     user didn't wire `--skill` via `extraArgs`).
   - PTY templates reference `${PROJECT_PATH}/.claude/skills` in their args — e.g. the `pidev`
     template's `"args": ["--skill", "${PROJECT_PATH}/.claude/skills"]`.
3. `RelayManagedSpec.Resolve()` now contacts relay's bridge purely to fetch the **project-scoped
   token** (and resolved project path), always sending `RegenSkills: "never"`.

## Bridge contract follow-up (landed)

The `ResolvePtyEnv` bridge call is the project-token broker; skill regen merely rode along on it.
Once relay owned every regen trigger, the `regen_skills` + `skill_path` request fields (and the
`skill_path` response field) carried no information — relayLLM is the sole caller and always sent
`regen_skills: never`. They were retired from the contract in a coordinated relayLLM + relay change
(`relay_bridge_client.go`/`relay_spawn.go` + `../relay/bridge/types.go`/`router.go`), and relay's
now-dead regen branch in `ResolvePtyEnv` was deleted. `ResolvePtyEnv` is now a pure token+workdir
resolver. The change was forward/backward compatible during rollout (JSON ignores unknown fields;
a missing `regen_skills` defaults to `never`).

Other notes:
- No sibling repo consumes the dropped `${SKILL_PATH}`/`${SKILLS_ROOT}` substitution tokens (swept
  relay/relayScheduler/eve/relayTelegram — only a relay *planning doc* mentions them).
- Existing `settings.json` keeps loading: the removed keys are unknown fields, silently ignored on
  unmarshal.

## Where relay regenerates now

With relayLLM out of the regen path, relay owns every trigger: **on relay startup** (for every
`GenerateSkill: true` project — `trayapp.go` → `regenProjectSkills`), on project create/update, after
MCP reconcile, and on an explicit per-project regen (`POST /api/projects/{id}/regen_skill`, surfaced
in eve's project kebab menu as "Regenerate Skills").

## Consequences

- **Good:** One less thing relayLLM does per spawn, and one fewer place skill-generation policy lives.
  relay is the single owner.
- **Good:** No bespoke `skillPath` knob to misconfigure; skills location is a fixed convention.
- **Migration (live config):** A `pidev`-style PTY template whose args used `${SKILLS_ROOT}` must be
  updated to `${PROJECT_PATH}/.claude/skills` — `${SKILLS_ROOT}` no longer resolves and would reach pi
  as a literal. The inert `autoRegenSkills`/`skillPath` keys can be deleted from `settings.json` at
  leisure (ignored if left).
- **Neutral:** If a project genuinely keeps skills somewhere other than `.claude/skills`, the user
  wires `--skill <dir>` explicitly via `extraArgs` (pi) or template args (PTY) — the auto-append backs
  off when `--skill` is already present.

## See also

- relay ADR-004 — project management + skill generation ownership in relay
- `relay_spawn.go::RelayManagedSpec.Resolve` — token-only bridge call
- `provider_pi.go` — pi `--skill` auto-append off the project root
