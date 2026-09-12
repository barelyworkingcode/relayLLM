package session

import (
	"testing"

	"relayllm/internal/config"
)

// --- Provider-type derivation ---
//
// Carved out of provider_openai_test.go when provider_openai.go moved to
// internal/provider: deriveProviderType lives in session.go and moves to
// internal/session in a later migration step, at which point this file
// moves with it.

func TestDeriveProviderType_OpenAIPrefix(t *testing.T) {
	cfg := &config.OpenAIConfig{
		Endpoints: []config.OpenAIEndpoint{
			{Name: "lmstudio", BaseURL: "http://x/v1"},
			{Name: "omlx", BaseURL: "http://y/v1"},
		},
	}
	cases := []struct {
		model string
		want  string
	}{
		{"haiku", "claude"},
		{"sonnet", "claude"},
		{"opus", "claude"},
		{"lmstudio/qwen-7b", "openai"},
		{"omlx/llama3", "openai"},
		{"unknown/qwen-7b", "ollama"}, // unknown prefix falls through
		{"qwen:7b", "ollama"},         // no slash
		{"gemma3:4b", "ollama"},
		{"/leading", "ollama"}, // malformed
	}
	for _, tc := range cases {
		got := deriveProviderType(tc.model, cfg, nil, nil)
		if got != tc.want {
			t.Errorf("deriveProviderType(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

func TestDeriveProviderType_MlxAlias(t *testing.T) {
	cfg := &config.OpenAIConfig{
		Endpoints: []config.OpenAIEndpoint{
			{Name: "lmstudio", BaseURL: "http://x/v1"},
		},
	}
	mlxCfg := &config.ServerConfig{
		Models: []config.ServerModelConfig{{Alias: "foo"}},
	}
	cases := []struct {
		model string
		want  string
	}{
		{"mlx/foo", "mlx"},     // configured mlx alias
		{"mlx/nope", "ollama"}, // unknown mlx alias with non-nil config → falls through
	}
	for _, tc := range cases {
		got := deriveProviderType(tc.model, cfg, nil, mlxCfg)
		if got != tc.want {
			t.Errorf("deriveProviderType(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}
