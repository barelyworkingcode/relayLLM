package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// RelayRouter aggregates managed-server aliases (llama.cpp, MLX, …) and
// reachable OpenAI endpoint models behind one OpenAI-compatible listener.
// Dispatch is by the request body's `model` field: bare alias → first
// matching manager; virtual name → ordered candidate targets, attempted in
// turn until one works (see routeVirtual); `endpoint.Name/id` → matching
// OpenAI upstream.
// Endpoints that fail their last probe drop out of /v1/models and refuse
// direct (`endpoint.Name/id`) routing until the next 15s TTL cycle — a
// virtual model may still route through an offline-believed endpoint as a
// last resort (see candidatesForVirtual).
type RelayRouter struct {
	managers []*ServerManager
	registry *ProxyRegistry
	virtual  *VirtualLLMConfig
	affinity *virtualAffinityStore
	server   *http.Server

	// reasoningEffortMap rewrites a top-level "reasoning_effort" string field
	// on every proxied body before it reaches a backend — see RouterConfig
	// and rewriteProxyBody. nil/empty (the zero value, and what every
	// constructor leaves it at) means no rewriting at all; wired in from
	// settings.json via StartRelayRouter's trailing *RouterConfig parameter
	// (see setReasoningEffortMap), applied before the serving goroutine is
	// spawned rather than a NewRelayRouter constructor parameter, to keep
	// this opt-in feature from touching NewRelayRouter's much larger set of
	// call sites. reasoningEffortTemplateKwargs, below, is its sibling knob.
	reasoningEffortMap map[string]string

	// reasoningEffortTemplateKwargs merges an object into the proxied body's
	// top-level "chat_template_kwargs" field — see RouterConfig for the
	// oMLX/llama.cpp measurements this exists to satisfy, and
	// rewriteProxyBody / applyReasoningEffortTemplateKwargs for the merge
	// semantics. Same nil/empty-means-off shape, wired in the same way
	// (setReasoningEffortTemplateKwargs, called before serving starts), as
	// reasoningEffortMap above.
	reasoningEffortTemplateKwargs map[string]map[string]any
}

// RouterConfig holds relay-router behavior that doesn't belong to any one
// backend — the reasoning-effort rewrite table and its sibling
// chat_template_kwargs merge table. Maps to settings.json's optional
// top-level "router" section.
type RouterConfig struct {
	// ReasoningEffortMap rewrites (or, mapped to "", removes) a top-level
	// string "reasoning_effort" field on every proxied request body. Absent
	// or empty disables rewriting entirely — the default, so behavior is
	// byte-identical to a settings.json with no "router" section at all.
	//
	// Keys and values are free-form strings on purpose, not a fixed
	// vocabulary: backends disagree about what they accept. Measured against
	// a real llama.cpp server (Qwen3.8-27B): "none" is accepted and turns
	// reasoning off; "low"/"medium"/"high"/"xhigh" are accepted and produce
	// reasoning; "minimal" 500s ("Unexpected reasoning effort minimal.
	// Supported types are xhigh (default), medium, and low."). Oh My Pi has
	// no wire value that means "off" — its `--thinking off` clamps to the
	// lowest entry in the model's configured `efforts` list and sends that
	// verbatim, so "minimal" is the lowest value such a client can be made
	// to send. Mapping {"minimal": "none"} turns that into the off signal
	// the backend actually understands. See CLAUDE.md's Relay-router section
	// for the full story.
	ReasoningEffortMap map[string]string `json:"reasoningEffortMap,omitempty"`

	// ReasoningEffortTemplateKwargs merges an object into a proxied body's
	// top-level "chat_template_kwargs" field when the request's ORIGINAL
	// "reasoning_effort" string value — matched the same way, and BEFORE,
	// ReasoningEffortMap rewrites it (see rewriteProxyBody) — matches a
	// configured key. Absent or empty disables this entirely, the default,
	// so behavior is unchanged from before this field existed.
	//
	// ReasoningEffortMap's value swap only fixes backends that interpret
	// reasoning_effort server-side (llama.cpp does). It does not fix oMLX:
	// measured at omlx/server.py:3594, oMLX merges request.reasoning_effort
	// verbatim into chat_template_kwargs and hands it to the model's Jinja
	// template — there is no server-side meaning to rewrite. The MLX build
	// of the measured model (CodeFast) uses the older Qwen convention
	// (enable_thinking), not reasoning_effort, so no VALUE swap of
	// reasoning_effort can reach it — the template never reads that field at
	// all. It needs a field-SHAPE rewrite: inject a different field.
	// Measured reasoning output length against oMLX CodeFast:
	//
	//	request                                            reasoning returned
	//	baseline                                            101 chars
	//	reasoning_effort: "none"                             94 chars — no effect
	//	chat_template_kwargs: {"enable_thinking": false}      0 chars — off
	//
	// The same chat_template_kwargs also turns reasoning off against
	// llama.cpp (0 chars, measured on "europa"), so each backend tolerates
	// the other's mechanism harmlessly — llama.cpp ignores an
	// unrecognized chat_template_kwargs key, oMLX ignores reasoning_effort
	// once nothing reads it. Configuring both knobs together (this field and
	// ReasoningEffortMap) is what makes "turn reasoning off" portable across
	// both backends from one client-side value.
	//
	// Matched against the value BEFORE ReasoningEffortMap's rewrite, not
	// after, because the two knobs describe ONE inbound client value
	// triggering TWO independent rewrites: configuring
	// {"minimal": "none"} (ReasoningEffortMap) alongside
	// {"minimal": {"enable_thinking": false}} (this field) must both fire
	// off the client's original "minimal". Matching post-rewrite would
	// require this field's keys to track whatever ReasoningEffortMap
	// happens to rewrite "minimal" INTO ("none") rather than what the client
	// actually sent — coupling the two maps together for no reason, and
	// breaking silently if either is reconfigured independently.
	//
	// Values are arbitrary JSON (bool, string, number, …), not just bools:
	// oMLX forwards chat_template_kwargs to the Jinja template verbatim, so
	// this passes values through untyped exactly like ReasoningEffortMap's
	// free-form string values do.
	//
	// The merge never overwrites a key the client's own body already sets
	// under chat_template_kwargs — mirroring oMLX's own
	// merged.setdefault(...) server-side, so the client's explicit choice
	// always wins over ours. See applyReasoningEffortTemplateKwargs.
	ReasoningEffortTemplateKwargs map[string]map[string]any `json:"reasoningEffortTemplateKwargs,omitempty"`
}

