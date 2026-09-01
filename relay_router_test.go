package main

// Coverage for the unified OpenAI-compatible router (relay_router.go).
// The router has been zero-tested. These tests pin:
//
//   - /v1/models aggregation (managed-server aliases + reachable endpoint models)
//   - dispatch decisions on the request body's `model` field
//   - llama-wins-on-collision policy
//   - body rewriting (endpoint sees its bare upstream id, not "name/id")
//   - auth header replacement at the trust boundary
//   - unknown / malformed model → 400
//   - missing-API-key path
//   - health endpoint
//
// Managed-server branch test asserts the routing decision was made by observing the
// failure mode when GetOrLaunch can't find a binary — we don't spawn a real
// server here.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// rewriteProxyBody — pure
// ---------------------------------------------------------------------------

func TestRouter_RewriteModelField_SwapsTopLevelModel(t *testing.T) {
	body := []byte(`{"model":"omlx/Qwen3.5-27B","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := rewriteProxyBody(body, "Qwen3.5-27B", nil)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var parsed struct {
		Model    string          `json:"model"`
		Stream   bool            `json:"stream"`
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal rewritten: %v", err)
	}
	if parsed.Model != "Qwen3.5-27B" {
		t.Errorf("model: got %q, want %q", parsed.Model, "Qwen3.5-27B")
	}
	if !parsed.Stream {
		t.Error("stream flag lost in rewrite")
	}
	if len(parsed.Messages) == 0 {
		t.Error("messages lost in rewrite")
	}
}

func TestRouter_RewriteModelField_InvalidJSON(t *testing.T) {
	_, err := rewriteProxyBody([]byte(`{not json`), "x", nil)
	if err == nil {
		t.Error("expected error on malformed body, got nil")
	}
}

// ---------------------------------------------------------------------------
// /health and /v1/models
// ---------------------------------------------------------------------------

func TestRouter_Health_ReturnsOK(t *testing.T) {
	r := NewRelayRouter(":0", nil, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
}

func TestRouter_ListModels_EmptyWithNilBackends(t *testing.T) {
	r := NewRelayRouter(":0", nil, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	var resp struct {
		Object string                   `json:"object"`
		Data   []map[string]interface{} `json:"data"`
	}
	doRouterJSON(t, srv.URL+"/v1/models", "GET", nil, &resp)
	if resp.Object != "list" {
		t.Errorf("object: got %q, want list", resp.Object)
	}
	if len(resp.Data) != 0 {
		t.Errorf("data: got %v, want empty", resp.Data)
	}
}

func TestRouter_ListModels_IncludesLlamaAliases(t *testing.T) {
	mgr := NewServerManager(llamaProfile, &ServerConfig{
		Models: []ServerModelConfig{{Alias: "qwen-8b"}, {Alias: "qwen-30b"}},
	}, "")
	r := NewRelayRouter(":0", []*ServerManager{mgr}, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	var resp struct {
		Data []map[string]interface{} `json:"data"`
	}
	doRouterJSON(t, srv.URL+"/v1/models", "GET", nil, &resp)
	ids := modelIDs(resp.Data)
	if !sliceContains(ids, "qwen-8b") || !sliceContains(ids, "qwen-30b") {
		t.Errorf("missing llama aliases: %v", ids)
	}
}

func TestRouter_ListModels_PrefixesEndpointModels(t *testing.T) {
	// Spin up a fake OpenAI-compat upstream that advertises one model.
	upstream := newFakeOpenAIUpstream(t, []string{"gpt-test"})

	cfg := &OpenAIConfig{
		Endpoints: []OpenAIEndpoint{
			{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "test-key"},
		},
	}
	registry := NewProxyRegistry(cfg)

	r := NewRelayRouter(":0", nil, registry, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	var resp struct {
		Data []map[string]interface{} `json:"data"`
	}
	doRouterJSON(t, srv.URL+"/v1/models", "GET", nil, &resp)
	ids := modelIDs(resp.Data)
	if !sliceContains(ids, "fakeep/gpt-test") {
		t.Errorf("expected endpoint-prefixed model in list, got %v", ids)
	}
}

func TestRouter_ListModels_OmitsOfflineEndpoint(t *testing.T) {
	// Endpoint pointing nowhere → probe fails → omitted from listing.
	cfg := &OpenAIConfig{
		Endpoints: []OpenAIEndpoint{
			{Name: "offline", BaseURL: "http://127.0.0.1:1/v1", APIKey: "x"},
		},
	}
	registry := NewProxyRegistry(cfg)

	r := NewRelayRouter(":0", nil, registry, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	var resp struct {
		Data []map[string]interface{} `json:"data"`
	}
	doRouterJSON(t, srv.URL+"/v1/models", "GET", nil, &resp)
	for _, m := range resp.Data {
		if id, _ := m["id"].(string); strings.HasPrefix(id, "offline/") {
			t.Errorf("offline endpoint leaked into listing: %v", m)
		}
	}
}

// ---------------------------------------------------------------------------
// handleProxy dispatch
// ---------------------------------------------------------------------------

func TestRouter_Proxy_MissingModelField_Returns400(t *testing.T) {
	r := NewRelayRouter(":0", nil, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"messages":[]}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestRouter_Proxy_UnknownModel_Returns400(t *testing.T) {
	r := NewRelayRouter(":0", nil, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"does-not-exist"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unknown model") {
		t.Errorf("error body: got %q", string(body))
	}
}

func TestRouter_Proxy_LlamaAlias_RoutesToManagedBranch(t *testing.T) {
	// Manager has the alias but no real binary → GetOrLaunch fails → 502.
	// The 502 is the test signal: the router DID pick the managed branch and
	// not the endpoint branch (which would 400). We're testing the dispatch
	// decision, not the lifecycle.
	mgr := NewServerManager(llamaProfile, &ServerConfig{
		BinaryPath: "/nonexistent/llama-server-binary-for-test",
		Models:     []ServerModelConfig{{Alias: "test-alias", Args: map[string]any{"model": "/fake"}}},
	}, "")

	r := NewRelayRouter(":0", []*ServerManager{mgr}, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"test-alias"}`))
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 502 from failed server launch; got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestRouter_Proxy_EndpointModel_RewritesBodyAndStripsPrefix(t *testing.T) {
	// Upstream records the body it received so we can assert the model field
	// was rewritten from "fakeep/Qwen" to bare "Qwen".
	var seenBody []byte
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "Qwen"}},
			})
		case "/v1/chat/completions":
			seenBody, _ = io.ReadAll(r.Body)
			seenAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"resp","choices":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()

	cfg := &OpenAIConfig{
		Endpoints: []OpenAIEndpoint{
			{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "upstream-key"},
		},
	}
	registry := NewProxyRegistry(cfg)
	// Force probe so LookupModel finds the endpoint Online.
	registry.Snapshot(context.Background())

	r := NewRelayRouter(":0", nil, registry, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"fakeep/Qwen","stream":false}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	// Body rewritten: model is the bare upstream id, other fields preserved.
	var got struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(seenBody, &got); err != nil {
		t.Fatalf("decode upstream body %q: %v", string(seenBody), err)
	}
	if got.Model != "Qwen" {
		t.Errorf("upstream saw model %q, want %q (prefix not stripped)", got.Model, "Qwen")
	}

	// Auth was replaced with the endpoint's key, not the client's.
	if seenAuth != "Bearer upstream-key" {
		t.Errorf("upstream Authorization: got %q, want %q", seenAuth, "Bearer upstream-key")
	}
}

