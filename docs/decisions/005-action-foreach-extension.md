# ADR-005: `ActionDecl.ForEach` — Per-Row Manifest Actions

**Status:** Accepted
**Date:** 2026-05-23

## Context

The V1 service-manifest spec (ADR-001, plus `../relay/plans/service-manifest-spec.md`) declared `actions` as a flat list of buttons — one declaration = one global button. The spec explicitly flagged per-row actions as a deferred V1 omission:

> Per-row actions are a deliberate V1 omission. When we want a "Stop this llama instance" button per row in a status table, we add a single optional field — e.g. `forEach: "instances"` — that tells the UI to render one button per entry in the named status array, with `{key}` placeholders substituted from the row. Additive, non-breaking, deferred until needed.

The relay menubar status panel (the original feature request that drove the entire manifest refactor) reached the point of needing exactly this. Without `forEach`, the only declarable action against relayLLM's `llama-server` instances would have been a coarse "Stop All" — useless when the user actually wants to stop one specific runaway instance.

The alternative was to leave actions empty for V1, which would have left the Service Inspector read-only and the manifest's `Actions` field unused — defeating half the point of the panel.

## Decision

Extend `ActionDecl` with one optional field:

```go
type ActionDecl struct {
    ID           string `json:"id"`
    Label        string `json:"label"`
    Method       string `json:"method"`
    PathTemplate string `json:"pathTemplate"`
    ForEach      string `json:"forEach,omitempty"` // status-payload array name
}
```

Semantics:

- `ForEach == ""` (default) → one global button, no placeholder substitution.
- `ForEach == "<arrayName>"` → one button per element in the status payload's top-level `<arrayName>` array. The UI substitutes `{key}` placeholders in `PathTemplate` from the row's keys; relay's dispatcher URL-escapes each substituted value and refuses anything not declared in the manifest.

The status payload becomes load-bearing for the substitution: it has to *contain* the array whose name `ForEach` references. relayLLM's `/api/status` was reshaped to embed `instances` (the existing `LlamaInstanceInfo` slice) so the manifest's `stop-llama` action could declare `forEach: "instances"` cleanly.

## Why additive

- Existing manifests (zero or more flat actions) keep working — `omitempty` means old wire payloads decode unchanged.
- Existing relay UIs that don't know about `forEach` would render every action as a global button (degraded but not broken).
- New services adopting per-row actions write only manifest data — no relay-side code.

## Consequences

- **Good:** First-class support for "act on this row" lands without a new RPC, a new auth surface, or a new UI primitive. The dispatch path (`ipc_service_action.go`) remains the single chokepoint for action validation.
- **Good:** The manifest stays the action whitelist. Row values come from the UI, but the *paths* are always service-declared — defense in depth against UI compromise.
- **Bad:** The status payload now has wire significance beyond display: the array name and row keys feed dispatch. Renaming `instances` or any of `LlamaInstanceInfo`'s fields is a coordinated change across the manifest declaration AND the relay UI. Locked in by `api_status_test.go` (relayLLM) and `bridge/manifest_test.go` (relay).
- **Bad:** URL-escaping happens per substituted value, but the path *structure* is still the service's responsibility. If a service ships a `PathTemplate` that's wrong on its own merits (e.g. missing a placeholder), the UI surfaces the dispatch failure but can't catch it ahead of time.

## What we didn't add

- **Confirmation prompts on destructive actions.** The spec called this out as a deferred V1 item; still deferred. `DELETE /api/llama/instances/{alias}` is reversible (just relaunch), so we ship without confirmations.
- **Row payload validation against a schema.** The UI passes row values through opaquely. Adding a JSONSchema-shaped declaration would be heavier than what's currently warranted.
- **Multiple `ForEach` arrays per action.** One action ↔ one array. Cross-array actions (e.g. "do something with elements from both tables") have no use case yet.

## See also

- ADR-001 — base manifest protocol
- ADR-004 — no service carveouts
- `manifest.go::buildManifest` — relayLLM's declaration site for `stop-llama`
- `../relay/ipc_service_action.go::buildActionPath` — substitution + escaping
- `../relay/settings.html::renderArrayBlock` — UI rendering of per-row buttons
