package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// OpenAIEndpoint describes a single OpenAI-compatible chat completions server.
// Multiple endpoints can be configured side-by-side (Ollama's /v1, LM Studio,
// OMLX, OpenAI proper, etc.). The Name field is used as a routing prefix on
// model identifiers (e.g. "lmstudio/qwen2.5-7b").
type OpenAIEndpoint struct {
	Name    string `json:"name"`    // routing prefix, e.g. "lmstudio"
	BaseURL string `json:"baseURL"` // e.g. "http://localhost:1234/v1"
	APIKey  string `json:"apiKey"`  // optional; sent as "Authorization: Bearer ..."
	Group   string `json:"group"`   // display group in model picker; defaults to Name
}

// OpenAIConfig is the top-level config file structure.
type OpenAIConfig struct {
	Endpoints []OpenAIEndpoint `json:"endpoints"`
}

// Find returns a pointer to the endpoint with the given Name, or nil if none
// matches. Comparison is case-sensitive to match how model prefixes are
// parsed elsewhere.
func (c *OpenAIConfig) Find(name string) *OpenAIEndpoint {
	if c == nil {
		return nil
	}
	for i := range c.Endpoints {
		if c.Endpoints[i].Name == name {
			return &c.Endpoints[i]
		}
	}
	return nil
}

// Names returns the list of endpoint names, in declaration order. Used by
// session routing to decide whether a "prefix/model" string refers to a
// configured OpenAI endpoint or falls through to the Ollama provider.
func (c *OpenAIConfig) Names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, len(c.Endpoints))
	for i, e := range c.Endpoints {
		names[i] = e.Name
	}
	return names
}

// LoadOpenAIConfig reads the JSON config file at path. If the file does not
// exist, it falls back to the OPENAI_BASE_URL / OPENAI_API_KEY env vars; if
// those are also unset, it returns an empty (but non-nil) config.
//
// Parse errors on an existing file are returned to the caller — a malformed
// config file should be surfaced loudly, not silently fall back to env vars.
func LoadOpenAIConfig(path string) (*OpenAIConfig, error) {
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			var cfg OpenAIConfig
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			normalize(&cfg)
			return &cfg, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		// File does not exist → fall through to env var fallback.
	}

	if baseURL := os.Getenv("OPENAI_BASE_URL"); baseURL != "" {
		name := os.Getenv("OPENAI_ENDPOINT_NAME")
		if name == "" {
			name = "openai"
		}
		cfg := &OpenAIConfig{
			Endpoints: []OpenAIEndpoint{
				{
					Name:    name,
					BaseURL: baseURL,
					APIKey:  os.Getenv("OPENAI_API_KEY"),
					Group:   "OpenAI",
				},
			},
		}
		normalize(cfg)
		return cfg, nil
	}

	return &OpenAIConfig{}, nil
}

// normalize trims trailing slashes from base URLs and defaults Group to Name.
func normalize(cfg *OpenAIConfig) {
	for i := range cfg.Endpoints {
		cfg.Endpoints[i].BaseURL = strings.TrimRight(cfg.Endpoints[i].BaseURL, "/")
		if cfg.Endpoints[i].Group == "" {
			cfg.Endpoints[i].Group = cfg.Endpoints[i].Name
		}
	}
}