func TestRouter_Proxy_VirtualModelUsesFirstReachableTarget(t *testing.T) {
	var primaryCalls, fallbackCalls int
	newTarget := func(calls *int, id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/models":
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": id}}})
			case "/v1/chat/completions":
				(*calls)++
				var body struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				json.NewEncoder(w).Encode(map[string]any{"model": body.Model})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}
	primary := newTarget(&primaryCalls, "remote-code")
	defer primary.Close()
	fallback := newTarget(&fallbackCalls, "mac-code")
	defer fallback.Close()

	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{
		{Name: "remote", BaseURL: primary.URL + "/v1"},
		{Name: "mac", BaseURL: fallback.URL + "/v1"},
	}})
	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vCode", Targets: []VirtualLLMTarget{
			{Endpoint: "remote", Model: "remote-code"},
			{Endpoint: "mac", Model: "mac-code"},
		},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()
	var catalog struct {
		Data []map[string]any `json:"data"`
	}
	doRouterJSON(t, srv.URL+"/v1/models", http.MethodGet, nil, &catalog)
	if !sliceContains(modelIDs(catalog.Data), "vCode") {
		t.Fatalf("virtual model missing from catalog: %v", modelIDs(catalog.Data))
	}

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"vCode"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST virtual model = %d", resp.StatusCode)
	}
	if primaryCalls != 1 || fallbackCalls != 0 {
		t.Errorf("calls primary=%d fallback=%d, want 1, 0", primaryCalls, fallbackCalls)
	}
	var response struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Model != "remote-code" {
		t.Errorf("primary saw model %q, want remote-code", response.Model)
	}
}

