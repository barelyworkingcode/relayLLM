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

// LlamaProxy is a thin OpenAI-compatible reverse proxy that routes requests
// to the correct llama-server instance based on the "model" field in the
// request body. External clients hit a single endpoint; the proxy handles
// launching and routing transparently.
type LlamaProxy struct {
	manager *LlamaServerManager
	server  *http.Server
}

// NewLlamaProxy creates a proxy on the given address (e.g. ":8080").
func NewLlamaProxy(addr string, manager *LlamaServerManager) *LlamaProxy {
	p := &LlamaProxy{manager: manager}

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

// ListenAndServe starts the proxy. Blocks until the server stops.
func (p *LlamaProxy) ListenAndServe() error {
	slog.Info("llama proxy listening", "addr", p.server.Addr)
	return p.server.ListenAndServe()
}

// Close gracefully shuts down the proxy.
func (p *LlamaProxy) Close() error {
	return p.server.Close()
}

// handleModels returns an OpenAI-compatible /v1/models response listing
// all configured llama models.
func (p *LlamaProxy) handleModels(w http.ResponseWriter, r *http.Request) {
	aliases := p.manager.Aliases()
	data := make([]map[string]any, len(aliases))
	for i, alias := range aliases {
		data[i] = map[string]any{
			"id":       alias,
			"object":   "model",
			"created":  0,
			"owned_by": "llama.cpp",
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (p *LlamaProxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// handleProxy reads the model from the request body, launches/finds the
// right llama-server, and reverse-proxies the entire request to it.
func (p *LlamaProxy) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Read the body so we can peek at the model field.
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}

	// Extract model name.
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Model == "" {
		http.Error(w, `{"error":"missing or invalid model field"}`, http.StatusBadRequest)
		return
	}

	// Launch or reuse the server for this model.
	endpoint, err := p.manager.GetOrLaunch(envelope.Model)
	if err != nil {
		slog.Warn("llama proxy: failed to get server", "model", envelope.Model, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Reverse proxy to the backend.
	target, _ := url.Parse(endpoint.BaseURL)
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			// Preserve the original path (e.g. /v1/chat/completions).
			req.Host = target.Host
			// Restore the body.
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			// Forward auth if the endpoint has a key.
			if endpoint.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
			}
		},
		FlushInterval: -1, // flush immediately for SSE streaming
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("llama proxy: backend error", "model", envelope.Model, "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("backend error: %v", err)})
		},
	}
	proxy.ServeHTTP(w, r)
}

// StartLlamaProxy starts the proxy in a background goroutine. Returns the
// proxy for later shutdown. Does nothing if addr is empty.
func StartLlamaProxy(addr string, manager *LlamaServerManager) *LlamaProxy {
	if addr == "" || manager == nil {
		return nil
	}
	p := NewLlamaProxy(addr, manager)
	go func() {
		if err := p.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("llama proxy error", "error", err)
		}
	}()
	return p
}
