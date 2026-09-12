package config

import "testing"

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

func TestParseUnifiedConfig_RouterSection(t *testing.T) {
	cfg, err := parseUnifiedConfig([]byte(`{
		"router": {"reasoningEffortMap": {"minimal": "none"}}
	}`), "test.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Router == nil {
		t.Fatal("cfg.Router is nil")
	}
	if got := cfg.Router.ReasoningEffortMap["minimal"]; got != "none" {
		t.Errorf("reasoningEffortMap[minimal] = %q, want %q", got, "none")
	}
}

// Sibling of TestParseUnifiedConfig_RouterSection above, for the
// chat_template_kwargs merge table — see RouterConfig.ReasoningEffortTemplateKwargs.
func TestParseUnifiedConfig_RouterSection_ReasoningEffortTemplateKwargs(t *testing.T) {
	cfg, err := parseUnifiedConfig([]byte(`{
		"router": {"reasoningEffortTemplateKwargs": {"minimal": {"enable_thinking": false}}}
	}`), "test.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Router == nil {
		t.Fatal("cfg.Router is nil")
	}
	got, ok := cfg.Router.ReasoningEffortTemplateKwargs["minimal"]["enable_thinking"]
	if boolVal, isBool := got.(bool); !ok || !isBool || boolVal {
		t.Errorf("reasoningEffortTemplateKwargs[minimal][enable_thinking] = %v (present=%v), want false", got, ok)
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
	if len(cfg.Router.ReasoningEffortMap) != 0 {
		t.Errorf("reasoningEffortMap = %v, want empty", cfg.Router.ReasoningEffortMap)
	}
	if len(cfg.Router.ReasoningEffortTemplateKwargs) != 0 {
		t.Errorf("reasoningEffortTemplateKwargs = %v, want empty", cfg.Router.ReasoningEffortTemplateKwargs)
	}
}