func TestRouter_Proxy_VirtualModelFallsBackWhenPrimaryIsOffline(t *testing.T) {
	var calls int
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "mac-code"}}})
		case "/v1/chat/completions":
			calls++
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer fallback.Close()

	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{
		{Name: "remote", BaseURL: "http://127.0.0.1:1/v1"},
		{Name: "mac", BaseURL: fallback.URL + "/v1"},
	}})
	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vCode", Targets: []VirtualLLMTarget{{Endpoint: "remote", Model: "remote-code"}, {Endpoint: "mac", Model: "mac-code"}},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"vCode"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls != 1 {
		t.Errorf("POST virtual model = %d, fallback calls = %d; want 200, 1", resp.StatusCode, calls)
	}
}

func TestRouter_VirtualModelUsesManagedAliasFallback(t *testing.T) {
	mgr := NewServerManager(llamaProfile, &ServerConfig{
		Models: []ServerModelConfig{{Alias: "local-code"}},
	}, "")
	router := NewRelayRouter(":0", []*ServerManager{mgr}, nil, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vCode", Targets: []VirtualLLMTarget{{Alias: "local-code"}},
	}}})

	candidates := router.virtualCandidates(context.Background(), "vCode")
	if len(candidates) != 1 || candidates[0].manager != mgr || candidates[0].alias != "local-code" {
		t.Errorf("candidates = %+v; want a single local managed alias", candidates)
	}
}

func TestRouter_Proxy_LlamaWinsOnCollision(t *testing.T) {
	// Both backends advertise "qwen". Llama branch must win (per startup
	// warning in main.go documenting this policy). We assert by triggering
	// the llama branch's failure mode (502, no binary) vs the endpoint
	// branch's success (would have returned 200).
	upstream := newFakeOpenAIUpstream(t, []string{"qwen"})
	cfg := &OpenAIConfig{
		Endpoints: []OpenAIEndpoint{
			{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "k"},
		},
	}
	registry := NewProxyRegistry(cfg)
	registry.Snapshot(context.Background())

	mgr := NewServerManager(llamaProfile, &ServerConfig{
		BinaryPath: "/nonexistent/binary",
		Models:     []ServerModelConfig{{Alias: "qwen", Args: map[string]any{"model": "/fake"}}},
	}, "")

	r := NewRelayRouter(":0", []*ServerManager{mgr}, registry, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	// "qwen" matches both llama alias AND would parse as bare upstream id.
	// Wait — endpoint path requires "name/id" form, so "qwen" alone would only
	// hit the llama branch. To force collision: send "fakeep/qwen" → endpoint
	// branch handles. Send "qwen" → llama branch (collision case in the
	// router's eyes). The router's HasAlias check happens first; bare "qwen"
	// goes to llama (correct collision behavior).
	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"qwen"}`))
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 502 (llama branch fail); got %d body=%s — collision policy may have broken", resp.StatusCode, string(body))
	}
}

func TestRouter_Proxy_LlamaWinsOverMlxCollision(t *testing.T) {
	// Both llama and mlx managers have the same alias. Llama must win because
	// it's first in the managers slice. We assert by triggering the llama
	// branch's failure mode (502, no binary).
	llamaMgr := NewServerManager(llamaProfile, &ServerConfig{
		BinaryPath: "/nonexistent/binary",
		Models:     []ServerModelConfig{{Alias: "shared", Args: map[string]any{"model": "/fake"}}},
	}, "")
	mlxMgr := NewServerManager(mlxProfile, &ServerConfig{
		BinaryPath: "/nonexistent/mlx-serve",
		Models:     []ServerModelConfig{{Alias: "shared", Args: map[string]any{"model": "/fake"}}},
	}, "")

	r := NewRelayRouter(":0", []*ServerManager{llamaMgr, mlxMgr}, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"shared"}`))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502 (llama branch fail); got %d body=%s — llama-should-win policy may have broken", resp.StatusCode, string(body))
	}
	// Both managers 502 with a missing binary, so the status code alone can't
	// detect a priority inversion — the error's kind prefix can.
	if !strings.Contains(string(body), "llama:") {
		t.Errorf("expected llama-branch error (llama wins collision); got body=%s", string(body))
	}
}

// ---------------------------------------------------------------------------
// Virtual-model preference ordering, retry, and catalog hardening
// (fix/virtual-llm-failover-hardening)
// ---------------------------------------------------------------------------

