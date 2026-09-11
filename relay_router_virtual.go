package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

type resolvedVirtualTarget struct {
	endpoint   OpenAIEndpoint
	upstreamID string
	manager    *ServerManager
	alias      string
}

// label renders the target for a human-readable failure message. Includes
// the upstream model id for an endpoint target — two targets on the same
// endpoint with different models (a big-then-small fallback pair) must be
// distinguishable in a 503's per-target failure list, or an operator reading
// it can't tell which one actually failed.
func (t resolvedVirtualTarget) label() string {
	if t.manager != nil {
		return fmt.Sprintf("alias %q", t.alias)
	}
	return fmt.Sprintf("endpoint %q model %q", t.endpoint.Name, t.upstreamID)
}

// identity is a stable, comparable value for this target — what
// virtualAffinityStore actually pins and compares against. Prefixed by kind
// so an endpoint named "x" and a managed alias named "x" (distinct
// namespaces everywhere else in the router) never collide here either.
//
// An endpoint target's identity also carries its upstream model id, not just
// the endpoint name. Two targets on the same endpoint but different models —
// e.g. a big-then-small fallback pair, [{endpoint:"lmstudio",model:"qwen-70b"},
// {endpoint:"lmstudio",model:"qwen-7b"}] — are different pins, not the same
// one: without the model id both candidates hash to "endpoint:lmstudio", so
// applyAffinity matches whichever of them happens to come first in
// candidates and can permanently re-pin a conversation that was actually
// served by qwen-7b onto qwen-70b next turn (code review item 1) — exactly
// the silent mid-conversation switch ADR-010 exists to prevent, and here it
// would never even self-correct. Each part is escaped so endpoint "a" model
// "b/c" and endpoint "a/b" model "c" can't collide on the "/" join.
func (t resolvedVirtualTarget) identity() string {
	if t.manager != nil {
		return "alias:" + t.alias
	}
	return "endpoint:" + escapeIdentityPart(t.endpoint.Name) + "/" + escapeIdentityPart(t.upstreamID)
}

// escapeIdentityPart escapes "\" and "/" in one component of a
// resolvedVirtualTarget identity, so joining endpoint-name and
// upstream-model-id with "/" can't produce the same string two different
// ways.
func escapeIdentityPart(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "/", `\/`)
	return s
}

// virtualCandidates returns every usable target for a configured virtual
// model, in the order handleProxy should attempt them. Returns nil when name
// isn't a configured virtual at all — callers distinguish "not a virtual" from
// "a virtual with no usable target" by checking p.virtual.Find themselves.
func (p *RelayRouter) virtualCandidates(ctx context.Context, name string) []resolvedVirtualTarget {
	if p.virtual == nil {
		return nil
	}
	virtual := p.virtual.Find(name)
	if virtual == nil {
		return nil
	}
	var statuses []EndpointStatus
	if p.registry != nil {
		statuses = p.registry.Snapshot(ctx)
	}
	candidates, _ := candidatesForVirtual(virtual, statuses, p.managers)
	return candidates
}

// candidatesForVirtual is virtualCandidates' pure ordering logic, factored
// out so handleModels can snapshot the registry once and reuse it across
// every configured virtual model instead of probing per model — Snapshot
// alone is O(endpoints); doing that once per virtual name made the old
// handleModels O(virtuals × endpoints) for a value it discarded once resolved.
//
// The registry's probe cache is 15s stale by design (see ProxyRegistry).
// Treating "online" as a hard gate — the old resolveVirtual's behavior — made
// a healthy endpoint unroutable for up to 15s after it recovered, and a dead
// one look routable for up to 15s after it dropped. Instead we walk the
// declared targets twice: pass one collects everything currently believed
// usable (in declared order); pass two appends the rest of the endpoint
// targets — configured, but currently believed offline — as last-resort
// attempts, still in declared order. A virtual name then works whenever *any*
// target actually works, not only when the cache happens to agree with
// reality. Skipped entirely, in both passes: an endpoint target naming an
// endpoint that isn't configured at all, a target missing one of its
// endpoint/model pair, and an alias target no manager has.
//
// freshCount reports how many of the returned candidates came from pass one
// — handleModels reports a virtual as "loaded" only when this is > 0, since
// the remainder are last-resort attempts the router isn't confident about.
func candidatesForVirtual(virtual *VirtualLLM, statuses []EndpointStatus, managers []*ServerManager) (candidates []resolvedVirtualTarget, freshCount int) {
	online := make(map[string]OpenAIEndpoint)
	configured := make(map[string]OpenAIEndpoint)
	for _, status := range statuses {
		configured[status.Endpoint.Name] = status.Endpoint
		if status.Online {
			online[status.Endpoint.Name] = status.Endpoint
		}
	}

	var fresh, stale []resolvedVirtualTarget
	for _, target := range virtual.Targets {
		switch classifyVirtualTarget(target) {
		case virtualTargetEndpoint:
			if endpoint, ok := online[target.Endpoint]; ok {
				fresh = append(fresh, resolvedVirtualTarget{endpoint: endpoint, upstreamID: target.Model})
			} else if endpoint, ok := configured[target.Endpoint]; ok {
				stale = append(stale, resolvedVirtualTarget{endpoint: endpoint, upstreamID: target.Model})
			}
			// else: names an endpoint that doesn't exist in config — skip.
		case virtualTargetAlias:
			for _, manager := range managers {
				if manager.HasAlias(target.Alias) {
					fresh = append(fresh, resolvedVirtualTarget{manager: manager, alias: target.Alias})
					break
				}
			}
			// else: no manager has this alias — skip.
		default: // virtualTargetInvalid: neither shape (e.g. endpoint set
			// without model, and no alias either) — skip. warnVirtualModelConfig
			// flags this at startup, using the same classifyVirtualTarget call,
			// so the two can no longer drift apart (code review item 5).
		}
	}
	return append(fresh, stale...), len(fresh)
}

