# ADR-001: Service Manifest Protocol

**Status:** Accepted
**Date:** 2026-05-23

## Context

relay (the macOS tray app) needs to dispatch incoming Eve traffic to the right backend service (relayLLM, relayScheduler, etc.). Originally relay hardcoded knowledge of each service (e.g. "if path starts with `/api/sessions`, proxy to relayLLM"). This was a carveout per service and got worse as the ecosystem grew.

## Decision

Every relay-enhanced service declares a **manifest** to relay over the bridge socket at startup. The manifest carries:

- The list of HTTP/WS routes the service serves
- The service's internal Unix socket + bearer token (for relay to dial)
- Optional status endpoint and user actions for the Settings UI

relay's front-door dispatcher does longest-prefix routing against the registered manifests. No service IDs are hardcoded anywhere in relay.

The protocol design and registration mechanics live in `../relay/plans/service-manifest-spec.md`. On the relayLLM side, the implementation is `manifest.go` (manifest construction + `maybeRegisterManifest`) and `relay_bridge_client.go` (transport).

Mode detection is environment-driven, not flag-driven: relayLLM checks `RELAY_BRIDGE_SOCKET`. Standalone runs (env unset) skip registration cleanly.

## Consequences

- **Good:** Adding a new service (relayTelegram, etc.) requires zero changes to relay. The service ships its own routes + manifest and is wired up the moment it starts.
- **Good:** Routes are documented in code where they're served, not duplicated in a routing table elsewhere.
- **Good:** The same listener serves both standalone and enhanced modes — no code fork.
- **Bad:** Bridge wire types are duplicated across `../relay/bridge/manifest.go` and `relayLLM/manifest.go` (separate Go modules). Drift risk; see ROADMAP item X1 for the eventual shared-module fix.

## See also

- `manifest.go` — manifest declaration + registration call site
- `relay_bridge_client.go` — bridge wire protocol
- `manifest_test.go` — handshake regression coverage (FakeBridge)
- `../relay/plans/service-manifest-spec.md` — full protocol contract
