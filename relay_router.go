package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
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
	server   *http.Server
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
	p := &RelayRouter{managers: live, registry: registry, virtual: virtual}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", p.handleModels)
	mux.HandleFunc("GET /models", p.handleModels)
	mux.HandleFunc("POST /models/load", p.handleModelLoad)
	mux.HandleFunc("POST /models/unload", p.handleModelUnload)
	mux.HandleFunc("GET /health", p.handleHealth)
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
	// Dispatch resolves a model id to exactly one behavior, so every row type
	// (managed alias, endpoint model, virtual name) shares this dedup set —
	// a name that collides with an earlier row is dead config either way.
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
			data = append(data, row)
		}
	}
	// Snapshotted once and reused for every endpoint row below AND every
	// virtual row further down — Snapshot is O(endpoints); probing it again
	// per virtual model would make this handler O(virtuals × endpoints) for
	// no benefit, since every virtual model shares the same registry state.
	var epStatuses []EndpointStatus
	if p.registry != nil {
		epStatuses = p.registry.Snapshot(r.Context())
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
				// trusting a number we invented.
				if m.ContextLength > 0 {
					row["meta"] = map[string]any{"n_ctx": m.ContextLength}
				}
				data = append(data, row)
			}
		}
	}
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

// label renders the target for a human-readable failure message.
func (t resolvedVirtualTarget) label() string {
	if t.manager != nil {
		return fmt.Sprintf("alias %q", t.alias)
	}
	return fmt.Sprintf("endpoint %q", t.endpoint.Name)
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
		switch {
		case target.Endpoint != "" && target.Model != "":
			if endpoint, ok := online[target.Endpoint]; ok {
				fresh = append(fresh, resolvedVirtualTarget{endpoint: endpoint, upstreamID: target.Model})
			} else if endpoint, ok := configured[target.Endpoint]; ok {
				stale = append(stale, resolvedVirtualTarget{endpoint: endpoint, upstreamID: target.Model})
			}
			// else: names an endpoint that doesn't exist in config — skip.
		case target.Alias != "":
			for _, manager := range managers {
				if manager.HasAlias(target.Alias) {
					fresh = append(fresh, resolvedVirtualTarget{manager: manager, alias: target.Alias})
					break
				}
			}
			// else: no manager has this alias — skip.
		}
		// else: neither shape (e.g. endpoint set without model) — skip.
		// warnVirtualModelConfig flags this at startup.
	}
	return append(fresh, stale...), len(fresh)
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
	if freshCount > 0 {
		status = map[string]any{"value": ModelStatusLoaded}
		modalities, meta = virtualRowMetadata(candidates[0], statuses)
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
			modalities, meta = virtualRowMetadata(candidates[0], statuses)
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
	return row
}