// setReasoningEffortMap installs the router-level reasoning_effort rewrite
// table (settings.json's router.reasoningEffortMap). nil or empty disables
// rewriting, which is also this field's zero value, so a router this is
// never called on behaves exactly as it did before the feature existed.
//
// MUST be called before the router starts serving — StartRelayRouter is the
// only production call site, and it calls this before spawning the
// ListenAndServe goroutine. Go's memory model guarantees a goroutine's
// creation happens-before its execution, so every request-handling goroutine
// transitively spawned from that one is guaranteed to observe the write; a
// call made after the goroutine is already running (the previous shape:
// main called the exported SetReasoningEffortMap after StartRelayRouter had
// already returned) races the first accepted connection under -race. Kept
// unexported, rather than removed, so tests that drive a router's handler
// directly without ever calling ListenAndServe (no goroutine, so no race)
// can still configure it post-construction.
func (p *RelayRouter) setReasoningEffortMap(m map[string]string) {
	p.reasoningEffortMap = m
}

// setReasoningEffortTemplateKwargs installs the router-level
// chat_template_kwargs merge table (settings.json's
// router.reasoningEffortTemplateKwargs). nil or empty disables it — also
// this field's zero value — so a router this is never called on behaves
// exactly as it did before the feature existed. Subject to the same
// pre-serve constraint as setReasoningEffortMap above (see its comment):
// StartRelayRouter calls this before spawning the ListenAndServe goroutine.
func (p *RelayRouter) setReasoningEffortTemplateKwargs(m map[string]map[string]any) {
	p.reasoningEffortTemplateKwargs = m
}

