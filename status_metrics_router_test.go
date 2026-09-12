package main

// Router-level coverage for status_metrics.go's wiring into handleProxy /
// routeVirtual (relay_router.go, relay_router_virtual.go) — the double-count
// and ghost-entry regressions that only show up once real dispatch is in
// the loop. Reuses relay_router_test.go's postBytes/httptest machinery.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"relayllm/internal/config"
	"strconv"
	"testing"
)

// injectHealthyManagedInstance registers alias as an already-running, healthy
// instance pointed at upstream's real listener — the same injection pattern
// server_budget_test.go's addInstance uses to exercise Acquire without
// spawning a real llama-server/mlx-serve binary, except here the port is a
// real httptest.Server so a subsequent reverse-proxy hop actually completes.
func injectHealthyManagedInstance(t *testing.T, m *ServerManager, alias string, upstream *httptest.Server) {
	t.Helper()
	_, portStr, err := splitHostPort(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	m.InjectReadyInstanceForTest(alias, port, 0)
}

func splitHostPort(rawURL string) (host, port string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}
	return net.SplitHostPort(u.Host)
}

func TestProxyMetrics_ManagedRouteRecordsTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	mgr := NewServerManager(llamaProfile, &config.ServerConfig{
		Models: []config.ServerModelConfig{{Alias: "test-alias"}},
	}, "")
	injectHealthyManagedInstance(t, mgr, "test-alias", upstream)

	r := NewRelayRouter(":0", []*ServerManager{mgr}, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"test-alias"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	io.ReadAll(resp.Body)

	active, _, recent := r.Metrics().Snapshot()
	if len(active) != 0 {
		t.Errorf("active connections after request completed = %+v, want empty", active)
	}
	if len(recent) != 1 {
		t.Fatalf("recentRequests = %+v, want exactly 1 entry", recent)
	}
	if recent[0].TargetKind != "managed" {
		t.Errorf("recent[0].TargetKind = %q, want %q", recent[0].TargetKind, "managed")
	}
	if recent[0].Target != "llama:test-alias" {
		t.Errorf("recent[0].Target = %q, want %q", recent[0].Target, "llama:test-alias")
	}
	if recent[0].Status != http.StatusOK {
		t.Errorf("recent[0].Status = %d, want 200", recent[0].Status)
	}
}

func TestProxyMetrics_EndpointRouteRecordsTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "Qwen"}}})
		case "/v1/chat/completions":
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstream.Close()
	registry := NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{
		{Name: "fakeep", BaseURL: upstream.URL + "/v1"},
	}})

	r := NewRelayRouter(":0", nil, registry, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"fakeep/Qwen"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	io.ReadAll(resp.Body)

	_, _, recent := r.Metrics().Snapshot()
	if len(recent) != 1 {
		t.Fatalf("recentRequests = %+v, want exactly 1 entry", recent)
	}
	if recent[0].TargetKind != "endpoint" || recent[0].Target != "fakeep/Qwen" {
		t.Errorf("recent[0] = %+v, want targetKind=endpoint target=fakeep/Qwen", recent[0])
	}
	if recent[0].Status != http.StatusOK {
		t.Errorf("recent[0].Status = %d, want 200", recent[0].Status)
	}
}

