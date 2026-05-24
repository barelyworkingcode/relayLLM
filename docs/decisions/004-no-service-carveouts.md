# ADR-004: No Service Carveouts in relay (IoC for Ecosystem Integrations)

**Status:** Accepted
**Date:** 2026-05-23

## Context

When relayLLM first integrated with relay, the temptation was to add per-service knowledge in relay: "if request path starts with `/api/sessions/`, proxy to relayLLM"; "settings UI has a special panel for relayLLM"; "scheduled-task routes are forwarded through relayLLM". This works for one or two services but doesn't scale, and it breaks the inversion-of-control principle: relay is supposed to be a container for arbitrary services, not a coordinator that knows each one.

## Decision

**No service IDs are hardcoded in relay.** Period. relay treats every connected service identically — they're all "services with manifests" (see ADR-001). Specifically:

- No `if serviceID == "relayllm"` branches.
- No "first-party" carveouts that get privileged treatment.
- Settings UI renders panels from the manifest (status path, declared actions), not from a service-specific component.
- Routing decisions come from the manifest's route list, not from a hardcoded table.

When the temptation arises to add a carveout, the answer is to extend the manifest protocol to make the capability generic. If a one-off carveout is the only viable path, that's the signal that the manifest needs a new field, not a special case.

## Consequences

- **Good:** New services (relayTelegram, hypothetical relayMail, etc.) can ship independently without touching relay. The ecosystem scales horizontally.
- **Good:** Forces every cross-service capability to be made into a protocol, which keeps the protocol small and well-considered.
- **Bad:** The first version of a feature often *is* a carveout. Resisting it means more upfront design — sometimes longer time-to-first-version.
- **Bad:** Cross-cutting concerns that genuinely need per-service config (e.g. observability sampling rates) have to land as manifest fields instead of relay-side config.

## Anti-examples we've avoided

- A "tier-0 service" carveout for the relayLLM proxy. (Was proposed, rejected, replaced with the generic dispatcher.)
- A hardcoded relayLLM panel in the settings UI. (Replaced with a generic Service Inspector that consumes the manifest.)
- Service-specific scheduler routing through relayLLM. (Replaced with relayScheduler registering its own manifest.)

## When this rule yields

Never silently. If a carveout seems necessary, write a new ADR explaining why the manifest protocol can't represent it, and what the protocol *would* need to. Often the design discussion alone surfaces the right manifest extension.

## See also

- ADR-001 — the manifest protocol this rule enables
- `../relay/plans/service-manifest-spec.md` — full design notes including original IoC discussion