// virtualTargetShape is what classifyVirtualTarget resolves a configured
// VirtualLLMTarget to.
type virtualTargetShape int

const (
	virtualTargetInvalid virtualTargetShape = iota
	virtualTargetEndpoint
	virtualTargetAlias
)

// classifyVirtualTarget is the single source of truth for what shape a
// configured target actually is — both candidatesForVirtual (routing) and
// warnVirtualModelConfig (startup validation, main.go) dispatch on this
// instead of hand-maintaining parallel switch statements. They used to do
// exactly that, and the case orders drifted apart (code review item 5): the
// validator checked "endpoint set, model not" before "alias set", so a
// target with both an endpoint (no model) *and* an alias — which
// candidatesForVirtual, checking alias second, routes fine via the alias —
// was flagged as the broken "endpoint without model" shape instead, and
// could even make the validator warn "no usable target" about a virtual that
// actually works.
//
// Precedence matches candidatesForVirtual exactly: endpoint+model wins when
// both are set, then alias. Anything else (endpoint without model and no
// alias, model without endpoint or alias, nothing set at all) is
// virtualTargetInvalid.
func classifyVirtualTarget(target VirtualLLMTarget) virtualTargetShape {
	switch {
	case target.Endpoint != "" && target.Model != "":
		return virtualTargetEndpoint
	case target.Alias != "":
		return virtualTargetAlias
	default:
		return virtualTargetInvalid
	}
}

// affinityKeyFromBody picks the conversation identifier that pins a virtual
// model's target — see ADR-010. Only these two standard OpenAI fields are
// read, in this precedence, because Oh My Pi already sends a stable
// per-conversation UUID as prompt_cache_key on every request. Deliberately
// not derived from anything else (headers, client IP): a wrong key pins
// unrelated conversations together, which is worse than no affinity at all.

func affinityKeyFromBody(promptCacheKey, user string) string {
	if promptCacheKey != "" {
		return promptCacheKey
	}
	return user
}

