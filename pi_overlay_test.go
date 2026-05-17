package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
			DefaultProvider: piRelayLlamaProvider,
			DefaultModel:    "qwen3-8b",
			DefaultThinking: "medium",
		},
	}
	inputs := PiOverlayInputs{
		OpenAI: &OpenAIConfig{Endpoints: []OpenAIEndpoint{
			{Name: "lmstudio", BaseURL: "http://localhost:1234/v1", APIKey: "sk-lm-xxx"},
		}},
		LlamaModels: []LlamaModelConfig{
			{Alias: "qwen3-8b"},
			{Alias: "deepseek-r1"},
		},
		LlamaProxyPort: "8091",
	}

	overlayDir, err := MaterializePiOverlay(projectDir, cfg, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if overlayDir == "" {
		t.Fatal("overlay dir empty when Mode=always")
	}

	var models struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	mustReadJSON(t, filepath.Join(overlayDir, "models.json"), &models)
	for _, want := range []string{piRelayLlamaProvider, "lmstudio", "user-ollama"} {
		if _, ok := models.Providers[want]; !ok {
			t.Errorf("models.json missing provider %q", want)
		}
	}
	if got := models.Providers[piRelayLlamaProvider]["baseUrl"]; got != "http://localhost:8091/v1" {
		t.Errorf("relay-llama baseUrl=%v", got)
	}

	var settings map[string]any
	mustReadJSON(t, filepath.Join(overlayDir, "settings.json"), &settings)
	if settings["defaultProvider"] != piRelayLlamaProvider {
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
