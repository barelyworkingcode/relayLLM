# ADR-012: TLS pinning for the relayLLM-to-upstream endpoint hop

**Status:** Accepted
**Date:** 2026-09-05

## Context

An `OpenAIEndpoint` (`openai_config.go`) is relayLLM's own client to another
OpenAI-compatible server — LM Studio, oMLX, a sibling relayLLM router, OpenAI
proper. On the deployment this ADR was written for, one such endpoint is a VM
whose relayLLM router forwards to a Mac host's relayLLM router over plain
`http`, on a LAN. Every other hop this deployment already secures — the
sibling `relayTTS`/`relaySTT` daemons pin *their* connection to this same
host router — leaving the endpoint hop the one unpinned link in the chain.

Two related but separate concerns follow from that: relayLLM as a *client*
dialing an upstream endpoint (Part A), and relayLLM as a *server* answering
on its own `--router-port` listener (Part B) — the thing a client like
relayTTS/relaySTT, or another relayLLM's endpoint config, actually dials.

## Decision

### Part A — pinned client transport per endpoint

`OpenAIEndpoint` gains `caFile` (a PEM bundle that becomes the *only* trust
anchor for that endpoint — system roots are not consulted once set) and
`pinSHA256` (SHA-256 hex fingerprints of the DER leaf certificate, colons and
case ignored). Both are validated at config load
(`normalizeOpenAI` → `endpoint_tls.go`'s `validateEndpointTransport`), not on
first use — a bad endpoint fails relayLLM startup, not the first chat
request routed to it. Rules: `http` is only reachable unconditionally when
the host is loopback; `caFile`/`pinSHA256` require `https` (they are silent
no-ops otherwise, which is worse than an error); a `caFile` must parse to at
least one certificate; every pin must normalize to a 64-hex-char digest.

A valid endpoint gets a transport built once and cached on it
(`OpenAIEndpoint.transport`, exposed via `Transport()`), cloned from
`http.DefaultTransport` so unrelated transport behavior (proxy env vars,
keep-alives) is untouched. Pin verification runs in `tls.Config.VerifyConnection`
— which, with `InsecureSkipVerify` false (the only state this codebase has),
runs *after* the standard chain verification, so pinning adds a check on top
of trust instead of replacing it. That ordering is what makes pinning able to
catch the case a bare CA anchor cannot: a MITM presenting a *different but
still CA-valid* certificate.

Fingerprint-of-leaf was chosen over SPKI pinning for two reasons: it needs
nothing beyond the stdlib (`sha256.Sum256(cert.Raw)`), and it matches the
output of `openssl x509 -fingerprint -sha256`, the tool an operator already
reaches for — no separate SPKI extraction step to get wrong.

Every call site that dials an `OpenAIEndpoint` now uses its transport:
`FetchOpenAIModels`, `NewOpenAIChatTransport`'s http.Client (`session.go`),
and `relay_router.go`'s endpoint-backed `newUpstreamProxy` call sites (the
audio-transcription route, the direct `endpoint.Name/id` route, and the
virtual-model retry path's endpoint branch — the last of these via
`VirtualTransport()`, a second cached transport built from
`virtualDialTransport`'s 3s dial timeout instead of the stdlib default, so a
black-holed virtual candidate still fails over promptly even when pinned).
Managed-server routes (llama-server/mlx-serve, always loopback processes)
and the Ollama provider (a different upstream kind entirely) are unchanged —
out of scope for this ADR.

### `allowPlaintextEndpoints` — a migration escape, not a downgrade

A non-loopback `http` endpoint fails config load by default. Existing
deployments (including the VM-to-host hop that motivated this work) need a
way to keep running unpinned while they migrate. Top-level settings.json
`"allowPlaintextEndpoints": true` lifts the *loopback* restriction — each
affected endpoint logs a `slog.Warn` naming itself as an unencrypted hop —
but it does nothing to `https` verification: it cannot set
`InsecureSkipVerify`, weaken `MinVersion`, or bypass a `caFile`/`pinSHA256`
check, because no code path connects the flag to any of those. It exists
purely so "I want http anyway" is a deliberate, logged, top-level decision
instead of an unreachable endpoint at boot.

### Part B — TLS on the router's own listener

`--router-tls-cert`/`--router-tls-key` (env `RELAY_LLM_ROUTER_TLS_CERT`/
`RELAY_LLM_ROUTER_TLS_KEY`) make `RelayRouter.ListenAndServe` call
`ListenAndServeTLS` with `tls.Config{MinVersion: tls.VersionTLS12}` instead of
plain `ListenAndServe`. `main` fails startup if only one of the pair is set —
a half-configured TLS listener that silently serves plaintext is exactly the
false sense of safety this whole ADR is trying to avoid elsewhere. Nothing
else about the router (dispatch, aggregation, affinity) changes; this is
purely which transport its one `*http.Server` is handed.

## Consequences

- An operator turning this on for the VM-to-host hop runs the host's router
  with `--router-tls-cert`/`--router-tls-key`, then on the VM's endpoint
  config sets `caFile` to the trust anchor for that cert — relay's own local
  CA (`~/Library/Application Support/Relay/ca.crt`) is the natural choice
  when the router's cert is relay-issued — and optionally `pinSHA256`,
  obtained with:

  ```
  openssl s_client -connect host:port </dev/null 2>/dev/null | openssl x509 -fingerprint -sha256 -noout
  ```

- `InsecureSkipVerify` does not exist anywhere in this code path and must
  never be added — `endpoint_tls.go` says so at the top of the file. A
  "skip verify" escape hatch would make every other rule in this ADR
  performative.
- The Ollama provider's own HTTP client is untouched; if it ever needs the
  same treatment, that's a separate ADR, not an extension of this one.
