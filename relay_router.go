package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// RelayRouter aggregates managed-server aliases (llama.cpp, MLX, …) and
// reachable OpenAI endpoint models behind one OpenAI-compatible listener.
// Dispatch is by the request body's `model` field: bare alias → first
// matching manager; virtual name → first reachable configured target;
// `endpoint.Name/id` → matching OpenAI upstream.
// Endpoints that fail their last probe drop out of /v1/models and refuse
// routing until the next 15s TTL cycle.
type RelayRouter struct {
	managers []*ServerManager
	registry *ProxyRegistry
	virtual  *VirtualLLMConfig
	server   *http.Server
}

// NewRelayRouter creates a router on addr. Nil entries in managers are
// dropped; registry may be nil to disable the endpoint branch. A router with
// no live backends 400s every request — StartRelayRouter guards against
// starting one.
func NewRelayRouter(addr string, managers []*ServerManager, registry *ProxyRegistry, virtual ...*VirtualLLMConfig) *RelayRouter {
	live := make([]*ServerManager, 0, len(managers))
	for _, m := range managers {
		if m != nil {
			live = append(live, m)
		}
	}
	p := &RelayRouter{managers: live, registry: registry}
	if len(virtual) > 0 {
		p.virtual = virtual[0]
	}

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
	// Dispatch gives an alias to the first manager that has it, so list each
	// alias once, under the manager that actually serves it.
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
	if p.registry != nil {
		for _, status := range p.registry.Snapshot(r.Context()) {
			if !status.Online {
				continue
			}
			for _, m := range status.Models {
				row := map[string]any{
					"id":       status.Endpoint.Name + "/" + m.ID,
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
		for _, virtual := range p.virtual.Models {
			if _, ok := p.resolveVirtual(r, virtual.Name); !ok {
				continue
			}
			data = append(data, map[string]any{
				"id":           virtual.Name,
				"object":       "model",
				"created":      0,
				"owned_by":     "virtual",
				"status":       map[string]any{"value": ModelStatusLoaded},
				"architecture": map[string]any{"input_modalities": []string{"text"}},
			})
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

// resolveVirtual probes the configured endpoint registry as needed, then
// chooses the first online target in the virtual model's declared order.
type resolvedVirtualTarget struct {
	endpoint   OpenAIEndpoint
	upstreamID string
	manager    *ServerManager
	alias      string
}

func (p *RelayRouter) resolveVirtual(r *http.Request, name string) (resolvedVirtualTarget, bool) {
	if p.virtual == nil {
		return resolvedVirtualTarget{}, false
	}
	virtual := p.virtual.Find(name)
	if virtual == nil || virtual.Name == "" {
		return resolvedVirtualTarget{}, false
	}
	online := make(map[string]OpenAIEndpoint)
	if p.registry != nil {
		for _, status := range p.registry.Snapshot(r.Context()) {
			if status.Online {
				online[status.Endpoint.Name] = status.Endpoint
			}
		}
	}
	for _, target := range virtual.Targets {
		if target.Endpoint != "" && target.Model != "" {
			if endpoint, ok := online[target.Endpoint]; ok {
				return resolvedVirtualTarget{endpoint: endpoint, upstreamID: target.Model}, true
			}
		}
		if target.Alias != "" {
			for _, manager := range p.managers {
				if manager.HasAlias(target.Alias) {
					return resolvedVirtualTarget{manager: manager, alias: target.Alias}, true
				}
			}
		}
	}
	return resolvedVirtualTarget{}, false
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
	if target, ok := p.resolveVirtual(r, envelope.Model); ok {
		if target.manager != nil {
			p.routeManaged(w, r, target.manager, target.alias, body)
		} else {
			p.routeOpenAI(w, r, target.endpoint, target.upstreamID, body)
		}
		return
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
	newUpstreamProxy(target, body, endpoint.APIKey, mgr.profile.Kind, alias).ServeHTTP(w, r)
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
	newUpstreamProxy(target, rewritten, ep.APIKey, "openai", ep.Name).ServeHTTP(w, r)
}

// newUpstreamProxy builds the reverse proxy shared by both branches. The
// Director replaces (or clears) Authorization so the inbound bearer token —
// which is relayLLM's internal token, meaningless to upstreams — never
// leaks across the trust boundary.
func newUpstreamProxy(target *url.URL, body []byte, apiKey, branch, label string) *httputil.ReverseProxy {
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
func StartRelayRouter(addr string, managers []*ServerManager, registry *ProxyRegistry, virtual ...*VirtualLLMConfig) *RelayRouter {
	if addr == "" {
		return nil
	}
	p := NewRelayRouter(addr, managers, registry, virtual...)
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