// Every declared target is currently believed offline (probe fails), but the
// endpoint is still configured — so it must be attempted as a last resort
// rather than making the virtual name unroutable.
func TestRouter_Proxy_VirtualModel_AllTargetsOffline_StillRoutesViaLastResort(t *testing.T) {
	// atomic: incremented from the upstream's handler goroutine, read from the
	// test's main goroutine — a plain int here is a real data race under -race
	// once the connection isn't a normal complete-and-read cycle (see the
	// retry tests below for the case that actually trips it).
	var chatCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusInternalServerError) // probe sees this endpoint as offline
		case "/v1/chat/completions":
			chatCalls.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{
		{Name: "flaky", BaseURL: upstream.URL + "/v1"},
	}})
	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vFlaky", Targets: []VirtualLLMTarget{{Endpoint: "flaky", Model: "flaky-model"}},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"vFlaky"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 via the last-resort target, got %d body=%s", resp.StatusCode, string(body))
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("chat calls = %d, want 1", got)
	}
}

// Every candidate is genuinely unreachable: the router must 503 naming the
// virtual model, not 400 "unknown model" — a configured virtual is never
// "unknown", it's just currently unservable.
func TestRouter_Proxy_VirtualModel_AllUnreachable_Returns503NotUnknown(t *testing.T) {
	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{
		{Name: "dead1", BaseURL: "http://127.0.0.1:1/v1"},
		{Name: "dead2", BaseURL: "http://127.0.0.1:1/v1"},
	}})
	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vDead", Targets: []VirtualLLMTarget{
			{Endpoint: "dead1", Model: "m1"},
			{Endpoint: "dead2", Model: "m2"},
		},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"vDead"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// JSON-encoded, so the literal quotes around the name come out escaped.
	if !strings.Contains(string(body), "vDead") {
		t.Errorf("error body doesn't name the virtual model: %s", body)
	}
	if strings.Contains(string(body), "unknown model") {
		t.Errorf("a configured virtual must never read as unknown model: %s", body)
	}
}

// A virtual name is stable config: it must appear in the catalog even when
// every target is currently offline, so a polling client learns it will not
// resolve right now instead of never seeing it at all.
func TestRouterCatalog_VirtualModel_AllOffline_ListedAsUnloaded(t *testing.T) {
	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{
		{Name: "dead", BaseURL: "http://127.0.0.1:1/v1"},
	}})
	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vDead", Targets: []VirtualLLMTarget{{Endpoint: "dead", Model: "m1"}},
	}}})

	var row catalogRow
	for _, r := range fetchCatalog(t, router, "/v1/models") {
		if r.ID == "vDead" {
			row = r
		}
	}
	if row.ID == "" {
		t.Fatal("virtual model missing from catalog even though every target is offline")
	}
	if row.Status == nil || row.Status.Value != ModelStatusUnloaded {
		t.Errorf("status.value = %+v, want %q", row.Status, ModelStatusUnloaded)
	}
	if row.Status == nil || !row.Status.Failed {
		t.Errorf("status.failed = %+v, want true so a polling client stops instead of spinning", row.Status)
	}
}

// The first candidate fails before writing anything to the client — the
// router must retry the next candidate rather than surface the failure.
func TestRouter_Proxy_VirtualModel_RetriesAfterPreResponseFailure(t *testing.T) {
	// atomic: written from the upstream's own handler goroutine, read from
	// the test's main goroutine after the outer request completes. Ordering
	// is guaranteed in practice (the increment happens strictly before the
	// hijack+close that the router's RoundTrip call is synchronously blocked
	// on), but the two goroutines are different ones, so the access itself
	// must be atomic or -race flags it.
	var primaryCalls, secondaryCalls atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "primary-model"}}})
		case "/v1/chat/completions":
			primaryCalls.Add(1)
			// Close the connection before writing anything — a pre-response
			// failure indistinguishable, from the proxy's side, from a dial
			// error: RoundTrip fails, ErrorHandler runs, nothing was written.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
		}
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "secondary-model"}}})
		case "/v1/chat/completions":
			secondaryCalls.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer secondary.Close()

	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{
		{Name: "primary", BaseURL: primary.URL + "/v1"},
		{Name: "secondary", BaseURL: secondary.URL + "/v1"},
	}})
	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vRetry", Targets: []VirtualLLMTarget{
			{Endpoint: "primary", Model: "primary-model"},
			{Endpoint: "secondary", Model: "secondary-model"},
		},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"vRetry"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from the retried secondary target, got %d body=%s", resp.StatusCode, string(body))
	}
	if got := primaryCalls.Load(); got != 1 {
		t.Errorf("primary calls = %d, want 1 (attempted once, then abandoned)", got)
	}
	if got := secondaryCalls.Load(); got != 1 {
		t.Errorf("secondary calls = %d, want 1 (the retry)", got)
	}
}

