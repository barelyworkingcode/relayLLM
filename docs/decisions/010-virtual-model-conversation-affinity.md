# ADR-010: Conversation affinity for virtual-model failover

**Status:** Accepted
**Date:** 2026-09-01

## Context

`virtual-llms` failover (no ADR of its own; shipped, then hardened immediately
before this) lets one stable model name attempt an ordered list of targets, preferring
whichever the 15s-stale reachability cache currently believes is usable. That
preference ordering is exactly what broke a real conversation in production.

A single Oh My Pi conversation had 97 turns served by a llama.cpp endpoint and
8 turns served by an oMLX endpoint, interleaved, because a spurious "endpoint
offline" reading caused a mid-conversation failover and then a failback.
Each backend encodes reasoning differently: llama.cpp emits `content` (an
array) plus `encrypted_content`; oMLX emits `summary` only. Oh My Pi stores
the assistant's reasoning items from every turn and replays the full history
on every subsequent request — standard behavior for a client built against
the Responses API's reasoning-item replay model. When routing swung back to
llama.cpp, llama.cpp hard-rejected the oMLX-shaped items already sitting in
the replayed history with `400 item['content'] is not an array`. Every retry
replayed the same poisoned history, so the conversation was permanently
wedged — not degraded, not slower, dead.

The incompatibility is one-directional, which is worth stating precisely
because it is easy to assume otherwise. Measured against both backends:

| reasoning item shape | llama.cpp | oMLX |
|---|---|---|
| `summary` only (what oMLX emits) | reject: `item['content'] is not an array` | accept |
| `content` array (what llama.cpp emits) | accept | accept |
| both `summary` and `content` | accept | accept |
| `content: []` | reject: `item['content'] is empty` | — |

So llama.cpp's transcript replays into oMLX without complaint; only the
reverse breaks. llama.cpp requires a non-empty `content` array on every
reasoning item, and oMLX never produces one — it emits `summary` with
`content: null` and no `encrypted_content`.

That still leaves nothing to translate. There is no way to reconstruct a
`content` array from an oMLX `summary`: the raw reasoning was never sent, and
`encrypted_content` is an opaque token valid only for the model that produced
it, so it cannot be synthesized either. A conversation that has taken even one
oMLX turn can never be replayed to llama.cpp. The only fix is to stop mixing:
**a conversation must stick to one target for its entire lifetime**,
independent of what the reachability cache prefers turn to turn.

## Decision

Pin a virtual model's chosen target per conversation, keyed on a
client-supplied conversation identifier (`prompt_cache_key`, falling back to
`user` — both standard OpenAI fields; Oh My Pi already sends a stable
per-conversation UUID as `prompt_cache_key` on every request). No key is
synthesized from headers or client IP: a wrong key would pin unrelated
conversations together, which is worse than the status quo of no pinning.

`virtualAffinityStore` (`virtual_affinity.go`) holds `map[(virtual,
conversation)]{target, lastUsed}` under a mutex. The key pairs the virtual
model name with the conversation key so two virtual models can reuse the same
client-generated key without colliding. The stored target is a string
identity — `"endpoint:<name>"` or `"alias:<name>"` — so an endpoint and a
managed alias that happen to share a bare name can never be confused with
each other, matching how every other namespace in the router already keeps
those two apart.

In `handleProxy`, once `candidatesForVirtual` has produced its normal
reachability-preferred ordering, a lookup by (virtual, conversation) moves
the pinned target to the front of that list — ahead of the preference
ordering, not merged into it. That is the entire point: a 15s reachability
wobble must not be allowed to hop an established conversation to a different
backend. If the pin names a target no longer present in the candidate list
(removed from config since it was recorded), it is silently ignored and the
normal order stands — never invent a target that isn't there. `routeVirtual`
records (or refreshes) the pin only after an attempt actually succeeds,
including onto a target other than the one that was pinned before, if that
one just failed pre-response and a later candidate served instead — the
conversation is already contaminated by the switch at that point, so the fix
is to pin forward, not to flap back on the next turn.

Bounded like `ProxyRegistry`'s reachability cache: a 1-hour TTL since last
use and a hard cap of 1024 entries evicting the least-recently-used survivor,
both swept lazily on the write path only — no background goroutine. A
long-lived router would otherwise accumulate one entry per distinct
`prompt_cache_key` forever.

### This puts state in a deliberately stateless router

Every other piece of `relay_router.go` — reachability, catalog rows, dispatch
— is derived fresh from config and the 15s probe cache on every request; the
router itself holds nothing about any individual conversation. This feature
breaks that on purpose. A router that forgets which backend answered a
conversation's last turn cannot avoid re-triggering this exact bug the next
time reachability flickers. The alternative — keeping the router fully
stateless — was rejected because statelessness was never the actual goal;
correctness was, and here they conflict. The state is deliberately narrow: an
in-memory, per-process map of opaque target identities, no persistence, no
cross-process sharing, bounded and self-pruning. It does not grow the
router's responsibility the way, say, tracking conversation content would.

### Alternative considered and rejected: sanitize inbound reasoning items

Instead of pinning, the router could inspect each request's reasoning items
and rewrite or drop the ones that don't match the selected target's expected
shape before forwarding. Rejected for two reasons. First, it requires the
router to know the reasoning-item shape of every backend it might ever proxy
to — format-specific knowledge (`content` vs. `summary` vs. whatever the next
integration invents) that has no other reason to live in a generic
OpenAI-compatible proxy. Second, and worse, it silently discards reasoning
the model already paid for: dropping or reshaping a reasoning item changes
what the model "remembers" about its own prior turn without telling the
caller, which is a much stranger failure mode than a request simply going to
the same backend it always has. Pinning keeps the router ignorant of
payload semantics and fixes the actual cause (backend-hopping) instead of
papering over its symptom.

## Consequences

- Affinity is scoped to virtual models only. Direct alias routing and
  `endpoint.Name/id` routing are single-target already — there is nothing to
  pin.
- `/v1/models` and `/models` remain fully stateless; affinity only touches
  the proxy path (`handleProxy` → `routeVirtual`).
- A conversation that never sends `prompt_cache_key` or `user` gets exactly
  today's behavior — reachability-preferred ordering, no pin, nothing stored.
- A relayLLM restart drops all pins. A conversation resumed after a restart
  re-establishes affinity on its next successful turn, same as a brand-new
  conversation. This is an accepted gap, not a design goal: persisting pins
  across restarts would need a store and a schema for a problem that only
  needs to survive the process's own uptime.
