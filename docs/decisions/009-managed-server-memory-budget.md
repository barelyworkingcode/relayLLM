# ADR-009: Managed-server memory budget with leases and idle reclaim

**Status:** Accepted
**Date:** 2026-08-17

## Context

`ServerManager` launched managed servers (llama-server, mlx-serve) on demand and
then kept them running forever. Nothing evicted them. The only way to reclaim
memory was `DELETE /api/{kind}/instances/{alias}` by hand.

On a 128 GB machine with the four models currently configured, all four resident
at once is ~99 GB of weights plus KV cache — before compute buffers. Two large
models plus a long-context session is enough to start swapping. The failure mode
is bad: the machine degrades globally rather than relayLLM reporting a problem.

llama.cpp shipped its own router mode (Dec 2025) covering on-demand launch, LRU
eviction, and `--models-max`. We considered adopting it and did not, for two
reasons. It only knows GGUF, so mlx-serve would still need `ServerManager`; and
it has no idle TTL — eviction fires only when a *new* model needs a slot, so
"reclaim memory when nothing is running" would remain unimplemented. Building
the budget here covers both backends and both triggers. Revisit if llama.cpp's
router grows an idle TTL and we drop mlx-serve.

## Decision

### Two caps, both optional

`maxLoaded` (instance count) and `maxMemoryGB` (estimated bytes) each trigger
eviction. Both default to unlimited, so the feature is inert until configured.

The count cap alone is a poor proxy: our four models span 7 GB to 41 GB, so
"2 loaded" means anywhere from 14 GB to 68 GB. The memory cap is the meaningful
one; the count cap is kept because it is exact and cheap to reason about.

### Sizes are computed, not declared

Per-model `memoryGB` exists as an override, but the default path reads the model
metadata:

```
weights = file size on disk        (exact: llama.cpp mmaps it, ngl=-1 keeps it resident)
kv      = Σ over layers: tokens(layer) × kv_heads[layer] × (k_len × bpe_k + v_len × bpe_v)
```

Requiring hand-entered sizes was rejected because nobody gets this right by
hand. Two effects make the naive guess badly wrong:

- **Sliding-window attention.** Gemma 4 12B flags 40 of its 48 layers as SWA
  (`sliding_window = 1024`), so those layers hold a 1024-token window rather
  than the full context, at a smaller head dimension. Ignoring this over-
  estimates that model at 32k context by ~25x (10.89 GB vs 0.43 GB actual).
- **Per-layer GQA.** `attention.head_count_kv` can be an array; Gemma alternates
  8 and 1 KV heads by layer.

Both are in the GGUF header, which is cheap to read (metadata only, no tensor
data). MLX models use the same shape via `config.json`.

The compute/graph buffer is deliberately *not* modelled — it depends on
ubatch-size and the graph's widest node. A flat `memoryHeadroomPercent`
(default 10) pads the estimate instead of pretending to compute it.

A model whose size cannot be determined estimates to 0, which means "cannot be
size-budgeted" — it still counts against `maxLoaded` but never against
`maxMemoryGB`. Guessing a number would silently mis-admit.

### Leases, not timestamps

Eviction must never kill an in-flight generation, so instances are refcounted
rather than judged by a last-request timestamp. `Acquire` returns an idempotent
release; an instance with `leases > 0` is invisible to both the LRU victim
search and the idle reaper.

The lease is held for a **whole turn**, not a single HTTP request, because a
tool-calling turn is many requests. `runToolLoop` owns exactly that scope and
carries the `defer release()`.

This forced a change on the session path. `session.go` previously resolved an
endpoint once at provider start and the transport then talked to that port for
the session's lifetime — the manager never saw subsequent traffic, so it could
not tell a busy model from an abandoned one. Transports backed by a managed
server now resolve per turn via `BackendResolver`, which both takes the lease
and picks up a new port if the model was evicted and relaunched. As a side
benefit this fixes a pre-existing bug: a session whose llama-server died was
previously stuck talking to a dead port forever.

Resolving per turn also means session creation no longer launches a model. The
transport's `Ping` is a no-op when a resolver is set — pinging by launching
would reintroduce the eager behavior the budget exists to prevent. Alias
validity is still checked at session creation via `HasAlias`, so a typo fails
early.

### Wait for idle rather than reject or kill

When the budget is full and every loaded instance is leased, `Acquire` waits for
one to go idle, bounded by `admissionTimeoutSeconds` (default 120) after which
it errors naming the busy aliases. Rejecting immediately would fail a second
concurrent session that would have succeeded a few seconds later; evicting
anyway would kill someone's in-flight generation.

The wait is a close-and-replace channel broadcast rather than a `sync.Cond`,
because `sync.Cond` does not compose with a timeout.

A model larger than the entire budget fails immediately with a message naming
the shortfall, rather than blocking for the full admission timeout on an
eviction that could never free enough room.

### Budget check and launch share one critical section

`fitsLocked` and `launchLocked` run under a single acquisition of `m.mu`, so two
concurrent `Acquire` calls for different aliases cannot both observe room and
both spend it. `cmd.Start` is a fork/exec and fast enough to hold the lock for;
the slow health poll runs outside it, as before.

## Consequences

- Budgets are inert until configured. Existing deployments behave exactly as
  before until someone sets a cap.
- `GetOrLaunch` remains as an acquire-then-immediately-release wrapper. It holds
  no lease, so its result can be evicted before the caller uses it — new code
  should use `Acquire`.
- Hermetic tests inject instances directly into `m.instances` and pin a
  nonexistent binary path, so admission, eviction, and reaping are covered
  without spawning a process. The estimator is verified against real GGUFs in
  the `llm` tier.
- A session idle past the timeout loses its backing process and pays a reload on
  the next message. That is the intended trade.