// applyAffinity moves the candidate matching pinned to the front of the
// list, ahead of the reachability-preferred ordering candidatesForVirtual
// already computed. That ordering optimizes for "believed usable right
// now"; a pin overrides it on purpose, because a 15s reachability-cache
// wobble must not be allowed to hop an established conversation to a
// different backend (ADR-010). pinned == "" is a no-op. A pin naming a
// target no longer present in candidates (e.g. removed from config) is
// silently ignored and the normal order stands — never invent a target that
// isn't there.
func applyAffinity(candidates []resolvedVirtualTarget, pinned string) []resolvedVirtualTarget {
	if pinned == "" {
		return candidates
	}
	idx := -1
	for i, c := range candidates {
		if c.identity() == pinned {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return candidates // not found, or already first: nothing to move.
	}
	reordered := make([]resolvedVirtualTarget, 0, len(candidates))
	reordered = append(reordered, candidates[idx])
	reordered = append(reordered, candidates[:idx]...)
	reordered = append(reordered, candidates[idx+1:]...)
	return reordered
}

// virtualCatalogRow builds the /v1/models row for one configured virtual
// model. A virtual name is stable config, so — unlike an endpoint model,
// which just disappears when its probe goes offline — the row always
// appears; status reflects whether the router currently believes a request
// for it will succeed.

// routeVirtual attempts each candidate in declared attempt order, moving to
// the next only when the previous attempt failed before any response byte
// reached the client — a dial/connection error, or a managed-server Acquire
// error. Once the upstream starts replying — including a partial SSE stream
// that then breaks — the exchange is committed: retrying would either
// duplicate a side-effecting request onto a second backend or splice two
// responses together, so we surface what the client already has instead of
// reaching for another target.
//
// affinityKey is "" when the request carried neither prompt_cache_key nor
// user — in that case p.affinity.record is a no-op (see its own nil/""
// guard), so this path costs nothing beyond the ordering already applied by
// the caller.
func (p *RelayRouter) routeVirtual(w http.ResponseWriter, r *http.Request, name string, candidates []resolvedVirtualTarget, body []byte, affinityKey string) {
	var failures []string
	for _, target := range candidates {
		// A caller that has already hung up (client disconnect →
		// context.Canceled, or a request deadline) must stop the failover
		// walk here rather than plow through every remaining candidate: each
		// remaining managed-alias candidate calls ServerManager.Acquire,
		// which takes no context and can cold-launch a model or block up to
		// admissionTimeoutSeconds (default 120s) — for a response nobody is
		// waiting on. Checked at the top of every iteration (not just once
		// before the loop) because the cancellation is typically what the
		// *previous* iteration's attemptVirtual just observed and reported
		// as its error, not something known in advance (code review item 2).
		if r.Context().Err() != nil {
			slog.Debug("relay router: caller context done, abandoning remaining virtual-model candidates",
				"model", name, "error", r.Context().Err())
			return
		}
		wrote, status, err := p.attemptVirtual(w, r, target, body)
		if err == nil {
			// Pin only a response the backend actually stands behind. A 5xx
			// is exactly the ADR-010 incident this guards against: llama.cpp
			// 500s on reasoning_effort:"minimal", and pinning that response
			// would lock every later turn onto the backend that just failed
			// instead of leaving the door open to fail over next time
			// (code review item 3). The response itself is NOT retried
			// either way — "upstream answered, whatever it answered stands"
			// — only whether it's worth remembering changes. A 4xx still
			// pins: the backend answered fine, the client sent something it
			// didn't like, and refusing to pin that would reintroduce the
			// backend-hopping ADR-010 exists to prevent.
			if status < http.StatusInternalServerError {
				// Record (or refresh) the pin on whichever target actually
				// served — including a target other than the one that was
				// pinned before, if that one just failed. The conversation is
				// already contaminated by the switch at that point, so pin
				// forward rather than flap back on the next turn (ADR-010).
				p.affinity.record(name, affinityKey, target.identity())
			}
			return // upstream answered — whatever it answered stands.
		}
		if wrote {
			return // already committed to the client; nothing left to retry.
		}
		failures = append(failures, fmt.Sprintf("%s: %v", target.label(), err))
	}
	writeRouterError(w, http.StatusServiceUnavailable,
		fmt.Sprintf("virtual model %q: no target reachable (%s)", name, strings.Join(failures, "; ")))
}

// attemptVirtual runs one virtual-model candidate against the real
// ResponseWriter through a recorder that tracks whether anything was written
// and, when it was, what status code the backend actually answered with —
// routeVirtual uses that to decide whether the response is worth pinning
// (see its 5xx handling, code review item 3). release is deferred (rather
// than called after ServeHTTP returns) because a mid-stream backend failure
// in a real net/http server panics with http.ErrAbortHandler — recovered by
// the standard library one frame up — and a bare post-call release() would
// leak the managed-server lease on that path.
func (p *RelayRouter) attemptVirtual(w http.ResponseWriter, r *http.Request, target resolvedVirtualTarget, body []byte) (wrote bool, status int, err error) {
	var backendErr error
	// onError intercepts the proxy's default 502 write: returning true tells
	// newUpstreamProxy the caller is handling the failure itself, so a
	// retryable attempt never leaks a partial error body to the client before
	// routeVirtual tries the next candidate.
	proxy, release, buildErr := p.buildVirtualAttempt(r.Context(), target, body, func(e error) bool {
		backendErr = e
		return true
	})
	if buildErr != nil {
		return false, 0, buildErr
	}
	defer release()

	rec := &virtualResponseRecorder{ResponseWriter: w}
	proxy.ServeHTTP(rec, r)
	if backendErr != nil {
		return rec.wrote, rec.statusCode, backendErr
	}
	return rec.wrote, rec.statusCode, nil
}

// buildVirtualAttempt constructs the reverse proxy for one virtual-model
// candidate, or reports why it couldn't (a managed-server Acquire failure, a
// bad body rewrite, or a bad endpoint URL) without writing anything —
// routeVirtual treats that identically to a pre-response backend failure and
// moves on to the next candidate. release is always non-nil (a no-op for
// endpoint targets, which have nothing to release).
//
// Requests are replayable across attempts because newUpstreamProxy's
// Director re-installs the body from the captured []byte on every call, so
// each candidate gets a fresh, undrained body.
func (p *RelayRouter) buildVirtualAttempt(ctx context.Context, target resolvedVirtualTarget, body []byte, onError func(error) bool) (proxy *httputil.ReverseProxy, release func(), err error) {
	if target.manager != nil {
		endpoint, rel, err := target.manager.Acquire(ctx, target.alias)
		if err != nil {
			return nil, nil, err
		}
		rewritten, err := rewriteProxyBody(body, "", p.reasoningEffortMap, p.reasoningEffortTemplateKwargs)
		if err != nil {
			rel()
			return nil, nil, fmt.Errorf("rewrite request body: %w", err)
		}
		targetURL, err := url.Parse(endpoint.BaseURL)
		if err != nil {
			rel()
			// Same nil-target panic risk routeManaged guards against (code
			// review item 7) — but here it's just one failed candidate, not
			// the whole request: the loop moves on to the next target.
			return nil, nil, fmt.Errorf("invalid managed server endpoint: %w", err)
		}
		proxy := newUpstreamProxy(targetURL, rewritten, endpoint.APIKey, target.manager.profile.Kind, target.alias, onError)
		proxy.Transport = virtualDialTransport
		return proxy, rel, nil
	}

	rewritten, err := rewriteProxyBody(body, target.upstreamID, p.reasoningEffortMap, p.reasoningEffortTemplateKwargs)
	if err != nil {
		return nil, nil, fmt.Errorf("rewrite request body: %w", err)
	}
	targetURL, err := url.Parse(target.endpoint.BaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid endpoint configuration: %w", err)
	}
	proxy = newUpstreamProxy(targetURL, rewritten, target.endpoint.APIKey, "openai", target.endpoint.Name, onError)
	proxy.Transport = target.endpoint.VirtualTransport()
	return proxy, func() {}, nil
}

// virtualResponseRecorder wraps the client's real ResponseWriter for one
// virtual-model attempt. routeVirtual reads `wrote` to decide whether the
// attempt is safe to retry: once a header or body byte has actually reached
// the client, the exchange is committed. It also captures the status code
// the backend answered with, so routeVirtual can decide whether the response
// is worth pinning (a 5xx is not — see code review item 3).
type virtualResponseRecorder struct {
	http.ResponseWriter
	wrote      bool
	statusCode int
}

func (v *virtualResponseRecorder) WriteHeader(statusCode int) {
	v.wrote = true
	v.statusCode = statusCode
	v.ResponseWriter.WriteHeader(statusCode)
}

func (v *virtualResponseRecorder) Write(b []byte) (int, error) {
	v.wrote = true
	if v.statusCode == 0 {
		// Write without a prior WriteHeader implies 200, same as the
		// standard library's own http.ResponseWriter — the reverse proxy
		// always calls WriteHeader itself before copying the body, so this
		// only matters for a handler that skips straight to Write (none of
		// ours do, but the zero value must not read as "unknown status").
		v.statusCode = http.StatusOK
	}
	return v.ResponseWriter.Write(b)
}

// Flush is required for SSE streaming: newUpstreamProxy sets
// FlushInterval: -1, which flushes through whatever ResponseWriter it was
// handed.
func (v *virtualResponseRecorder) Flush() {
	if f, ok := v.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// virtualDialTransport is used only by the virtual-model retry path. A
// target host that black-holes packets (rather than actively refusing the
// connection — the real case this fixes, an unreachable LAN host) would
// otherwise eat http.DefaultTransport's ~30s dial timeout per candidate
// before failover even started trying the next one. ResponseHeaderTimeout is
// deliberately left unset: generation can legitimately take a long time, and
// a slow-but-alive backend must not be mistaken for a dead one. Direct
// (non-virtual) routes keep plain http.DefaultTransport behavior.
var virtualDialTransport http.RoundTripper = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 3 * time.Second}).DialContext
	return t
}()