// NewRelayRouter creates a router on addr. Nil entries in managers are
// dropped; registry may be nil to disable the endpoint branch; virtual may be
// nil to disable the virtual-model branch. A router with no live backends
// 400s every request — StartRelayRouter guards against starting one.
func NewRelayRouter(addr string, managers []*ServerManager, registry *ProxyRegistry, virtual *VirtualLLMConfig) *RelayRouter {
	live := make([]*ServerManager, 0, len(managers))
	for _, m := range managers {
		if m != nil {
			live = append(live, m)
		}
	}
	p := &RelayRouter{managers: live, registry: registry, virtual: virtual, affinity: newVirtualAffinityStore(nil)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", p.handleModels)
	mux.HandleFunc("GET /models", p.handleModels)
	mux.HandleFunc("POST /models/load", p.handleModelLoad)
	mux.HandleFunc("POST /models/unload", p.handleModelUnload)
	mux.HandleFunc("GET /health", p.handleHealth)
	mux.HandleFunc("POST /v1/audio/transcriptions", p.handleAudioTranscription)
	mux.HandleFunc("/", p.handleProxy)

	p.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return p
}

func (p *RelayRouter) ListenAndServe() error {
	slog.Info("relay router listening", "addr", p.server.Addr)
	return p.server.ListenAndServe()
}

func (p *RelayRouter) Close() error {
	return p.server.Close()
}

// handleModels serves the catalog for both /v1/models and /models.
//
// Rows carry llama.cpp router mode's extra fields (status, meta, architecture)
// alongside the OpenAI ones. That is deliberate: clients written against
// llama.cpp's router — pi ships a built-in extension that does exactly this —
// validate every row has a string `status.value` and reject the whole catalog
// without it. The fields are additive, so plain OpenAI clients ignore them.
func (p *RelayRouter) handleModels(w http.ResponseWriter, r *http.Request) {
	var data []map[string]any
	// Dispatch resolves a model id to exactly one behavior — managed alias,
	// then virtual name, then endpoint model, in that priority order (see
	// handleProxy) — so every row type shares this dedup set, built in that
	// same order, and the row that survives here is the one that will
	// actually serve a request for that id. A collision with a managed alias
	// is dead config no matter which section declares it: managers are
	// checked first in handleProxy regardless of catalog order. A collision
	// between a virtual name and an endpoint's prefixed id is NOT symmetric
	// the same way — handleProxy checks p.virtual.Find before
	// p.registry.LookupModel, so it's always the endpoint side that's
	// unreachable, never the virtual side. Building rows in dispatch order
	// (managed, virtual, endpoint — it used to be managed, endpoint, virtual)
	// is what keeps this listing honest about which one that is.
	seen := make(map[string]bool)
	for _, m := range p.managers {
		for _, entry := range m.ModelCatalog() {
			if seen[entry.Alias] {
				continue
			}
			seen[entry.Alias] = true

			status := map[string]any{"value": entry.Status}
			if entry.Failed {
				// Clients poll until "loaded"; without a failure flag a model
				// that can never start would be polled forever.
				status["failed"] = true
				if entry.Error != "" {
					status["error"] = entry.Error
				}
			}

			modalities := []string{"text"}
			if entry.SupportsImages {
				modalities = append(modalities, "image")
			}

			row := map[string]any{
				"id":           entry.Alias,
				"object":       "model",
				"created":      0,
				"owned_by":     m.profile.Group,
				"status":       status,
				"architecture": map[string]any{"input_modalities": modalities},
			}
			// n_ctx is what the server will actually run with; n_ctx_train is
			// the model's native limit. Clients read n_ctx first and fall back
			// to n_ctx_train, so a model with no pinned ctx-size still reports
			// a real number instead of the client's generic default.
			meta := map[string]any{}
			if entry.ContextSize > 0 {
				meta["n_ctx"] = entry.ContextSize
			}
			if entry.TrainedContext > 0 {
				meta["n_ctx_train"] = entry.TrainedContext
			}
			if len(meta) > 0 {
				row["meta"] = meta
			}
			// context_length mirrors the same number at the top level, for
			// clients that discover models the plain-OpenAI way (LM Studio-,
			// vLLM-, OpenRouter-style) and never look inside meta at all —
			// Oh My Pi's openai-models-list branch is one, and its fallback
			// on a missing field is a hardcoded 128K, not an error. That's
			// silently wrong for a model actually pinned smaller: a client
			// that trusts it builds an oversized request and fails mid-turn.
			// Omitted, never zero, when neither number is known, so `??
			// default` in the client falls through instead of landing on an
			// invented context window.
			if v, ok := resolveContextLength(entry.ContextSize, entry.TrainedContext); ok {
				row["context_length"] = v
			}
			data = append(data, row)
		}
	}
	// Snapshotted once and reused for every virtual row below AND every
	// endpoint row further down — Snapshot is O(endpoints); probing it again
	// per virtual model would make this handler O(virtuals × endpoints) for
	// no benefit, since every virtual model shares the same registry state.
	var epStatuses []EndpointStatus
	if p.registry != nil {
		epStatuses = p.registry.Snapshot(r.Context())
	}

	// Virtual rows come before endpoint rows: dispatch (handleProxy) checks
	// p.virtual.Find before p.registry.LookupModel, so a virtual name that
	// happens to collide with an endpoint's prefixed id (e.g. a virtual
	// literally named "ep/model") must win the dedup here too, or the
	// catalog would list a row a request for that id would never actually
	// reach (see the dedup comment above / code review item 6).
	if p.virtual != nil {
		for i := range p.virtual.Models {
			virtual := &p.virtual.Models[i]
			if seen[virtual.Name] {
				continue
			}
			seen[virtual.Name] = true
			// A virtual name is stable config — unlike an endpoint model, it
			// doesn't disappear from the catalog just because its targets are
			// all offline right now. It reports unloaded+failed instead, so a
			// client polling for readiness stops rather than spinning forever
			// on a name that will never resolve.
			data = append(data, p.virtualCatalogRow(virtual, epStatuses))
		}
	}

	for _, status := range epStatuses {
		if !status.Online {
			continue
		}
		for _, m := range status.Models {
			id := status.Endpoint.Name + "/" + m.ID
			if seen[id] {
				continue
			}
			seen[id] = true
			row := map[string]any{
				"id":       id,
				"object":   "model",
				"created":  0,
				"owned_by": status.Endpoint.Name,
				// Remote endpoints have no load step and the registry has
				// already dropped the unreachable ones, so anything listed
				// here is usable right now.
				"status": map[string]any{"value": ModelStatusLoaded},
				// Text unless the upstream advertised otherwise: plain
				// OpenAI /v1/models has no modality field, so a VLM behind
				// an endpoint that stays quiet is indistinguishable from a
				// text model. Emitting the key either way keeps every row
				// the same shape for clients that read
				// architecture.input_modalities unconditionally.
				"architecture": map[string]any{"input_modalities": endpointModalities(m)},
			}
			// Only when the upstream actually advertised one — omitting the
			// field lets the client apply its own default rather than
			// trusting a number we invented. context_length is the flat
			// counterpart to meta.n_ctx above, for the same reason it
			// exists on managed rows: see the comment there.
			if v, ok := resolveContextLength(m.ContextLength); ok {
				row["meta"] = map[string]any{"n_ctx": v}
				row["context_length"] = v
			}
			data = append(data, row)
		}
	}
	if data == nil {
		data = []map[string]any{}
	}

	writeRouterJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// handleModelLoad starts loading a managed model and returns immediately —
// see ServerManager.StartLoad for why this must not block.
func (p *RelayRouter) handleModelLoad(w http.ResponseWriter, r *http.Request) {
	mgr, model, ok := p.managedModelFromBody(w, r)
	if !ok {
		return
	}
	if err := mgr.StartLoad(model); err != nil {
		writeRouterError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeRouterJSON(w, http.StatusOK, map[string]any{"success": true, "model": model})
}

// handleModelUnload stops a managed model. Unloading something that is not
// running is a no-op rather than an error: callers use this to reach a state,
// not to perform a transition.
func (p *RelayRouter) handleModelUnload(w http.ResponseWriter, r *http.Request) {
	mgr, model, ok := p.managedModelFromBody(w, r)
	if !ok {
		return
	}
	if err := mgr.StopInstance(model); err != nil {
		slog.Debug("relay router: unload of a model that was not running", "model", model, "error", err)
	}
	writeRouterJSON(w, http.StatusOK, map[string]any{"success": true, "model": model})
}

// managedModelFromBody decodes {"model": "..."} and resolves it to the manager
// that owns it, writing the error response itself when it cannot.
func (p *RelayRouter) managedModelFromBody(w http.ResponseWriter, r *http.Request) (*ServerManager, string, bool) {
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
		writeRouterError(w, http.StatusBadRequest, "missing or invalid model field")
		return nil, "", false
	}
	for _, mgr := range p.managers {
		if mgr.HasAlias(body.Model) {
			return mgr, body.Model, true
		}
	}

	writeRouterError(w, http.StatusBadRequest,
		fmt.Sprintf("model %q is not a managed server; only llama-server and mlx-serve models can be loaded or unloaded", body.Model))
	return nil, "", false
}

// resolvedVirtualTarget is one candidate the router will actually try for a
// virtual model. manager set means an alias target; otherwise it's an
// endpoint target (endpoint + upstreamID).
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
func (p *RelayRouter) virtualCatalogRow(virtual *VirtualLLM, statuses []EndpointStatus) map[string]any {
	candidates, freshCount := candidatesForVirtual(virtual, statuses, p.managers)

	modalities := []string{"text"}
	var meta map[string]any
	var status map[string]any
	var contextLength int64
	var hasContextLength bool
	if freshCount > 0 {
		status = map[string]any{"value": ModelStatusLoaded}
		modalities, meta, contextLength, hasContextLength = virtualRowMetadata(candidates[0], statuses)
	} else {
		status = map[string]any{
			"value":  ModelStatusUnloaded,
			"failed": true,
			// Same convention as a managed alias's load failure: without this
			// a client polling for "loaded" spins forever instead of stopping.
			"error": virtualUnavailableReason(virtual, statuses),
		}
		if len(candidates) > 0 {
			// Still inherit metadata from the best last-resort candidate —
			// it's what a request would actually hit if it succeeded.
			modalities, meta, contextLength, hasContextLength = virtualRowMetadata(candidates[0], statuses)
		}
	}

	row := map[string]any{
		"id":           virtual.Name,
		"object":       "model",
		"created":      0,
		"owned_by":     "virtual",
		"status":       status,
		"architecture": map[string]any{"input_modalities": modalities},
	}
	if len(meta) > 0 {
		row["meta"] = meta
	}
	// context_length inherits the same precedence as a managed row's — see
	// the comment in handleModels — because a virtual name resolves to
	// whatever candidates[0] is, managed alias or endpoint target alike.
	if hasContextLength {
		row["context_length"] = contextLength
	}
	return row
}

// virtualRowMetadata inherits architecture/meta/context_length from a virtual
// model's first attempt-order candidate — the target dispatch will actually
// try first. A managed alias's metadata is a config fact, available whether
// or not the server is currently running; an endpoint target's metadata only
// exists when its last probe succeeded, so an offline candidate falls back to
// the same text-only/no-meta defaults the rest of /v1/models uses for
// anything unadvertised. Never claim "image" support that can't be backed —
// offering images to a server that can't take them fails mid-turn (see
// CLAUDE.md).
func virtualRowMetadata(first resolvedVirtualTarget, statuses []EndpointStatus) (modalities []string, meta map[string]any, contextLength int64, hasContextLength bool) {
	if first.manager != nil {
		for _, entry := range first.manager.ModelCatalog() {
			if entry.Alias != first.alias {
				continue
			}
			modalities = []string{"text"}
			if entry.SupportsImages {
				modalities = append(modalities, "image")
			}
			metaOut := map[string]any{}
			if entry.ContextSize > 0 {
				metaOut["n_ctx"] = entry.ContextSize
			}
			if entry.TrainedContext > 0 {
				metaOut["n_ctx_train"] = entry.TrainedContext
			}
			contextLength, hasContextLength = resolveContextLength(entry.ContextSize, entry.TrainedContext)
			if len(metaOut) == 0 {
				return modalities, nil, contextLength, hasContextLength
			}
			return modalities, metaOut, contextLength, hasContextLength
		}
		return []string{"text"}, nil, 0, false
	}

	for _, status := range statuses {
		if status.Endpoint.Name != first.endpoint.Name {
			continue
		}
		for _, m := range status.Models {
			if m.ID != first.upstreamID {
				continue
			}
			var metaOut map[string]any
			contextLength, hasContextLength = resolveContextLength(m.ContextLength)
			if hasContextLength {
				metaOut = map[string]any{"n_ctx": contextLength}
			}
			return endpointModalities(m), metaOut, contextLength, hasContextLength
		}
	}
	return []string{"text"}, nil, 0, false
}

// resolveContextLength picks the single number a catalog row's flat
// context_length field reports, in priority order: the first positive value
// wins. For a managed model that's (ContextSize, TrainedContext) — the
// pinned ctx-size is what the server will actually run with, the trained
// context is the honest fallback for a model with no pin — the same
// precedence as the meta block's n_ctx/n_ctx_train pair. For an endpoint
// model there's only ever one number (ContextLength). One function serves
// both because context_length is flat: unlike meta, it has no room to carry
// two numbers, so every row type must collapse to this single call.
func resolveContextLength(candidates ...int64) (int64, bool) {
	for _, v := range candidates {
		if v > 0 {
			return v, true
		}
	}
	return 0, false
}

// virtualUnavailableReason explains, for the catalog's status.error field,
// why a virtual model currently has no target believed usable. It only
// describes reachability (offline endpoints, missing aliases) — config
// mistakes (bad target shape, unknown endpoint/alias names) are
// warnVirtualModelConfig's job at startup, not a per-request runtime message.
func virtualUnavailableReason(virtual *VirtualLLM, statuses []EndpointStatus) string {
	configured := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		configured[status.Endpoint.Name] = true
	}
	var reasons []string
	for _, target := range virtual.Targets {
		switch {
		case target.Alias != "":
			reasons = append(reasons, fmt.Sprintf("alias %q not available", target.Alias))
		case target.Endpoint != "" && target.Model != "" && configured[target.Endpoint]:
			reasons = append(reasons, fmt.Sprintf("endpoint %q offline", target.Endpoint))
		}
	}
	if len(reasons) == 0 {
		return "no usable target configured"
	}
	return "no target currently reachable: " + strings.Join(reasons, "; ")
}

func writeRouterJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

// writeRouterError emits the {"error":{"message":...}} envelope that
// OpenAI-compatible and llama.cpp clients both parse for a human-readable
// reason.
func writeRouterError(w http.ResponseWriter, status int, msg string) {
	writeRouterJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg},
	})
}

