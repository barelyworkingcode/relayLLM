package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFrom(t *testing.T, files map[string]string) (*LoadedConfig, error) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return LoadConfig(dir, "")
}

func TestLoadConfig_UnknownAPIValueFailsNamingWhere(t *testing.T) {
	cases := []struct {
		name, file, body, where, bad string
	}{
		{"endpoint", "settings.json", `{"openai":{"endpoints":[{"name":"acme","baseURL":"http://127.0.0.1:1234/v1","api":["openai","anthropics"]}]}}`, "acme", "anthropics"},
		{"llama-server", "settings.json", `{"llama-server":{"api":["Anthropic"],"models":[]}}`, "llama-server", "Anthropic"},
		{"mlx-serve", "settings.json", `{"mlx-serve":{"api":["messages"],"models":[]}}`, "mlx-serve", "messages"},
		{"splash-serve", "settings.json", `{"splash-serve":{"api":[""],"models":[]}}`, "splash-serve", `""`},
		{"legacy llama_models.json", "llama_models.json", `{"api":["claude"],"models":[]}`, "llama_models.json", "claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFrom(t, map[string]string{tc.file: tc.body})
			if err == nil {
				t.Fatal("load succeeded, want an error for the unknown api value")
			}
			for _, want := range []string{tc.where, tc.bad, "openai", "anthropic"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestLoadConfig_APIDeclarations(t *testing.T) {
	cases := []struct {
		name, api string
		anthropic bool
	}{
		{"absent", ``, false},
		{"null", `"api":null,`, false},
		{"empty", `"api":[],`, false},
		{"openai only", `"api":["openai"],`, false},
		{"both", `"api":["openai","anthropic"],`, true},
		{"duplicates", `"api":["anthropic","anthropic"],`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadFrom(t, map[string]string{"settings.json": `{
				"openai":{"endpoints":[{"name":"acme",` + tc.api + `"baseURL":"http://127.0.0.1:1234/v1"}]},
				"llama-server":{` + tc.api + `"models":[{"alias":"a","model":"/m.gguf","ctx-size":4096}]},
				"mlx-serve":{` + tc.api + `"models":[]},
				"splash-serve":{` + tc.api + `"models":[]}}`})
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if got := cfg.OpenAI.Endpoints[0].SpeaksAnthropic(); got != tc.anthropic {
				t.Errorf("endpoint SpeaksAnthropic = %v, want %v", got, tc.anthropic)
			}
			for name, sec := range map[string]*ServerConfig{"llama-server": cfg.Llama, "mlx-serve": cfg.Mlx, "splash-serve": cfg.Splash} {
				if got := sec.SpeaksAnthropic(); got != tc.anthropic {
					t.Errorf("%s SpeaksAnthropic = %v, want %v", name, got, tc.anthropic)
				}
			}
			// Per-model keys become CLI flags; the section-level api must not.
			if _, leaked := cfg.Llama.Models[0].Args["api"]; leaked {
				t.Errorf("section api leaked into model args %v", cfg.Llama.Models[0].Args)
			}
		})
	}
}

func TestServerConfigSpeaksAnthropic_NilSafe(t *testing.T) {
	var c *ServerConfig
	if c.SpeaksAnthropic() {
		t.Error("nil ServerConfig reports anthropic")
	}
}
