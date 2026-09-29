package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_SplashSectionNeverNil(t *testing.T) {
	tests := map[string]string{
		"no settings.json":             "",
		"settings.json without splash": `{"mlx-serve":{"models":[]}}`,
	}
	for name, settings := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if settings != "" {
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := LoadConfig(dir, "")
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.Splash == nil {
				t.Fatal("Splash section is nil; callers read cfg.Splash.Models unguarded")
			}
		})
	}
}

// A Splash model is a repo id, not a path, so modelDir must not be joined onto it.
func TestParseUnifiedConfig_SplashIgnoresModelDir(t *testing.T) {
	cfg, err := parseUnifiedConfig([]byte(`{"splash-serve":{"modelDir":"/base","models":[
		{"alias":"tiny","model":"acme/tiny-GGUF:Q4"}]}}`), "test.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Splash.Models) != 1 {
		t.Fatalf("splash models = %+v, want one", cfg.Splash.Models)
	}
	if got := cfg.Splash.Models[0].Args["model"]; got != "acme/tiny-GGUF:Q4" {
		t.Errorf("model = %v, want the repo id unchanged", got)
	}
}
