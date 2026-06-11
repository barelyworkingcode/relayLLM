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
// matching manager; `endpoint.Name/id` → matching OpenAI upstream.
// Endpoints that fail their last probe drop out of /v1/models and refuse
// routing until the next 15s TTL cycle.
type RelayRouter struct {
	managers []*ServerManager
	registry *ProxyRegistry
	server   *http.Server
}

// NewRelayRouter creates a router on addr. Nil entries in managers are
// dropped; registry may be nil to disable the endpoint branch. A router with
// no live backends 400s every request — StartRelayRouter guards against
// starting one.
func NewRelayRouter(addr string, managers []*ServerManager, registry *ProxyRegistry) *RelayRouter {
	live := make([]*ServerManager, 0, len(managers))
	for _, m := range managers {
		if m != nil {
			live = append(live, m)
		}
	}
	p := &RelayRouter{managers: live, registry: registry}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", p.handleModels)
	mux.HandleFunc("GET /models", p.handleModels)
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

func (p *RelayRouter) handleModels(w http.ResponseWriter, r *http.Request) {
	var data []map[string]any
	// Dispatch gives an alias to the first manager that has it, so list each
	// alias once, under the manager that actually serves it.
	seen := make(map[string]bool)
	for _, m := range p.managers {
		for _, alias := range m.Aliases() {
			if seen[alias] {
				continue
			}
			seen[alias] = true
			data = append(data, map[string]any{
				"id":       alias,
				"object":   "model",
				"created":  0,
				"owned_by": m.profile.Group,
			})
		}
	}
	if p.registry != nil {
		for _, status := range p.registry.Snapshot(r.Context()) {
			if !status.Online {
				continue
			}
			for _, id := range status.Models {
				data = append(data, map[string]any{
					"id":       status.Endpoint.Name + "/" + id,
					"object":   "model",
					"created":  0,
					"owned_by": status.Endpoint.Name,
				})
			}
		}
	}
	if data == nil {
		data = []map[string]any{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
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
	endpoint, err := mgr.GetOrLaunch(alias)
	if err != nil {
		slog.Warn("relay router: failed to launch managed server", "kind", mgr.profile.Kind, "model", alias, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
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
func StartRelayRouter(addr string, managers []*ServerManager, registry *ProxyRegistry) *RelayRouter {
	if addr == "" {
		return nil
	}
	p := NewRelayRouter(addr, managers, registry)
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
