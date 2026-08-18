package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestPiOverlayMaterialize exercises the happy path: relay providers are
// merged on top of the user's global pi config, settings inherit user
// preferences, auth.json is symlinked back to the global file, and the
// disabled case is a no-op. The temp-HOME setup ensures globalPiAgentDir()
// resolves into the test fixture, not the developer's real ~/.pi/.
func TestPiOverlayMaterialize(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	globalDir := filepath.Join(tmp, ".pi", "agent")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(globalDir, "auth.json"), `{"anthropic":"sk-test"}`)
	mustWrite(t, filepath.Join(globalDir, "models.json"),
		`{"providers":{"user-ollama":{"baseUrl":"http://127.0.0.1:11434/v1","api":"openai-completions"}}}`)
	mustWrite(t, filepath.Join(globalDir, "settings.json"), `{"theme":"dark"}`)

	projectDir := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(filepath.Join(projectDir, ".claude", "skills", "relay"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &PiConfig{
		ProjectOverlay: PiProjectOverlay{
			Mode:            AutoRegenAlways,
			DefaultProvider: piRelayRouterProvider,
			DefaultModel:    "qwen3-8b",
			DefaultThinking: "medium",
		},
	}
	inputs := PiOverlayInputs{
		ServerModels: []ServerModelConfig{
			{Alias: "qwen3-8b"},
			{Alias: "deepseek-r1"},
		},
		RouterPort: "8091",
		RouterModels: []PiRouterModel{
			{ID: "qwen3-8b", SupportsImages: true},
			{ID: "deepseek-r1"},
			{ID: "lmstudio/Qwen3.5-27B"},
		},
	}

	overlayDir, err := MaterializePiOverlay(projectDir, cfg, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if overlayDir == "" {
		t.Fatal("overlay dir empty when Mode=always")
	}

	var models struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			Models  []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	mustReadJSON(t, filepath.Join(overlayDir, "models.json"), &models)
	for _, want := range []string{piRelayRouterProvider, "user-ollama"} {
		if _, ok := models.Providers[want]; !ok {
			t.Errorf("models.json missing provider %q", want)
		}
	}
	if _, ok := models.Providers["lmstudio"]; ok {
		t.Errorf("per-endpoint provider leaked into overlay; expected consolidation under relay-router")
	}
	router := models.Providers[piRelayRouterProvider]
	if router.BaseURL != "http://localhost:8091/v1" {
		t.Errorf("relay-router baseUrl=%q", router.BaseURL)
	}
	gotIDs := make(map[string]bool, len(router.Models))
	for _, m := range router.Models {
		gotIDs[m.ID] = true
	}
	for _, want := range []string{"qwen3-8b", "deepseek-r1", "lmstudio/Qwen3.5-27B"} {
		if !gotIDs[want] {
			t.Errorf("relay-router models missing %q (got %v)", want, gotIDs)
		}
	}

	var settings map[string]any
	mustReadJSON(t, filepath.Join(overlayDir, "settings.json"), &settings)
	if settings["defaultProvider"] != piRelayRouterProvider {
		t.Errorf("defaultProvider=%v", settings["defaultProvider"])
	}
	if settings["defaultModel"] != "qwen3-8b" {
		t.Errorf("defaultModel=%v", settings["defaultModel"])
	}
	if settings["theme"] != "dark" {
		t.Errorf("user-global theme not preserved: %v", settings["theme"])
	}
	if skills, _ := settings["skills"].([]any); len(skills) == 0 {
		t.Errorf("skills array empty")
	}

	authPath := filepath.Join(overlayDir, "auth.json")
	info, err := os.Lstat(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("auth.json not a symlink: mode=%v", info.Mode())
	}

	// Idempotency.
	if _, err := MaterializePiOverlay(projectDir, cfg, inputs); err != nil {
		t.Fatalf("second materialize: %v", err)
	}

	// Disabled overlay must be a no-op even if everything else is set.
	disabled := &PiConfig{}
	if dir, err := MaterializePiOverlay(projectDir, disabled, inputs); err != nil || dir != "" {
		t.Errorf("disabled overlay: dir=%q err=%v", dir, err)
	}
}

// TestPiOverlayMissingAuthErrors guards the fail-closed contract: when the
// symlink strategy is active but global auth.json doesn't exist, materialize
// must return an error rather than spawn pi without credentials.
func TestPiOverlayMissingAuthErrors(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	projectDir := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &PiConfig{ProjectOverlay: PiProjectOverlay{Mode: AutoRegenAlways}}

	if _, err := MaterializePiOverlay(projectDir, cfg, PiOverlayInputs{}); err == nil {
		t.Error("expected error when global auth.json missing, got nil")
	}

	// authStrategy:"none" opts out of the symlink and must succeed.
	cfg.ProjectOverlay.AuthStrategy = "none"
	if _, err := MaterializePiOverlay(projectDir, cfg, PiOverlayInputs{}); err != nil {
		t.Errorf("authStrategy:none should skip symlink: %v", err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustReadJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// pi gates image attachments on the model's `input` array (model-config.js:
// input: ("text"|"image")[]). Bare {"id": ...} rows leave every model
// text-only, so pi answers "Current model does not support images" even when
// the managed server was launched with an mmproj and can see fine.
func TestPiOverlay_ModelsCarryInputModalities(t *testing.T) {
	projectDir := t.TempDir()
	cfg := &PiConfig{
		ProjectOverlay: PiProjectOverlay{
			Mode:                 AutoRegenAlways,
			ExcludeUserProviders: true,
			ExcludeUserSettings:  true,
			AuthStrategy:         "none",
		},
	}
	inputs := PiOverlayInputs{
		RouterPort: "8180",
		RouterModels: []PiRouterModel{
			{ID: "vision-model", SupportsImages: true},
			{ID: "text-model"},
		},
	}

	overlayDir, err := MaterializePiOverlay(projectDir, cfg, inputs)
	if err != nil {
		t.Fatal(err)
	}

	var models struct {
		Providers map[string]struct {
			Models []struct {
				ID    string   `json:"id"`
				Input []string `json:"input"`
			} `json:"models"`
		} `json:"providers"`
	}
	mustReadJSON(t, filepath.Join(overlayDir, "models.json"), &models)

	got := map[string][]string{}
	for _, m := range models.Providers[piRelayRouterProvider].Models {
		got[m.ID] = m.Input
	}
	if want := []string{"text", "image"}; !reflect.DeepEqual(got["vision-model"], want) {
		t.Errorf("vision-model input = %v, want %v", got["vision-model"], want)
	}
	// Stated explicitly rather than left to pi's default, so the contract is
	// visible in the file.
	if want := []string{"text"}; !reflect.DeepEqual(got["text-model"], want) {
		t.Errorf("text-model input = %v, want %v", got["text-model"], want)
	}
}