// The double-count regression: a virtual model whose first candidate is
// unreachable and second candidate succeeds must produce exactly ONE
// ProxyConn (registered once by handleProxy, not once per candidate), with
// attempts == 2, the final target, and bytesOut matching only the successful
// candidate's body.
func TestProxyMetrics_VirtualFailoverSingleEntry(t *testing.T) {
	const body = `{"ok":true,"padding":"0123456789"}`
	// primary is declared first and believed online (seeded, not probed), but
	// its /v1/chat/completions hijacks and closes the connection before
	// writing anything — a pre-response failure indistinguishable from a
	// dial error, forcing routeVirtual's retry path rather than
	// candidatesForVirtual's reachability-preferred ordering.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			return
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
	}))
	defer primary.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "live-model"}}})
		case "/v1/chat/completions":
			w.Write([]byte(body))
		}
	}))
	defer live.Close()

	primaryEP := config.OpenAIEndpoint{Name: "primary", BaseURL: primary.URL + "/v1"}
	liveEP := config.OpenAIEndpoint{Name: "live", BaseURL: live.URL + "/v1"}
	registry := NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{primaryEP, liveEP}})
	seedEndpointStatus(registry, primaryEP, true, UpstreamModel{ID: "primary-model"})
	seedEndpointStatus(registry, liveEP, true, UpstreamModel{ID: "live-model"})

	router := NewRelayRouter(":0", nil, registry, &config.VirtualLLMConfig{Models: []config.VirtualLLM{{
		Name: "vFail",
		Targets: []config.VirtualLLMTarget{
			{Endpoint: "primary", Model: "primary-model"},
			{Endpoint: "live", Model: "live-model"},
		},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"vFail"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, b)
	}
	got, _ := io.ReadAll(resp.Body)

	_, _, recent := router.Metrics().Snapshot()
	if len(recent) != 1 {
		t.Fatalf("recentRequests = %+v, want exactly 1 entry (no double-count across candidates)", recent)
	}
	row := recent[0]
	if row.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (one failed, one succeeded)", row.Attempts)
	}
	if row.TargetKind != "virtual" {
		t.Errorf("TargetKind = %q, want virtual", row.TargetKind)
	}
	if row.BytesOut != int64(len(got)) {
		t.Errorf("BytesOut = %d, want %d (exactly the live candidate's body, nothing from the dead one)", row.BytesOut, len(got))
	}
}

// A mid-stream backend abort (the failure http.ErrAbortHandler recovers from
// one frame up in net/http) must not leave a ghost entry in `active` —
// exactly what handleProxy's `defer p.metrics.end(conn)` exists to guarantee.
func TestProxyMetrics_MidStreamAbortDeregisters(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: chunk1\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Abort mid-stream: hijack and close the connection, simulating a
		// backend that dies after headers are already out.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
	}))
	defer upstream.Close()

	registry := NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{
		{Name: "flaky", BaseURL: upstream.URL + "/v1"},
	}})
	// Warm the endpoint so LookupModel resolves it directly (its /v1/models
	// probe would otherwise 404 against this handler and read as offline).
	seedEndpointStatus(registry, config.OpenAIEndpoint{Name: "flaky", BaseURL: upstream.URL + "/v1"}, true, UpstreamModel{ID: "m"})

	router := NewRelayRouter(":0", nil, registry, nil)
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"flaky/m","stream":true}`))
	defer resp.Body.Close()
	io.ReadAll(resp.Body) // drain; a read error here is expected, the connection broke mid-body

	active, _, recent := router.Metrics().Snapshot()
	if len(active) != 0 {
		t.Errorf("active after mid-stream abort = %+v, want empty (ghost-entry regression)", active)
	}
	if len(recent) != 1 {
		t.Fatalf("recentRequests = %+v, want exactly 1 entry", recent)
	}
}

// An unknown model still must appear in recentRequests with its 400 status —
// the connection is registered before dispatch even resolves the model.
func TestProxyMetrics_UnknownModelStillRecorded(t *testing.T) {
	r := NewRelayRouter(":0", nil, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"does-not-exist"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	io.ReadAll(resp.Body)

	_, _, recent := r.Metrics().Snapshot()
	if len(recent) != 1 {
		t.Fatalf("recentRequests = %+v, want exactly 1 entry", recent)
	}
	if recent[0].Status != http.StatusBadRequest {
		t.Errorf("recent[0].Status = %d, want 400", recent[0].Status)
	}
	if recent[0].TargetKind != "unknown" {
		t.Errorf("recent[0].TargetKind = %q, want unknown", recent[0].TargetKind)
	}
}