func (p *RelayRouter) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// maxTranscriptionBytes caps a buffered audio upload. The handler below has to
// hold the whole request in memory to rewrite the model field, so this is the
// difference between a bounded cost per request and an OOM lever. 25 MB is
// OpenAI's own audio limit, which is what clients are written against.
// A var, not a const, so tests can exercise the overflow path without
// allocating 25 MB to do it.
var maxTranscriptionBytes int64 = 25 << 20

// handleAudioTranscription proxies OpenAI's speech-to-text endpoint.
//
// It exists because handleProxy cannot: that one decodes the body as JSON to
// find "model", and /v1/audio/transcriptions is multipart/form-data, so every
// request 400s on the envelope parse before routing is even attempted. Audio
// in, JSON transcript out — the shape is different enough to need its own door.
//
// Routing is endpoint-only, deliberately. Managed servers (llama.cpp, mlx) and
// virtual models are text-completion routes; neither serves audio, so a hit
// there would be a misconfiguration rather than a fallback worth honoring.
//
// The whole request is buffered because the model field must be rewritten from
// the router's prefixed id ("omlx/whatever") to the bare id the endpoint knows,
// and a multipart field cannot be edited in flight — its value may arrive after
// the file part. Re-emission reuses the ORIGINAL boundary so the client's
// Content-Type header stays correct and does not need rewriting too.
func (p *RelayRouter) handleAudioTranscription(w http.ResponseWriter, r *http.Request) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		writeRouterError(w, http.StatusBadRequest,
			"/v1/audio/transcriptions expects multipart/form-data")
		return
	}
	boundary := params["boundary"]
	if boundary == "" {
		writeRouterError(w, http.StatusBadRequest, "multipart body has no boundary")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTranscriptionBytes))
	r.Body.Close()
	if err != nil {
		// MaxBytesReader's error is the overflow case and deserves the status
		// that tells a client to send less, not a generic read failure.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeRouterError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("audio upload exceeds %d bytes", maxTranscriptionBytes))
			return
		}
		writeRouterError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	parts, model, err := readMultipartParts(bytes.NewReader(body), boundary)
	if err != nil {
		writeRouterError(w, http.StatusBadRequest, fmt.Sprintf("malformed multipart body: %v", err))
		return
	}
	if model == "" {
		writeRouterError(w, http.StatusBadRequest, "missing or empty model field")
		return
	}

	if p.registry == nil {
		writeRouterError(w, http.StatusServiceUnavailable, "no endpoints configured")
		return
	}
	ep, upstreamID, ok := p.registry.LookupModel(r.Context(), model)
	if !ok {
		slog.Warn("relay router: unknown transcription model", "model", model)
		writeRouterError(w, http.StatusBadRequest, fmt.Sprintf("unknown model %q", model))
		return
	}

	rewritten, err := rewriteMultipartModel(parts, boundary, upstreamID)
	if err != nil {
		slog.Warn("relay router: multipart rewrite failed", "endpoint", ep.Name, "error", err)
		writeRouterError(w, http.StatusInternalServerError, "failed to rewrite model field")
		return
	}

	target, err := url.Parse(ep.BaseURL)
	if err != nil {
		slog.Warn("relay router: bad endpoint baseURL", "endpoint", ep.Name, "baseURL", ep.BaseURL, "error", err)
		writeRouterError(w, http.StatusInternalServerError, "invalid endpoint configuration")
		return
	}
	newUpstreamProxy(target, rewritten, ep.APIKey, "audio", ep.Name, nil).ServeHTTP(w, r)
}

