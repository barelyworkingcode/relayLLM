//go:build llm

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Estimator check against the real models registered in the user's
// settings.json. Tagged `llm` because it needs the GGUF files on disk; skips
// cleanly when the config or the models are absent.
//
// Run with: go test -tags=llm -run TestEstimate -v .
func TestEstimateModelMemory_AgainstInstalledModels(t *testing.T) {
	home, err := os.UserConfigDir()
	if err != nil {
		t.Skip("no user config dir")
	}
	dataDir := filepath.Join(home, "relayLLM")
	if _, err := os.Stat(filepath.Join(dataDir, "settings.json")); err != nil {
		t.Skipf("no settings.json in %s", dataDir)
	}

	cfg, err := LoadConfig(dataDir, "")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Llama == nil || len(cfg.Llama.Models) == 0 {
		t.Skip("no llama-server models configured")
	}

	var total int64
	for _, mc := range cfg.Llama.Models {
		modelPath, _ := mc.Args["model"].(string)
		if _, err := os.Stat(modelPath); err != nil {
			t.Logf("%-24s SKIP (model file absent)", mc.Alias)
			continue
		}

		weights, kv, err := estimateLlamaMemory(modelPath, mc)
		if err != nil {
			t.Errorf("%s: estimateLlamaMemory: %v", mc.Alias, err)
			continue
		}
		est := estimateModelMemory(llamaProfile, mc, cfg.Llama.MemoryHeadroomPercent)
		total += est

		t.Logf("%-24s weights=%-9s kv=%-9s total=%-9s (with headroom %s)",
			mc.Alias, formatGB(weights), formatGB(kv), formatGB(weights+kv), formatGB(est))

		if weights <= 0 {
			t.Errorf("%s: weights estimated at zero", mc.Alias)
		}
		// A KV cache of exactly zero means the geometry was not found, which
		// would silently under-budget every large-context model.
		if kv <= 0 {
			t.Errorf("%s: KV cache estimated at zero", mc.Alias)
		}
	}
	t.Logf("all models resident simultaneously: %s", formatGB(total))
}
