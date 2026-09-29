package config

import (
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// parseUnifiedConfig — mlx-serve section parsing
// ---------------------------------------------------------------------------

func TestParseUnifiedConfig_MlxServeSection(t *testing.T) {
	data := []byte(`{
		"mlx-serve": {
			"modelDir": "/base",
			"models": [
				{"alias": "q4", "model": "sub/dir", "temp": 0.7}
			]
		}
	}`)

	cfg, err := parseUnifiedConfig(data, "test.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Mlx section should be parsed.
	if cfg.Mlx == nil {
		t.Fatal("cfg.Mlx is nil")
	}
	if len(cfg.Mlx.Models) != 1 {
		t.Fatalf("Mlx models count: got %d, want 1", len(cfg.Mlx.Models))
	}
	m := cfg.Mlx.Models[0]
	if m.Alias != "q4" {
		t.Errorf("alias: got %q, want q4", m.Alias)
	}
	// Relative model path should be resolved against modelDir.
	if m.Args["model"] != "/base/sub/dir" {
		t.Errorf("model: got %v, want /base/sub/dir", m.Args["model"])
	}
	// temp arg should be preserved as a float64 from JSON.
	if v, ok := m.Args["temp"].(float64); !ok || v != 0.7 {
		t.Errorf("temp: got %v, want 0.7", m.Args["temp"])
	}

	// Llama section absent → empty non-nil config.
	if cfg.Llama == nil {
		t.Fatal("cfg.Llama should be non-nil even when absent")
	}
	if len(cfg.Llama.Models) != 0 {
		t.Errorf("Llama models count: got %d, want 0", len(cfg.Llama.Models))
	}
}

func TestParseUnifiedConfig_VirtualLLMs(t *testing.T) {
	cfg, err := parseUnifiedConfig([]byte(`{
		"virtual-llms": {"models": [{
			"name": "vCode",
			"targets": [{"endpoint": "remote", "model": "code"}, {"alias": "local-code"}]
		}]}
	}`), "test.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Virtual == nil || len(cfg.Virtual.Models) != 1 {
		t.Fatalf("virtual models = %+v, want one", cfg.Virtual)
	}
	got := cfg.Virtual.Models[0]
	if got.Name != "vCode" || len(got.Targets) != 2 || got.Targets[0].Endpoint != "remote" || got.Targets[1].Alias != "local-code" {
		t.Errorf("virtual model = %+v, want vCode with ordered remote/local targets", got)
	}
}

// Absent "router" section → empty, non-nil config, exactly like Virtual/Llama
// above: callers dereference cfg.Router without a nil check.
func TestParseUnifiedConfig_RouterSection_AbsentIsEmptyNonNil(t *testing.T) {
	cfg, err := parseUnifiedConfig([]byte(`{}`), "test.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Router == nil {
		t.Fatal("cfg.Router should be non-nil even when absent")
	}
}

// A settings.json written before the reasoning-effort rewrites were removed
// still loads, and the router keys that remain are read as before.
func TestParseUnifiedConfig_RemovedReasoningKeysStillLoad(t *testing.T) {
	cfg, err := parseUnifiedConfig([]byte(`{
		"openai": {"endpoints": [{"name": "ep", "baseURL": "http://127.0.0.1:1234/v1"}]},
		"router": {
			"reasoningEffortMap": {"minimal": "none"},
			"reasoningEffortTemplateKwargs": {"minimal": {"enable_thinking": false}},
			"anthropic": {"modelMap": {"acme/coder": "ep/code"}},
			"passthrough": {"acme": {"upstream": "https://api.example.com"}}
		}
	}`), "test.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.OpenAI.Endpoints) != 1 || cfg.OpenAI.Endpoints[0].Name != "ep" {
		t.Errorf("openai endpoints = %+v, want one named ep", cfg.OpenAI.Endpoints)
	}
	if cfg.Router.Anthropic == nil || cfg.Router.Anthropic.ModelMap["acme/coder"] != "ep/code" {
		t.Errorf("router.anthropic = %+v, want modelMap acme/coder -> ep/code", cfg.Router.Anthropic)
	}
	if got := cfg.Router.Passthrough["acme"].Upstream; got != "https://api.example.com" {
		t.Errorf("router.passthrough[acme].upstream = %q, want https://api.example.com", got)
	}
}

func TestLoadConfig_RouterSystemModels(t *testing.T) {
	cases := []struct {
		name, body string
		want       []string
	}{
		{"absent", `{}`, nil},
		{"empty list", `{"router":{"systemModels":[]}}`, nil},
		{"listed", `{"router":{"systemModels":["acme-draft","ep/draft"]}}`, []string{"acme-draft", "ep/draft"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadFrom(t, map[string]string{"settings.json": tc.body})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if !slices.Equal(cfg.Router.SystemModels, tc.want) {
				t.Errorf("SystemModels = %q, want %q", cfg.Router.SystemModels, tc.want)
			}
		})
	}
}

func TestLoadConfig_RouterSystemModelsEmptyEntryFailsNamingIndex(t *testing.T) {
	_, err := loadFrom(t, map[string]string{"settings.json": `{"router":{"systemModels":["acme-draft",""]}}`})
	if err == nil || !strings.Contains(err.Error(), "router.systemModels[1]") {
		t.Fatalf("err = %v, want a load failure naming router.systemModels[1]", err)
	}
}