// multipartPart is one buffered form part. Audio uploads are small enough to
// hold whole (see maxTranscriptionBytes) and there is no way to rewrite a field
// that may arrive last without having read past it.
type multipartPart struct {
	name     string
	fileName string
	header   textproto.MIMEHeader
	data     []byte
}

// readMultipartParts buffers every part and returns the value of the "model"
// form field alongside them.
func readMultipartParts(r io.Reader, boundary string) ([]multipartPart, string, error) {
	mr := multipart.NewReader(r, boundary)
	var parts []multipartPart
	var model string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", err
		}
		data, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			return nil, "", err
		}
		if part.FormName() == "model" && part.FileName() == "" {
			model = string(data)
		}
		parts = append(parts, multipartPart{
			name:     part.FormName(),
			fileName: part.FileName(),
			header:   part.Header,
			data:     data,
		})
	}
	return parts, model, nil
}

// rewriteMultipartModel re-emits the parts with "model" replaced by the id the
// upstream endpoint expects. The original boundary is reused so the request's
// existing Content-Type header remains accurate — the reverse proxy forwards
// the client's headers, and a fresh boundary would silently contradict them.
//
// Every other part is copied through with its headers intact, so a file part
// keeps its filename and content type: mlx-audio backends route on the file
// extension, and dropping it changes how the audio gets decoded.
func rewriteMultipartModel(parts []multipartPart, boundary, upstreamID string) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("set boundary: %w", err)
	}
	for _, part := range parts {
		data := part.data
		if part.name == "model" && part.fileName == "" {
			data = []byte(upstreamID)
		}
		w, err := mw.CreatePart(part.header)
		if err != nil {
			return nil, fmt.Errorf("create part %q: %w", part.name, err)
		}
		if _, err := w.Write(data); err != nil {
			return nil, fmt.Errorf("write part %q: %w", part.name, err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart: %w", err)
	}
	return buf.Bytes(), nil
}

func (p *RelayRouter) handleProxy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}

	var envelope struct {
		Model          string `json:"model"`
		PromptCacheKey string `json:"prompt_cache_key"`
		User           string `json:"user"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Model == "" {
		http.Error(w, `{"error":"missing or invalid model field"}`, http.StatusBadRequest)
		return
	}

	// Managed servers checked in priority order (llama first, then mlx).
	// First HasAlias match wins — llama wins on collision.
	for _, mgr := range p.managers {
		if mgr.HasAlias(envelope.Model) {
			p.routeManaged(w, r, mgr, envelope.Model, body)
			return
		}
	}

	if p.virtual != nil {
		if virtual := p.virtual.Find(envelope.Model); virtual != nil {
			candidates := p.virtualCandidates(r.Context(), envelope.Model)
			if len(candidates) == 0 {
				// A configured virtual name is never "unknown" — that error
				// sends whoever's debugging after the wrong problem. Every
				// target here is misconfigured (bad endpoint/alias
				// reference), not missing; warnVirtualModelConfig already
				// flagged this at startup.
				writeRouterError(w, http.StatusServiceUnavailable,
					fmt.Sprintf("virtual model %q: no usable target configured", envelope.Model))
				return
			}
			// A pinned target (see ADR-010) outranks reachability ordering —
			// it goes to the front even if candidatesForVirtual currently
			// believes something else is more reachable. A pin naming a
			// target dropped from candidates (removed from config) is a
			// no-op inside applyAffinity, so routing falls back to the
			// normal order.
			affinityKey := affinityKeyFromBody(envelope.PromptCacheKey, envelope.User)
			candidates = applyAffinity(candidates, p.affinity.lookup(envelope.Model, affinityKey))
			p.routeVirtual(w, r, envelope.Model, candidates, body, affinityKey)
			return
		}
	}

	if p.registry != nil {
		if ep, upstreamID, ok := p.registry.LookupModel(r.Context(), envelope.Model); ok {
			p.routeOpenAI(w, r, ep, upstreamID, body)
			return
		}
	}

	slog.Warn("relay router: unknown model", "model", envelope.Model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("unknown model %q", envelope.Model)})
}

func (p *RelayRouter) routeManaged(w http.ResponseWriter, r *http.Request, mgr *ServerManager, alias string, body []byte) {
	// The lease is held for the whole proxied exchange, including the SSE
	// stream, so the budget cannot evict this instance mid-response.
	endpoint, release, err := mgr.Acquire(alias)
	if err != nil {
		slog.Warn("relay router: failed to launch managed server", "kind", mgr.profile.Kind, "model", alias, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	defer release()

	// No model swap needed here — the client already sent the bare alias the
	// managed server expects — but the reasoning_effort rewrite still applies
	// (see RouterConfig): a client hitting a managed alias has exactly the
	// same backend-vocabulary problem as one hitting an endpoint.
	rewritten, err := rewriteProxyBody(body, "", p.reasoningEffortMap, p.reasoningEffortTemplateKwargs)
	if err != nil {
		slog.Warn("relay router: body rewrite failed", "alias", alias, "error", err)
		writeRouterError(w, http.StatusBadRequest, "failed to rewrite request body")
		return
	}

	target, err := url.Parse(endpoint.BaseURL)
	if err != nil {
		// endpoint.BaseURL is normally built internally (e.g.
		// "http://127.0.0.1:<port>/v1") and always valid, but a dropped error
		// here used to leave target nil — newUpstreamProxy's Director
		// dereferences target.Scheme unconditionally, so a bad URL panicked
		// inside the handler instead of failing the request (code review
		// item 7).
		slog.Warn("relay router: bad managed server endpoint", "kind", mgr.profile.Kind, "alias", alias, "error", err)
		writeRouterError(w, http.StatusBadGateway, fmt.Sprintf("invalid managed server endpoint: %v", err))
		return
	}
	newUpstreamProxy(target, rewritten, endpoint.APIKey, mgr.profile.Kind, alias, nil).ServeHTTP(w, r)
}

// routeOpenAI rewrites the body's `model` to the bare upstream id (so OMLX
// et al. see their own name, not "omlx/X") and forwards to the endpoint.
func (p *RelayRouter) routeOpenAI(w http.ResponseWriter, r *http.Request, ep OpenAIEndpoint, upstreamID string, body []byte) {
	rewritten, err := rewriteProxyBody(body, upstreamID, p.reasoningEffortMap, p.reasoningEffortTemplateKwargs)
	if err != nil {
		slog.Warn("relay router: body rewrite failed", "endpoint", ep.Name, "error", err)
		http.Error(w, `{"error":"failed to rewrite model field"}`, http.StatusBadRequest)
		return
	}
	target, err := url.Parse(ep.BaseURL)
	if err != nil {
		slog.Warn("relay router: bad endpoint baseURL", "endpoint", ep.Name, "baseURL", ep.BaseURL, "error", err)
		http.Error(w, `{"error":"invalid endpoint configuration"}`, http.StatusInternalServerError)
		return
	}
	newUpstreamProxy(target, rewritten, ep.APIKey, "openai", ep.Name, nil).ServeHTTP(w, r)
}

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
	proxy, release, buildErr := p.buildVirtualAttempt(target, body, func(e error) bool {
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
func (p *RelayRouter) buildVirtualAttempt(target resolvedVirtualTarget, body []byte, onError func(error) bool) (proxy *httputil.ReverseProxy, release func(), err error) {
	if target.manager != nil {
		endpoint, rel, err := target.manager.Acquire(target.alias)
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
	proxy.Transport = virtualDialTransport
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

// newUpstreamProxy builds the reverse proxy shared by every branch. The
// Director replaces (or clears) Authorization so the inbound bearer token —
// which is relayLLM's internal token, meaningless to upstreams — never
// leaks across the trust boundary.
//
// onError, when non-nil, is consulted before the default 502 is written on a
// backend failure. httputil.ReverseProxy only invokes ErrorHandler on a
// RoundTrip failure, which always happens before any response byte is
// written — a body-copy failure after headers are sent is not routed through
// here. Returning true means the caller is handling the failure itself (the
// virtual-model retry path: a failed attempt must emit nothing so the next
// candidate gets a clean response to write into); false or nil falls through
// to the normal 502 body.
func newUpstreamProxy(target *url.URL, body []byte, apiKey, branch, label string, onError func(error) bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			if apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+apiKey)
			} else {
				req.Header.Del("Authorization")
			}
		},
		FlushInterval: -1, // flush immediately for SSE streaming
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("relay router: backend error", "branch", branch, "target", label, "error", err)
			if onError != nil && onError(err) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("backend error: %v", err)})
		},
	}
}

// rewriteProxyBody applies the router's field-level body rewrites in one
// decode/encode pass, so an endpoint-routed body is never unmarshalled and
// remarshalled twice for independent rewrites. model, when non-empty,
// replaces the top-level "model" field (endpoint routes rewrite
// "endpoint.Name/id" down to the bare id the endpoint itself expects — this
// used to be rewriteModelField's whole job). effortMap, when non-empty,
// rewrites or removes a top-level string "reasoning_effort" field; see
// applyReasoningEffortMap. templateKwargsMap, when non-empty, merges an
// object into a top-level "chat_template_kwargs" field; see
// applyReasoningEffortTemplateKwargs.
//
// When all three are no-ops (no model swap, no configured maps) the body is
// returned completely untouched rather than round-tripped through
// encoding/json — that's load-bearing for the managed-alias route, which has
// no model to swap: with neither map configured (the default), it must stay
// byte-identical to before these features existed, not just semantically
// unchanged with reordered keys.
//
// RawMessage avoids re-marshalling nested payloads verbatim (large
// image_url parts, ordered tool definitions, etc.) when a rewrite does
// happen. Top-level key order is not preserved in that case: json.Marshal of
// a map sorts keys.
func rewriteProxyBody(body []byte, model string, effortMap map[string]string, templateKwargsMap map[string]map[string]any) ([]byte, error) {
	if model == "" && len(effortMap) == 0 && len(templateKwargsMap) == 0 {
		return body, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if model != "" {
		encoded, err := json.Marshal(model)
		if err != nil {
			return nil, fmt.Errorf("encode upstream id: %w", err)
		}
		raw["model"] = encoded
	}

	// Capture the client's ORIGINAL reasoning_effort value before
	// applyReasoningEffortMap gets a chance to rewrite or remove it. Both
	// reasoning_effort rewrites key off this same original value — see
	// RouterConfig.ReasoningEffortTemplateKwargs for why matching after the
	// value-map swap would be wrong: {"minimal":"none"} (effortMap) and
	// {"minimal":{"enable_thinking":false}} (templateKwargsMap) describe two
	// independent reactions to ONE client value, "minimal". Reading the
	// field again after applyReasoningEffortMap ran would see "none" instead
	// (or nothing, if minimal maps to removal), silently breaking that
	// combination.
	effort, hasEffort := reasoningEffortValue(raw)

	applyReasoningEffortMap(raw, effortMap)
	if hasEffort {
		applyReasoningEffortTemplateKwargs(raw, templateKwargsMap, effort)
	}
	return json.Marshal(raw)
}

// reasoningEffortValue reads raw's top-level "reasoning_effort" field as a
// string, reporting ok=false when the field is absent or not a JSON string.
// Factored out of applyReasoningEffortMap so rewriteProxyBody can capture
// the field's value BEFORE that function has a chance to mutate or remove
// it — see rewriteProxyBody's comment on why the original value is what
// applyReasoningEffortTemplateKwargs must match against.
func reasoningEffortValue(raw map[string]json.RawMessage) (string, bool) {
	rawEffort, ok := raw["reasoning_effort"]
	if !ok {
		return "", false
	}
	var effort string
	if err := json.Unmarshal(rawEffort, &effort); err != nil {
		return "", false // not a JSON string — leave whatever it is alone.
	}
	return effort, true
}

// applyReasoningEffortMap rewrites, or removes, a top-level string
// "reasoning_effort" field in raw per effortMap — see RouterConfig for why
// this exists. Mutates raw in place; a no-op effortMap or a body with
// nothing to rewrite leaves raw untouched.
//
// Only an exact, case-sensitive match on the field's current string value is
// rewritten. Everything else passes through deliberately: a missing field, a
// non-string value (some other client sending a number isn't ours to
// interpret), and a string that isn't a configured key (it may already be
// exactly what the backend wants).
func applyReasoningEffortMap(raw map[string]json.RawMessage, effortMap map[string]string) {
	if len(effortMap) == 0 {
		return
	}
	effort, ok := reasoningEffortValue(raw)
	if !ok {
		return
	}
	mapped, ok := effortMap[effort]
	if !ok {
		return
	}
	if mapped == "" {
		// Some backends reject an empty string outright; "omit the field" is
		// a distinct, useful outcome from "set it to none" — see RouterConfig.
		delete(raw, "reasoning_effort")
		slog.Debug("relay router: removed reasoning_effort", "from", effort)
		return
	}
	encoded, err := json.Marshal(mapped)
	if err != nil {
		return // unreachable in practice (marshalling a string cannot fail)
	}
	raw["reasoning_effort"] = encoded
	slog.Debug("relay router: rewrote reasoning_effort", "from", effort, "to", mapped)
}

// applyReasoningEffortTemplateKwargs merges kwargsMap[effort] into raw's
// top-level "chat_template_kwargs" object, creating it if absent. effort is
// the client's ORIGINAL "reasoning_effort" value — the caller (
// rewriteProxyBody) captures it before applyReasoningEffortMap runs, per
// RouterConfig.ReasoningEffortTemplateKwargs's doc comment. Mutates raw in
// place; a no-op kwargsMap, a non-matching effort, or an empty configured
// object leave raw untouched.
//
// A key the merge would set is left alone if raw's existing
// chat_template_kwargs already defines it — mirroring oMLX's own
// merged.setdefault(...) server-side, so a client's explicit choice always
// wins over ours (see RouterConfig).
func applyReasoningEffortTemplateKwargs(raw map[string]json.RawMessage, kwargsMap map[string]map[string]any, effort string) {
	if len(kwargsMap) == 0 {
		return
	}
	kwargs, ok := kwargsMap[effort]
	if !ok || len(kwargs) == 0 {
		return
	}

	existing := map[string]json.RawMessage{}
	if rawExisting, present := raw["chat_template_kwargs"]; present {
		if err := json.Unmarshal(rawExisting, &existing); err != nil {
			// Not a JSON object — a malformed client body isn't ours to fix;
			// leave it untouched rather than clobbering it with our own.
			return
		}
	}

	changed := false
	for k, v := range kwargs {
		if _, present := existing[k]; present {
			continue // client-supplied value wins — setdefault semantics.
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			continue // unreachable: config values decode from JSON already
		}
		existing[k] = encoded
		changed = true
	}
	if !changed {
		return
	}
	encoded, err := json.Marshal(existing)
	if err != nil {
		return // unreachable: existing is built entirely from valid RawMessages
	}
	raw["chat_template_kwargs"] = encoded
	slog.Debug("relay router: merged reasoning_effort template kwargs", "reasoning_effort", effort, "keys", kwargs)
}

// StartRelayRouter starts the router in a background goroutine. Returns nil
// (no-op) when addr is empty or no live backend remains after dropping nil
// managers.
//
// router (may be nil) carries RouterConfig-level behavior — the
// reasoning_effort rewrite map and its sibling chat_template_kwargs merge
// table — and both are applied via their setters before the serving
// goroutine is spawned, not after StartRelayRouter returns. That ordering is
// load-bearing, not stylistic: Go's memory model guarantees a goroutine's
// creation happens-before its execution, so setting the fields first means
// every connection-handling goroutine transitively spawned from the one
// below is guaranteed to observe them. The previous shape — main calling the
// router's (then-exported) SetReasoningEffortMap after StartRelayRouter had
// already returned — left a window where a request accepted the instant the
// listener came up could read the field concurrently with that write, an
// unsynchronized race the detector flags under real traffic (code review
// item 4). StartRelayRouter is the one production call site for this,
// chosen over adding the parameters to NewRelayRouter because it has far
// fewer call sites to touch.
func StartRelayRouter(addr string, managers []*ServerManager, registry *ProxyRegistry, virtual *VirtualLLMConfig, router *RouterConfig) *RelayRouter {
	if addr == "" {
		return nil
	}
	p := NewRelayRouter(addr, managers, registry, virtual)
	if len(p.managers) == 0 && p.registry == nil {
		return nil
	}
	if router != nil {
		p.setReasoningEffortMap(router.ReasoningEffortMap)
		p.setReasoningEffortTemplateKwargs(router.ReasoningEffortTemplateKwargs)
	}
	go func() {
		if err := p.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("relay router error", "error", err)
		}
	}()
	return p
}

// endpointModalities renders an upstream model's advertised input modalities.
// Text is always present: every chat model takes text, and a client that finds
// an empty list has nothing to fall back on.
func endpointModalities(m UpstreamModel) []string {
	if m.SupportsImages {
		return []string{"text", "image"}
	}
	return []string{"text"}
}
