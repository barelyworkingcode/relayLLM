package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"relayllm/internal/config"
	regpkg "relayllm/internal/registry"
	"relayllm/internal/servermanager"
	"testing"
)

func fetchCatalogRows(t *testing.T, router *RelayRouter) map[string]map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	router.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, want 200", rec.Code)
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	rows := make(map[string]map[string]any, len(payload.Data))
	for _, row := range payload.Data {
		if id, ok := row["id"].(string); ok {
			rows[id] = row
		}
	}
	return rows
}

func TestAnthropic_ModelsCatalog_ListsMappedKeyOnlyWhileTargetUsable(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		target     string
		extra      map[string]string // further modelMap entries, each expected listed
		online     bool
		advertised []string
		wantListed bool
	}{
		{name: "endpoint offline", target: "acme/m1", online: false, wantListed: false},
		{name: "endpoint online advertising model", target: "acme/m1", online: true, advertised: []string{"m1"}, wantListed: true},
		{name: "endpoint online without model", target: "acme/m1", online: true, advertised: []string{"m2"}, wantListed: false},
		{name: "managed alias", target: "local-model", online: false, wantListed: true},
		{name: "virtual with fresh candidate", target: "vCode", online: true, advertised: []string{"m1"}, wantListed: true},
		{name: "virtual with no fresh candidate", target: "vCode", online: false, wantListed: false},
		{name: "unknown target", target: "nowhere", online: true, advertised: []string{"m1"}, wantListed: false},
		// The chain key sorts after the key it targets, so a catalog built in
		// key order has already listed the target when it reaches the chain.
		{name: "target is another mapped key", key: "acme/chain", target: "acme/base",
			extra: map[string]string{"acme/base": "local-model"}, online: true, advertised: []string{"m1"}, wantListed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := tc.key
			if key == "" {
				key = "acme/coder"
			}
			modelMap := map[string]string{key: tc.target}
			for k, v := range tc.extra {
				modelMap[k] = v
			}
			ep := config.OpenAIEndpoint{Name: "acme", BaseURL: "http://127.0.0.1:1/v1"}
			reg := regpkg.NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{ep}})
			var models []regpkg.UpstreamModel
			for _, id := range tc.advertised {
				models = append(models, regpkg.UpstreamModel{ID: id})
			}
			reg.SetStatusForTest(ep, tc.online, models...)

			mgr := servermanager.NewServerManager(servermanager.LlamaProfile, &config.ServerConfig{
				Models: []config.ServerModelConfig{{Alias: "local-model", Args: map[string]any{"model": "/fake"}}},
			}, "")
			virtual := &config.VirtualLLMConfig{Models: []config.VirtualLLM{{
				Name: "vCode", Targets: []config.VirtualLLMTarget{{Endpoint: "acme", Model: "m1"}},
			}}}
			router := newAnthropicRouter(t,
				anthropicUpstreamCfg(t, "https://unused.invalid", modelMap),
				[]*servermanager.ServerManager{mgr}, reg, virtual)

			rows := fetchCatalogRows(t, router)
			for k := range tc.extra {
				if _, ok := rows[k]; !ok {
					t.Fatalf("precondition: mapped key %q not listed", k)
				}
			}
			row, listed := rows[key]
			if listed != tc.wantListed {
				t.Fatalf("%q listed = %v, want %v (target %q)", key, listed, tc.wantListed, tc.target)
			}
			if _, ok := rows["vCode"]; !ok {
				t.Errorf("virtual row vCode missing; it must stay listed whatever its reachability")
			}
			if !listed {
				return
			}
			if row["owned_by"] != "anthropic-map" {
				t.Errorf("owned_by = %v, want anthropic-map", row["owned_by"])
			}
			if row["target"] != tc.target {
				t.Errorf("target = %v, want %q", row["target"], tc.target)
			}
			status, _ := row["status"].(map[string]any)
			if status["value"] != servermanager.ModelStatusLoaded {
				t.Errorf("status = %v, want value %q", row["status"], servermanager.ModelStatusLoaded)
			}
		})
	}
}