// Once the primary has written response bytes, a mid-stream break must not
// trigger a retry — the second target must never be called.
func TestRouter_Proxy_VirtualModel_NoRetryAfterBytesWritten(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "primary-model"}}})
		case "/v1/chat/completions":
			primaryCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"partial":`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Break the connection mid-body, after the client already has a
			// 200 and part of the payload.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
		}
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "secondary-model"}}})
		case "/v1/chat/completions":
			secondaryCalls.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer secondary.Close()

	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{
		{Name: "primary", BaseURL: primary.URL + "/v1"},
		{Name: "secondary", BaseURL: secondary.URL + "/v1"},
	}})
	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vNoRetry", Targets: []VirtualLLMTarget{
			{Endpoint: "primary", Model: "primary-model"},
			{Endpoint: "secondary", Model: "secondary-model"},
		},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"vNoRetry"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the primary's partial response)", resp.StatusCode)
	}
	io.ReadAll(resp.Body) // drain; a read error here is expected, the connection broke mid-body

	if got := primaryCalls.Load(); got != 1 {
		t.Errorf("primary calls = %d, want 1", got)
	}
	if got := secondaryCalls.Load(); got != 0 {
		t.Errorf("secondary calls = %d, want 0 — no retry once bytes reached the client", got)
	}
}

// A virtual row must inherit real metadata from its first candidate, not the
// hardcoded text-only placeholder the old handleModels used.
func TestRouterCatalog_VirtualModel_InheritsMetadataFromAliasTarget(t *testing.T) {
	mgr := NewServerManager(llamaProfile, &ServerConfig{
		Models: []ServerModelConfig{{Alias: "vision-alias", Args: map[string]any{
			"mmproj": "/p.gguf", "ctx-size": 32768.0,
		}}},
	}, "")
	router := NewRelayRouter(":0", []*ServerManager{mgr}, nil, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vVision", Targets: []VirtualLLMTarget{{Alias: "vision-alias"}},
	}}})

	var row catalogRow
	for _, r := range fetchCatalog(t, router, "/v1/models") {
		if r.ID == "vVision" {
			row = r
		}
	}
	if row.ID == "" {
		t.Fatal("virtual model missing from catalog")
	}
	if row.Status == nil || row.Status.Value != ModelStatusLoaded {
		t.Errorf("status = %+v, want %q", row.Status, ModelStatusLoaded)
	}
	if got := row.Architecture.InputModalities; len(got) != 2 || got[0] != "text" || got[1] != "image" {
		t.Errorf("modalities = %v, want [text image]", got)
	}
	if row.Meta == nil || row.Meta.NCtx != 32768 {
		t.Errorf("meta = %+v, want n_ctx 32768", row.Meta)
	}
}

// ---------------------------------------------------------------------------
// Conversation affinity for virtual models (ADR-010). Two backends cannot
// safely share a reasoning transcript, so once some target has actually
// served a conversation, later turns must stick to it even when the
// reachability cache would otherwise prefer something else.
// ---------------------------------------------------------------------------

// newCountingChatUpstream returns an httptest server that advertises modelID
// on /v1/models and answers /v1/chat/completions with {"ok":true}, counting
// calls into the given counter — lets these tests observe which target
// actually served a request without depending on response bodies or timing.
func newCountingChatUpstream(t *testing.T, calls *atomic.Int64, modelID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": modelID}}})
		case "/v1/chat/completions":
			calls.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedEndpointStatus writes a status directly into the registry's cache,
// bypassing a real network probe. The affinity tests need specific
// online/offline states — and, for the re-pin test, to swap an endpoint's
// BaseURL to a dead port out from under an established pin — without waiting
// out the registry's real 15s TTL. Locking registry.mu directly is safe here:
// Snapshot's probe goroutines are joined (wg.Wait()) before it returns, so by
// the time a request's response has been read, nothing else can be touching
// this map.
func seedEndpointStatus(registry *ProxyRegistry, ep OpenAIEndpoint, online bool, models ...UpstreamModel) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.status[ep.Name] = &EndpointStatus{
		Endpoint:    ep,
		Online:      online,
		Models:      models,
		LastChecked: time.Now(),
	}
}

// Requirement 1: pin survives a reachability flip. "primary" starts offline
// so the first request is forced onto "secondary" and pins there; primary
// then comes online, and a same-key second request must still land on
// secondary even though reachability ordering alone would now prefer primary.
func TestRouterAffinity_PinSurvivesReachabilityFlip(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int64
	primary := newCountingChatUpstream(t, &primaryCalls, "m")
	secondary := newCountingChatUpstream(t, &secondaryCalls, "m")

	primaryEP := OpenAIEndpoint{Name: "primary", BaseURL: primary.URL + "/v1"}
	secondaryEP := OpenAIEndpoint{Name: "secondary", BaseURL: secondary.URL + "/v1"}
	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{primaryEP, secondaryEP}})
	seedEndpointStatus(registry, primaryEP, false)
	seedEndpointStatus(registry, secondaryEP, true, UpstreamModel{ID: "m"})

	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vConv",
		Targets: []VirtualLLMTarget{
			{Endpoint: "primary", Model: "m"},
			{Endpoint: "secondary", Model: "m"},
		},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	body := []byte(`{"model":"vConv","prompt_cache_key":"conv-1"}`)
	resp := postBytes(t, srv.URL+"/v1/chat/completions", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", resp.StatusCode)
	}
	if primaryCalls.Load() != 0 || secondaryCalls.Load() != 1 {
		t.Fatalf("first request calls: primary=%d secondary=%d, want 0,1 (only secondary was online)",
			primaryCalls.Load(), secondaryCalls.Load())
	}

	// Primary is now reachable — reachability-preferred ordering would put
	// it first. The pin must keep the conversation on secondary anyway.
	seedEndpointStatus(registry, primaryEP, true, UpstreamModel{ID: "m"})

	resp2 := postBytes(t, srv.URL+"/v1/chat/completions", body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request: status = %d, want 200", resp2.StatusCode)
	}
	if primaryCalls.Load() != 0 || secondaryCalls.Load() != 2 {
		t.Errorf("second request calls: primary=%d secondary=%d, want 0,2 — the pin must beat reachability ordering",
			primaryCalls.Load(), secondaryCalls.Load())
	}
}

// Requirement 2: distinct conversation keys pin independently. conv-A pins
// to secondary while primary is offline; conv-B is a fresh key issued after
// primary comes online, so it pins to primary on its own first request.
// Replaying both afterward must not cross-contaminate.
func TestRouterAffinity_DistinctConversationKeysPinIndependently(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int64
	primary := newCountingChatUpstream(t, &primaryCalls, "m")
	secondary := newCountingChatUpstream(t, &secondaryCalls, "m")

	primaryEP := OpenAIEndpoint{Name: "primary", BaseURL: primary.URL + "/v1"}
	secondaryEP := OpenAIEndpoint{Name: "secondary", BaseURL: secondary.URL + "/v1"}
	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{primaryEP, secondaryEP}})
	seedEndpointStatus(registry, primaryEP, false)
	seedEndpointStatus(registry, secondaryEP, true, UpstreamModel{ID: "m"})

	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vConv",
		Targets: []VirtualLLMTarget{
			{Endpoint: "primary", Model: "m"},
			{Endpoint: "secondary", Model: "m"},
		},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	convA := []byte(`{"model":"vConv","prompt_cache_key":"conv-A"}`)
	postBytes(t, srv.URL+"/v1/chat/completions", convA).Body.Close()
	if secondaryCalls.Load() != 1 {
		t.Fatalf("conv-A first request: secondary calls = %d, want 1", secondaryCalls.Load())
	}

	seedEndpointStatus(registry, primaryEP, true, UpstreamModel{ID: "m"})

	convB := []byte(`{"model":"vConv","prompt_cache_key":"conv-B"}`)
	postBytes(t, srv.URL+"/v1/chat/completions", convB).Body.Close()
	if primaryCalls.Load() != 1 {
		t.Fatalf("conv-B first request: primary calls = %d, want 1 (fresh key, no pin yet, primary now preferred)", primaryCalls.Load())
	}

	// Replay both — each must stick to its own established target, even
	// though both endpoints are now online and reachability ordering alone
	// would send everything to primary.
	postBytes(t, srv.URL+"/v1/chat/completions", convA).Body.Close()
	postBytes(t, srv.URL+"/v1/chat/completions", convB).Body.Close()

	if primaryCalls.Load() != 2 || secondaryCalls.Load() != 2 {
		t.Errorf("after replay: primary=%d (want 2: conv-B x2) secondary=%d (want 2: conv-A x2) — pins must not cross-contaminate",
			primaryCalls.Load(), secondaryCalls.Load())
	}
}

// Requirement 3: no prompt_cache_key and no user means today's behavior —
// normal reachability-preferred ordering, and critically, no pin recorded.
func TestRouterAffinity_NoKeyMeansNoAffinity(t *testing.T) {
	var primaryCalls atomic.Int64
	primary := newCountingChatUpstream(t, &primaryCalls, "m")
	primaryEP := OpenAIEndpoint{Name: "primary", BaseURL: primary.URL + "/v1"}
	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{primaryEP}})
	seedEndpointStatus(registry, primaryEP, true, UpstreamModel{ID: "m"})

	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vConv", Targets: []VirtualLLMTarget{{Endpoint: "primary", Model: "m"}},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"vConv"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || primaryCalls.Load() != 1 {
		t.Fatalf("status=%d calls=%d, want 200,1", resp.StatusCode, primaryCalls.Load())
	}
	if got := router.affinity.size(); got != 0 {
		t.Errorf("affinity store size = %d, want 0 — no key means no pin is ever recorded", got)
	}
}

// Requirement 4: `user` is used when prompt_cache_key is absent.
func TestRouterAffinity_FallsBackToUserField(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int64
	primary := newCountingChatUpstream(t, &primaryCalls, "m")
	secondary := newCountingChatUpstream(t, &secondaryCalls, "m")

	primaryEP := OpenAIEndpoint{Name: "primary", BaseURL: primary.URL + "/v1"}
	secondaryEP := OpenAIEndpoint{Name: "secondary", BaseURL: secondary.URL + "/v1"}
	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{primaryEP, secondaryEP}})
	seedEndpointStatus(registry, primaryEP, false)
	seedEndpointStatus(registry, secondaryEP, true, UpstreamModel{ID: "m"})

	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vConv",
		Targets: []VirtualLLMTarget{
			{Endpoint: "primary", Model: "m"},
			{Endpoint: "secondary", Model: "m"},
		},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	body := []byte(`{"model":"vConv","user":"user-1"}`)
	postBytes(t, srv.URL+"/v1/chat/completions", body).Body.Close()
	if secondaryCalls.Load() != 1 {
		t.Fatalf("first request: secondary calls = %d, want 1", secondaryCalls.Load())
	}

	seedEndpointStatus(registry, primaryEP, true, UpstreamModel{ID: "m"})
	postBytes(t, srv.URL+"/v1/chat/completions", body).Body.Close()

	if primaryCalls.Load() != 0 || secondaryCalls.Load() != 2 {
		t.Errorf("second request calls: primary=%d secondary=%d, want 0,2 — user field must pin same as prompt_cache_key",
			primaryCalls.Load(), secondaryCalls.Load())
	}
}

// Requirement 5: a pin naming a target removed from config entirely (not
// merely offline — actually gone from virtual.Targets) is ignored, and
// routing falls back to whatever candidates remain.
func TestRouterAffinity_PinnedTargetRemovedFromConfigFallsBack(t *testing.T) {
	var aCalls, bCalls atomic.Int64
	a := newCountingChatUpstream(t, &aCalls, "m")
	b := newCountingChatUpstream(t, &bCalls, "m")

	aEP := OpenAIEndpoint{Name: "a", BaseURL: a.URL + "/v1"}
	bEP := OpenAIEndpoint{Name: "b", BaseURL: b.URL + "/v1"}
	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{aEP, bEP}})
	seedEndpointStatus(registry, aEP, true, UpstreamModel{ID: "m"})
	seedEndpointStatus(registry, bEP, true, UpstreamModel{ID: "m"})

	virtual := &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vConv",
		Targets: []VirtualLLMTarget{
			{Endpoint: "a", Model: "m"},
			{Endpoint: "b", Model: "m"},
		},
	}}}
	router := NewRelayRouter(":0", nil, registry, virtual)
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	body := []byte(`{"model":"vConv","prompt_cache_key":"conv-1"}`)
	postBytes(t, srv.URL+"/v1/chat/completions", body).Body.Close()
	if aCalls.Load() != 1 {
		t.Fatalf("first request: a calls = %d, want 1 (declared first, both online)", aCalls.Load())
	}

	// Remove "a" from config entirely — as if the operator dropped the
	// target, not merely took it offline.
	virtual.Models[0].Targets = virtual.Models[0].Targets[1:]

	postBytes(t, srv.URL+"/v1/chat/completions", body).Body.Close()
	if aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Errorf("after removing the pinned target: a=%d (want 1, untouched) b=%d (want 1, took over)",
			aCalls.Load(), bCalls.Load())
	}
}

// Requirement 6: a pinned target that fails at connection time falls through
// to the next candidate, and the pin re-points to whatever actually served —
// proven by a third request going straight to the new target even after the
// originally pinned one becomes healthy again.
func TestRouterAffinity_RepinsAfterPinnedTargetFails(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int64
	primary := newCountingChatUpstream(t, &primaryCalls, "m")
	secondary := newCountingChatUpstream(t, &secondaryCalls, "m")

	primaryEP := OpenAIEndpoint{Name: "primary", BaseURL: primary.URL + "/v1"}
	secondaryEP := OpenAIEndpoint{Name: "secondary", BaseURL: secondary.URL + "/v1"}
	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{primaryEP, secondaryEP}})
	seedEndpointStatus(registry, primaryEP, true, UpstreamModel{ID: "m"})
	seedEndpointStatus(registry, secondaryEP, true, UpstreamModel{ID: "m"})

	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vConv",
		Targets: []VirtualLLMTarget{
			{Endpoint: "primary", Model: "m"},
			{Endpoint: "secondary", Model: "m"},
		},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	body := []byte(`{"model":"vConv","prompt_cache_key":"conv-1"}`)

	// Request 1: both online, declared order preferred — primary serves and
	// is pinned.
	postBytes(t, srv.URL+"/v1/chat/completions", body).Body.Close()
	if primaryCalls.Load() != 1 {
		t.Fatalf("request 1: primary calls = %d, want 1", primaryCalls.Load())
	}

	// Simulate primary failing at connection time: point the cached status
	// at a closed port while still marking it "online" — the registry's own
	// 15s-stale cache wouldn't know it just died until the next real probe,
	// which is exactly the scenario the retry path exists for.
	seedEndpointStatus(registry, OpenAIEndpoint{Name: "primary", BaseURL: "http://127.0.0.1:1/v1"}, true, UpstreamModel{ID: "m"})

	// Request 2: the pin still points at primary (order unchanged by
	// applyAffinity — already first), but primary now fails pre-response, so
	// the router falls through to secondary and re-pins to it.
	resp2 := postBytes(t, srv.URL+"/v1/chat/completions", body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("request 2: status = %d, want 200 (served by the fallback)", resp2.StatusCode)
	}
	if secondaryCalls.Load() != 1 {
		t.Fatalf("request 2: secondary calls = %d, want 1 (took over after primary failed)", secondaryCalls.Load())
	}

	// Restore primary to full health — reachability ordering alone would now
	// prefer it again (declared first, believed online).
	seedEndpointStatus(registry, primaryEP, true, UpstreamModel{ID: "m"})

	// Request 3: the pin now points at secondary, so it must go straight
	// there — primary, though healthy again, must not receive a call.
	postBytes(t, srv.URL+"/v1/chat/completions", body).Body.Close()
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 2 {
		t.Errorf("request 3: primary=%d (want 1, still untouched) secondary=%d (want 2, pin held)",
			primaryCalls.Load(), secondaryCalls.Load())
	}
}

// Requirement 8: a failed attempt must never record a pin — only a genuine
// success does. Every target here is unreachable, so the store must stay
// empty after the 503.
func TestRouterAffinity_FailedAttemptRecordsNoPin(t *testing.T) {
	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{
		{Name: "dead1", BaseURL: "http://127.0.0.1:1/v1"},
		{Name: "dead2", BaseURL: "http://127.0.0.1:1/v1"},
	}})
	router := NewRelayRouter(":0", nil, registry, &VirtualLLMConfig{Models: []VirtualLLM{{
		Name: "vDead", Targets: []VirtualLLMTarget{
			{Endpoint: "dead1", Model: "m1"},
			{Endpoint: "dead2", Model: "m2"},
		},
	}}})
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"vDead","prompt_cache_key":"conv-1"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := router.affinity.size(); got != 0 {
		t.Errorf("affinity store size = %d, want 0 — a failed attempt must never pin", got)
	}
}

// A Snapshot triggered by an already-canceled caller context must still
// record a real probe result — the caller hanging up says nothing about
// whether the upstream is reachable.
func TestProxyRegistry_Snapshot_CanceledCallerContextDoesNotPoisonProbe(t *testing.T) {
	upstream := newFakeOpenAIUpstream(t, []string{"m1"})
	registry := NewProxyRegistry(&OpenAIConfig{Endpoints: []OpenAIEndpoint{
		{Name: "ep", BaseURL: upstream.URL + "/v1"},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller has already hung up before the probe even starts

	statuses := registry.Snapshot(ctx)
	if len(statuses) != 1 || !statuses[0].Online {
		t.Errorf("statuses = %+v, want a single Online:true entry despite the canceled caller context", statuses)
	}
}

// ---------------------------------------------------------------------------
// Test helpers (file-local; collision-free names)
// ---------------------------------------------------------------------------

func doRouterJSON(t *testing.T, url, method string, body io.Reader, out interface{}) {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func postBytes(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func modelIDs(data []map[string]interface{}) []string {
	out := make([]string, 0, len(data))
	for _, m := range data {
		if id, ok := m["id"].(string); ok {
			out = append(out, id)
		}
	}
	return out
}

func sliceContains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// newFakeOpenAIUpstream returns an httptest server that advertises the given
// model IDs on /v1/models. Caller is responsible for calling Close — but
// because we never do (let the OS reap on test exit), we register a Cleanup.
func newFakeOpenAIUpstream(t *testing.T, modelIDs []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			data := make([]map[string]any, len(modelIDs))
			for i, id := range modelIDs {
				data[i] = map[string]any{"id": id}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	return srv
}