// virtualRowMetadata inherits architecture/meta from a virtual model's first
// attempt-order candidate — the target dispatch will actually try first. A
// managed alias's metadata is a config fact, available whether or not the
// server is currently running; an endpoint target's metadata only exists
// when its last probe succeeded, so an offline candidate falls back to the
// same text-only/no-meta defaults the rest of /v1/models uses for anything
// unadvertised. Never claim "image" support that can't be backed — offering
// images to a server that can't take them fails mid-turn (see CLAUDE.md).
func virtualRowMetadata(first resolvedVirtualTarget, statuses []EndpointStatus) ([]string, map[string]any) {
	if first.manager != nil {
		for _, entry := range first.manager.ModelCatalog() {
			if entry.Alias != first.alias {
				continue
			}
			modalities := []string{"text"}
			if entry.SupportsImages {
				modalities = append(modalities, "image")
			}
			meta := map[string]any{}
			if entry.ContextSize > 0 {
				meta["n_ctx"] = entry.ContextSize
			}
			if entry.TrainedContext > 0 {
				meta["n_ctx_train"] = entry.TrainedContext
			}
			if len(meta) == 0 {
				return modalities, nil
			}
			return modalities, meta
		}
		return []string{"text"}, nil
	}

	for _, status := range statuses {
		if status.Endpoint.Name != first.endpoint.Name {
			continue
		}
		for _, m := range status.Models {
			if m.ID != first.upstreamID {
				continue
			}
			var meta map[string]any
			if m.ContextLength > 0 {
				meta = map[string]any{"n_ctx": m.ContextLength}
			}
			return endpointModalities(m), meta
		}
	}
	return []string{"text"}, nil
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

func (p *RelayRouter) handleProxy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}

	var envelope struct {
		Model string `json:"model"`
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
			p.routeVirtual(w, r, envelope.Model, candidates, body)
			return
		}
	}

	if p.registry != nil {
		if ep, upstreamID, ok := p.registry.LookupModel(envelope.Model); ok {
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

	target, _ := url.Parse(endpoint.BaseURL)
	newUpstreamProxy(target, body, endpoint.APIKey, mgr.profile.Kind, alias, nil).ServeHTTP(w, r)
}

// routeOpenAI rewrites the body's `model` to the bare upstream id (so OMLX
// et al. see their own name, not "omlx/X") and forwards to the endpoint.
func (p *RelayRouter) routeOpenAI(w http.ResponseWriter, r *http.Request, ep OpenAIEndpoint, upstreamID string, body []byte) {
	rewritten, err := rewriteModelField(body, upstreamID)
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
func (p *RelayRouter) routeVirtual(w http.ResponseWriter, r *http.Request, name string, candidates []resolvedVirtualTarget, body []byte) {
	var failures []string
	for _, target := range candidates {
		wrote, err := p.attemptVirtual(w, r, target, body)
		if err == nil {
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
// ResponseWriter through a recorder that tracks whether anything was written.
// release is deferred (rather than called after ServeHTTP returns) because a
// mid-stream backend failure in a real net/http server panics with
// http.ErrAbortHandler — recovered by the standard library one frame up —
// and a bare post-call release() would leak the managed-server lease on that
// path.
func (p *RelayRouter) attemptVirtual(w http.ResponseWriter, r *http.Request, target resolvedVirtualTarget, body []byte) (wrote bool, err error) {
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
		return false, buildErr
	}
	defer release()

	rec := &virtualResponseRecorder{ResponseWriter: w}
	proxy.ServeHTTP(rec, r)
	if backendErr != nil {
		return rec.wrote, backendErr
	}
	return rec.wrote, nil
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
		targetURL, _ := url.Parse(endpoint.BaseURL)
		proxy := newUpstreamProxy(targetURL, body, endpoint.APIKey, target.manager.profile.Kind, target.alias, onError)
		proxy.Transport = virtualDialTransport
		return proxy, rel, nil
	}

	rewritten, err := rewriteModelField(body, target.upstreamID)
	if err != nil {
		return nil, nil, fmt.Errorf("rewrite model field: %w", err)
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
// the client, the exchange is committed.
type virtualResponseRecorder struct {
	http.ResponseWriter
	wrote bool
}

func (v *virtualResponseRecorder) WriteHeader(statusCode int) {
	v.wrote = true
	v.ResponseWriter.WriteHeader(statusCode)
}

func (v *virtualResponseRecorder) Write(b []byte) (int, error) {
	v.wrote = true
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

// rewriteModelField swaps the top-level "model" field for upstreamID,
// preserving every other field verbatim (RawMessage avoids re-marshalling
// nested payloads — large image_url parts, ordered tool definitions, etc.).
// Top-level key order is not preserved: json.Marshal of a map sorts keys.
func rewriteModelField(body []byte, upstreamID string) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	encoded, err := json.Marshal(upstreamID)
	if err != nil {
		return nil, fmt.Errorf("encode upstream id: %w", err)
	}
	raw["model"] = encoded
	return json.Marshal(raw)
}

// StartRelayRouter starts the router in a background goroutine. Returns nil
// (no-op) when addr is empty or no live backend remains after dropping nil
// managers.
func StartRelayRouter(addr string, managers []*ServerManager, registry *ProxyRegistry, virtual *VirtualLLMConfig) *RelayRouter {
	if addr == "" {
		return nil
	}
	p := NewRelayRouter(addr, managers, registry, virtual)
	if len(p.managers) == 0 && p.registry == nil {
		return nil
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
