# ADR-005: `ActionDecl.ForEach` — Per-Row Manifest Actions

**Status:** Accepted
**Date:** 2026-05-23

## Context

The V1 service-manifest spec (ADR-001) declared `actions` as a flat list — one declaration = one global button — and flagged per-row actions as a deferred omission, roughly:

> Per-row actions are a deliberate V1 omission. When we want a "Stop this llama instance" button per row in a status table, add a single optional field — e.g. `forEach: "instances"` — that renders one button per entry in the named status array, with `{key}` placeholders substituted from the row. Additive, non-breaking, deferred until needed.

(That deferral is what this ADR resolves; `forEach` is now part of the shipped protocol — see `../relay/docs/service-manifest.md`.)

The relay menubar status panel (the feature request that drove the whole manifest refactor) needed exactly this for relayLLM's `llama-server` instances: without `forEach` the only declarable action would have been a coarse "Stop All" — useless when the user wants to stop one specific runaway instance. The alternative — leave actions empty for V1 — would have left the Service Inspector read-only and the manifest's `Actions` field unused.

## Decision

Add one optional field to `ActionDecl` (`manifest.go`): `ForEach string` (`json:"forEach,omitempty"`), naming a top-level array key in the status payload.

- `ForEach == ""` (default) → one global button, no substitution.
- `ForEach == "<arrayName>"` → one button per element of the status payload's top-level `<arrayName>` array. The UI substitutes `{key}` placeholders in `PathTemplate` from the row's keys; relay's dispatcher URL-escapes each value and refuses anything not declared in the manifest.

The status payload becomes load-bearing: it must *contain* the referenced array. `/api/status` embeds three `[]ServerInstanceInfo`/`[]TerminalSummary` arrays — `instances`, `mlxInstances`, `terminals` — backing three declared forEach actions: `stop-llama` (`forEach: "instances"`), `stop-mlx` (`mlxInstances`), and `stop-terminal` (`terminals`).

## Consequences

- **Good:** "Act on this row" lands without a new RPC, auth surface, or UI primitive. `../relay/ipc_service_action.go::buildActionPath` stays the single chokepoint: row *values* come from the UI, but the *paths* are always service-declared — the manifest remains the action whitelist, defense-in-depth against UI compromise. Existing flat-action manifests keep working (`omitempty`), and UIs ignorant of `forEach` degrade to global buttons rather than breaking. New services adopting per-row actions write only manifest data.
- **Bad:** The status payload now has wire significance beyond display — array names and row keys feed dispatch. Renaming `instances`/`mlxInstances`/`terminals` or any `ServerInstanceInfo`/`TerminalSummary` field is a coordinated change across the manifest declaration AND the relay UI. Locked by `api_status_test.go` (relayLLM) and `bridge/manifest_test.go` (relay).
- **Bad:** Escaping is per-value; the path *structure* is still the service's responsibility. A malformed `PathTemplate` (e.g. missing placeholder) surfaces as a dispatch failure, not a pre-flight error.

## What we didn't add

- **Confirmation prompts on destructive actions.** Deferred (per the spec). Stopping a llama/mlx instance is reversible (relaunch on demand); destructive-prompt UX waits until a non-reversible action ships.
- **Row payload validation against a schema.** Row values pass through opaquely; a JSONSchema-shaped declaration is heavier than warranted.
- **Multiple `ForEach` arrays per action.** One action ↔ one array. Cross-array actions have no use case yet.

## See also

- ADR-001 — base manifest protocol
- ADR-004 — no service carveouts
- `manifest.go::buildManifest` — declaration site for the three forEach actions
- `../relay/ipc_service_action.go::buildActionPath` — substitution + escaping
- `../relay/web/src/app.js::renderArrayBlock` — UI rendering of per-row buttons