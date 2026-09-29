package router

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"relayllm/internal/config"
	"relayllm/internal/servermanager"
	"relayllm/internal/testutil"
	"strings"
	"testing"
	"time"
)

// newSplashRouter loads one splash-serve model from settings.json, backed by
// a fake splash install with the model downloaded.
func newSplashRouter(t *testing.T) (*RelayRouter, *servermanager.ServerManager) {
	t.Helper()
	fake := testutil.NewFakeSplash(t)
	fake.Install(t, "acme/tiny-GGUF:Q4", "model.json")
	dir := t.TempDir()
	settings := fmt.Sprintf(`{"splash-serve":{"binaryPath":%q,"basePort":%d,"models":[
		{"alias":"tiny","model":"acme/tiny-GGUF:Q4","max-context":"140K","memoryGB":4}]}}`, fake.Binary, fake.BasePort)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(dir, "")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	mgr := servermanager.NewServerManager(servermanager.SplashProfile, cfg.Splash, "")
	mgr.SetSplashModelsRootForTest(fake.ModelsRoot)
	mgr.SetStopGraceForTest(200 * time.Millisecond)
	t.Cleanup(mgr.StopAll)
	return NewRelayRouter("127.0.0.1:0", []*servermanager.ServerManager{mgr}, nil, nil), mgr
}

func TestRouterSplash_CatalogRow(t *testing.T) {
	router, _ := newSplashRouter(t)

	rows := fetchCatalog(t, router, "/v1/models")
	if len(rows) != 1 {
		t.Fatalf("catalog = %+v, want one row", rows)
	}
	row := rows[0]
	if row.ID != "tiny" || row.OwnedBy != "Splash" {
		t.Errorf("row id/owned_by = %q/%q, want tiny/Splash", row.ID, row.OwnedBy)
	}
	// loaded = usable now: the router launches on demand.
	if row.Status == nil || row.Status.Value != "loaded" || row.Status.Failed {
		t.Errorf("status = %+v, want loaded, not failed", row.Status)
	}
	if row.ContextLength == nil || *row.ContextLength != 140*1024 {
		t.Errorf("context_length = %v, want %d from max-context 140K", row.ContextLength, 140*1024)
	}
}

func TestRouterSplash_ChatStreamsSameReplyAsDirectCall(t *testing.T) {
	router, mgr := newSplashRouter(t)
	srv := httptest.NewServer(router.server.Handler)
	defer srv.Close()
	body := []byte(`{"model":"tiny","stream":true,"messages":[{"role":"user","content":"hello there"}]}`)

	post := func(url string) (int, string, string) {
		t.Helper()
		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", url, err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(data)
	}

	code, ctype, viaRouter := post(srv.URL + "/v1/chat/completions")
	if code != http.StatusOK || !strings.HasPrefix(ctype, "text/event-stream") {
		t.Fatalf("router reply = %d %q %s, want 200 event stream", code, ctype, viaRouter)
	}
	if !strings.Contains(viaRouter, "hello there") || !strings.Contains(viaRouter, "data: [DONE]") {
		t.Errorf("router reply = %s, want the streamed echo and [DONE]", viaRouter)
	}

	instances := mgr.ListInstances()
	if len(instances) != 1 {
		t.Fatalf("instances = %+v, want the one launched splash", instances)
	}
	_, _, direct := post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", instances[0].Port))
	if viaRouter != direct {
		t.Errorf("router reply differs from direct call:\nrouter: %s\ndirect: %s", viaRouter, direct)
	}
}
