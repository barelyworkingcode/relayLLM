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
		NCtx int64 `json:"n_ctx"`
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

func TestRouterCatalog_ReportsLoadState(t *testing.T) {
	router, mgr, clk := newCatalogRouter(t)

	byID := func() map[string]catalogRow {
		out := map[string]catalogRow{}
		for _, r := range fetchCatalog(t, router, "/models") {
			out[r.ID] = r
		}
		return out
	}

	// Nothing running yet.
	if got := byID()["plain"].Status.Value; got != ModelStatusUnloaded {
		t.Errorf("status = %q, want %q before launch", got, ModelStatusUnloaded)
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

	// A model that never started must stay unloaded.
	if got := byID()["vision"].Status.Value; got != ModelStatusUnloaded {
		t.Errorf("vision status = %q, want %q", got, ModelStatusUnloaded)
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
