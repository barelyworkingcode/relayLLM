package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The router's catalog has to satisfy two audiences at once: plain
// OpenAI-compatible clients, and clients written against llama.cpp router
// mode — which reject the entire catalog unless every row carries a string
// status.value. These tests pin that contract.

type catalogRow struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	Status  *struct {
		Value  string `json:"value"`
		Failed bool   `json:"failed"`
		Error  string `json:"error"`
	} `json:"status"`
	Meta *struct {
		NCtx      int64 `json:"n_ctx"`
		NCtxTrain int64 `json:"n_ctx_train"`
	} `json:"meta"`
	Architecture *struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
}

func fetchCatalog(t *testing.T, router *RelayRouter, path string) []catalogRow {
	t.Helper()
	rec := httptest.NewRecorder()
	router.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, rec.Code)
	}
	var payload struct {
		Object string       `json:"object"`
		Data   []catalogRow `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if payload.Object != "list" {
		t.Errorf("object = %q, want list", payload.Object)
	}
	return payload.Data
}

// assertLlamaClientAccepts replicates the check in pi's llama.cpp client:
// every row must have a string id and a string status.value, or it throws
// "Server is not running in llama.cpp router mode" and drops the catalog.
func assertLlamaClientAccepts(t *testing.T, rows []catalogRow) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatal("empty catalog: a llama.cpp-compatible client would see no models")
	}
	for _, row := range rows {
		if row.ID == "" {
			t.Errorf("row has no id: %+v", row)
		}
		if row.Status == nil || row.Status.Value == "" {
			t.Errorf("row %q has no status.value; a llama.cpp client rejects the whole catalog on this", row.ID)
		}
	}
}

func newCatalogRouter(t *testing.T) (*RelayRouter, *ServerManager, *FakeClock) {
	t.Helper()
	cfg := &ServerConfig{}
	mgr, clk := newBudgetManager(t, cfg, nil)
	// Two models: one plain, one multimodal with a pinned context.
	cfg.Models = append(cfg.Models,
		ServerModelConfig{Alias: "plain", Args: map[string]any{"memoryGB": 4.0, "ctx-size": 32768.0}},
		ServerModelConfig{Alias: "vision", Args: map[string]any{"memoryGB": 4.0, "mmproj": "/p.gguf"}},
	)
	mgr.memory["plain"] = 4 * bytesPerGB
	mgr.memory["vision"] = 4 * bytesPerGB

	return NewRelayRouter("127.0.0.1:0", []*ServerManager{mgr}, nil), mgr, clk
}

func TestRouterCatalog_SatisfiesLlamaCppClientContract(t *testing.T) {
	router, _, _ := newCatalogRouter(t)

	for _, path := range []string{"/models", "/v1/models"} {
		t.Run(path, func(t *testing.T) {
			assertLlamaClientAccepts(t, fetchCatalog(t, router, path))
		})
	}
}

// A configured model reports "loaded" because our router launches on demand:
// the client can use it right now without asking for a load. Residency is
// reported through /api/status instead. Reporting residency here would empty a
// client's model picker every time the idle reaper ran.
func TestRouterCatalog_ConfiguredModelsAreUsable(t *testing.T) {
	router, _, _ := newCatalogRouter(t)

	for _, row := range fetchCatalog(t, router, "/models") {
		if row.Status.Value != ModelStatusLoaded {
			t.Errorf("%q status = %q, want %q so clients can select it without an explicit load",
				row.ID, row.Status.Value, ModelStatusLoaded)
		}
	}
}

func TestRouterCatalog_ReportsLoadState(t *testing.T) {
	router, mgr, clk := newCatalogRouter(t)

	byID := func() map[string]catalogRow {
		out := map[string]catalogRow{}
		for _, r := range fetchCatalog(t, router, "/models") {
			out[r.ID] = r
		}
		return out
	}

	// Nothing running, but usable on demand.
	if got := byID()["plain"].Status.Value; got != ModelStatusLoaded {
		t.Errorf("status = %q, want %q before launch", got, ModelStatusLoaded)
	}

	// A process that exists but has not passed its health check is "loading" —
	// clients poll on this transition, so it must not read as unloaded.
	inst := &serverInstance{ready: make(chan struct{}), lastUsed: clk.Now()}
	mgr.mu.Lock()
	mgr.instances["plain"] = inst
	mgr.mu.Unlock()
	if got := byID()["plain"].Status.Value; got != ModelStatusLoading {
		t.Errorf("status = %q, want %q while starting up", got, ModelStatusLoading)
	}

	inst.healthy.Store(true)
	if got := byID()["plain"].Status.Value; got != ModelStatusLoaded {
		t.Errorf("status = %q, want %q once healthy", got, ModelStatusLoaded)
	}

	// A model that never started is still selectable.
	if got := byID()["vision"].Status.Value; got != ModelStatusLoaded {
		t.Errorf("vision status = %q, want %q", got, ModelStatusLoaded)
	}
}

// A live instance proves any recorded failure is stale.
func TestRouterCatalog_RunningInstanceOverridesStaleFailure(t *testing.T) {
	router, mgr, clk := newCatalogRouter(t)

	mgr.mu.Lock()
	mgr.loadErrors["plain"] = "an old failure"
	mgr.mu.Unlock()
	addInstance(mgr, "plain", 0, clk.Now())

	for _, row := range fetchCatalog(t, router, "/models") {
		if row.ID != "plain" {
			continue
		}
		if row.Status.Failed {
			t.Error("stale failure reported for an alias that is running")
		}
		if row.Status.Value != ModelStatusLoaded {
			t.Errorf("status = %q, want %q", row.Status.Value, ModelStatusLoaded)
		}
	}
}

func TestRouterCatalog_MetadataMapping(t *testing.T) {
	router, _, _ := newCatalogRouter(t)

	rows := map[string]catalogRow{}
	for _, r := range fetchCatalog(t, router, "/models") {
		rows[r.ID] = r
	}

	// ctx-size drives n_ctx, which the client turns into contextWindow.
	// Without it the client silently assumes 128000.
	if rows["plain"].Meta == nil || rows["plain"].Meta.NCtx != 32768 {
		t.Errorf("plain meta = %+v, want n_ctx 32768", rows["plain"].Meta)
	}
	// No ctx-size configured: omit meta rather than claim a wrong number.
	if rows["vision"].Meta != nil {
		t.Errorf("vision meta = %+v, want omitted when ctx-size is unset", rows["vision"].Meta)
	}

	if got := rows["plain"].Architecture.InputModalities; len(got) != 1 || got[0] != "text" {
		t.Errorf("plain modalities = %v, want [text]", got)
	}
	// mmproj configured means the model can take images.
	if got := rows["vision"].Architecture.InputModalities; len(got) != 2 || got[1] != "image" {
		t.Errorf("vision modalities = %v, want [text image]", got)
	}
}

func TestRouterCatalog_SurfacesLoadFailure(t *testing.T) {
	router, mgr, _ := newCatalogRouter(t)

	mgr.mu.Lock()
	mgr.loadErrors["plain"] = "binary not found"
	mgr.mu.Unlock()

	var row catalogRow
	for _, r := range fetchCatalog(t, router, "/models") {
		if r.ID == "plain" {
			row = r
		}
	}
	// Without failed:true a polling client spins until its own timeout.
	if !row.Status.Failed {
		t.Error("status.failed not set after a failed load; clients would poll forever")
	}
	// A model that cannot start is not usable, so it must not claim to be.
	if row.Status.Value != ModelStatusUnloaded {
		t.Errorf("status = %q, want %q after a failed load", row.Status.Value, ModelStatusUnloaded)
	}
	if row.Status.Error != "binary not found" {
		t.Errorf("status.error = %q, want the failure reason", row.Status.Error)
	}
}

func TestRouterCatalog_LoadClearsPriorFailure(t *testing.T) {
	router, mgr, _ := newCatalogRouter(t)

	mgr.mu.Lock()
	mgr.loadErrors["plain"] = "stale failure"
	mgr.mu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/models/load", bytes.NewReader([]byte(`{"model":"plain"}`)))
	router.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /models/load = %d, want 200", rec.Code)
	}

	// The launch runs in the background and will fail (no binary), but the
	// stale error must be cleared synchronously so the client does not read a
	// previous run's failure as this one's.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mgr.mu.Lock()
		msg := mgr.loadErrors["plain"]
		mgr.mu.Unlock()
		if msg != "stale failure" {
			return // cleared, then possibly replaced by the real failure
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stale load error was never cleared")
}

func TestRouterModelLoad_ReturnsWithoutWaiting(t *testing.T) {
	router, _, _ := newCatalogRouter(t)

	// Loading is asynchronous by contract: clients put a short timeout on the
	// request itself, so a cold model must not block the response.
	start := time.Now()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/models/load", bytes.NewReader([]byte(`{"model":"plain"}`)))
	router.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /models/load = %d, want 200", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("load took %s; it must return immediately, not wait for the model", elapsed)
	}
}

func TestRouterModelUnload_IsIdempotent(t *testing.T) {
	router, _, _ := newCatalogRouter(t)

	// Unloading something that is not running expresses a desired state, not
	// a transition — it must not error.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/models/unload", bytes.NewReader([]byte(`{"model":"plain"}`)))
		router.server.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("unload attempt %d = %d, want 200", i+1, rec.Code)
		}
	}
}

func TestRouterModelLoad_RejectsUnmanagedModel(t *testing.T) {
	router, _, _ := newCatalogRouter(t)

	tests := []struct {
		name string
		body string
	}{
		{"unknown alias", `{"model":"nope"}`},
		{"endpoint-prefixed model", `{"model":"omlx/Some-Model"}`},
		{"missing model field", `{}`},
		{"malformed json", `{`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/models/load", bytes.NewReader([]byte(tc.body)))
			router.server.Handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			// Clients read {"error":{"message":...}} for the reason.
			var payload struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if payload.Error.Message == "" {
				t.Errorf("no error.message in %s", rec.Body.String())
			}
		})
	}
}

func TestRouterCatalog_TrainedContextFallback(t *testing.T) {
	cfg := &ServerConfig{}
	mgr, _ := newBudgetManager(t, cfg, nil)
	cfg.Models = append(cfg.Models,
		ServerModelConfig{Alias: "pinned", Args: map[string]any{"ctx-size": 8192.0}},
		ServerModelConfig{Alias: "unpinned", Args: map[string]any{}},
	)
	// Native context as read from model metadata at construction.
	mgr.trainedContext["pinned"] = 131072
	mgr.trainedContext["unpinned"] = 262144

	router := NewRelayRouter("127.0.0.1:0", []*ServerManager{mgr}, nil)
	rows := map[string]catalogRow{}
	for _, r := range fetchCatalog(t, router, "/models") {
		rows[r.ID] = r
	}

	// Both figures when ctx-size pins a value below the model's native limit.
	if got := rows["pinned"].Meta; got == nil || got.NCtx != 8192 || got.NCtxTrain != 131072 {
		t.Errorf("pinned meta = %+v, want n_ctx 8192 and n_ctx_train 131072", got)
	}
	// No ctx-size: the native limit still gives clients a real number rather
	// than leaving them on a generic default.
	if got := rows["unpinned"].Meta; got == nil || got.NCtx != 0 || got.NCtxTrain != 262144 {
		t.Errorf("unpinned meta = %+v, want only n_ctx_train 262144", got)
	}
}

func TestUpstreamModelRow_ContextLengthFieldNames(t *testing.T) {
	// Each server family advertises context under a different key; we read
	// whichever one is present so endpoint models get a real window.
	tests := []struct {
		name string
		body string
		want int64
	}{
		{"vLLM / OMLX", `{"id":"m","max_model_len":262144}`, 262144},
		{"LM Studio", `{"id":"m","max_context_length":32768}`, 32768},
		{"context_length", `{"id":"m","context_length":16384}`, 16384},
		{"context_window", `{"id":"m","context_window":8192}`, 8192},
		{"llama.cpp meta", `{"id":"m","meta":{"n_ctx":4096}}`, 4096},
		{"meta n_ctx_train fallback", `{"id":"m","meta":{"n_ctx_train":2048}}`, 2048},
		{"none advertised", `{"id":"m"}`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var row upstreamModelRow
			if err := json.Unmarshal([]byte(tc.body), &row); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := row.contextLength(); got != tc.want {
				t.Errorf("contextLength() = %d, want %d", got, tc.want)
			}
		})
	}
}

// Endpoint-backed rows must carry the same keys as managed ones. A client that
// reads architecture.input_modalities unconditionally panics on a row that
// omits it, taking the whole catalog down with it.
func TestRouterCatalog_EndpointRowsCarryArchitecture(t *testing.T) {
	upstream := newFakeOpenAIUpstream(t, []string{"gpt-test"})
	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{
			{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "test-key"},
		},
	})

	router := NewRelayRouter(":0", nil, registry)

	var row catalogRow
	for _, r := range fetchCatalog(t, router, "/v1/models") {
		if r.ID == "fakeep/gpt-test" {
			row = r
		}
	}
	if row.ID == "" {
		t.Fatal("endpoint model missing from catalog")
	}
	if row.Architecture == nil {
		t.Fatal("endpoint row has no architecture; clients reading it unconditionally reject the catalog")
	}
	// The upstream advertises no modality field, so text is all we can honestly
	// claim — a VLM behind the endpoint is indistinguishable from here.
	if got := row.Architecture.InputModalities; len(got) != 1 || got[0] != "text" {
		t.Errorf("endpoint modalities = %v, want [text]", got)
	}
}

// An upstream that speaks llama.cpp router mode declares its modalities; we
// pass that through rather than flattening every endpoint model to text. This
// is the only way a VLM behind an OpenAI endpoint can be distinguished from a
// text model — plain /v1/models has no field for it.
func TestRouterCatalog_EndpointVisionPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "vlm", "architecture": map[string]any{"input_modalities": []string{"text", "image"}}},
			{"id": "txt", "architecture": map[string]any{"input_modalities": []string{"text"}}},
			{"id": "quiet"},
		}})
	}))
	defer upstream.Close()

	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "ep", BaseURL: upstream.URL + "/v1"}},
	})
	router := NewRelayRouter(":0", nil, registry)

	rows := map[string]catalogRow{}
	for _, r := range fetchCatalog(t, router, "/v1/models") {
		rows[r.ID] = r
	}

	if got := rows["ep/vlm"].Architecture.InputModalities; len(got) != 2 || got[1] != "image" {
		t.Errorf("ep/vlm modalities = %v, want [text image]", got)
	}
	if got := rows["ep/txt"].Architecture.InputModalities; len(got) != 1 || got[0] != "text" {
		t.Errorf("ep/txt modalities = %v, want [text]", got)
	}
	// Silence is not a vision claim: sending images to a server that never said
	// it takes them fails mid-turn, which is worse than not offering it.
	if got := rows["ep/quiet"].Architecture.InputModalities; len(got) != 1 || got[0] != "text" {
		t.Errorf("ep/quiet modalities = %v, want [text]", got)
	}
}
