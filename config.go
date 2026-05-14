package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// relayConfig is the unified config.json structure. Each top-level key is
// optional; missing sections produce empty (non-nil) configs.
type relayConfig struct {
	OpenAI      *OpenAIConfig               `json:"openai,omitempty"`
	LlamaServer *LlamaConfig                `json:"llama-server,omitempty"`
	PTY         map[string]TerminalTemplate `json:"pty,omitempty"`
}

// LoadConfig loads provider configuration. It tries sources in order:
//
//  1. {dataDir}/config.json — unified config (preferred)
//  2. Separate files — {dataDir}/openai_endpoints.json, {dataDir}/llama_models.json
//  3. Environment variables — OPENAI_BASE_URL / OPENAI_API_KEY (OpenAI only)
//
// The openaiConfigOverride flag (--openai-config) bypasses all of the above
// for the OpenAI section and reads that file directly.
//
// The returned pty map is the raw config.json `pty` section (may be nil if
// absent). Seeding happens in TemplateStore.Load, not here.
func LoadConfig(dataDir string, openaiConfigOverride string) (*OpenAIConfig, *LlamaConfig, map[string]TerminalTemplate, error) {
	configPath := filepath.Join(dataDir, "config.json")

	// Try unified config.json first.
	data, err := os.ReadFile(configPath)
	if err == nil {
		openaiCfg, llamaCfg, ptyCfg, err := parseUnifiedConfig(data, configPath)
		if err != nil {
			return nil, nil, nil, err // parse error — don't silently fall back
		}

		// --openai-config flag overrides the unified config's openai section.
		if openaiConfigOverride != "" {
			if override, err := loadOpenAIConfigFile(openaiConfigOverride); err == nil {
				openaiCfg = override
			} else if !os.IsNotExist(err) {
				return nil, nil, nil, err
			}
		}

		slog.Info("loaded config.json", "path", configPath)
		return openaiCfg, llamaCfg, ptyCfg, nil
	}
	if !os.IsNotExist(err) {
		return nil, nil, nil, fmt.Errorf("read %s: %w", configPath, err)
	}

	// config.json not found — fall back to separate files + env vars.
	openaiPath := openaiConfigOverride
	if openaiPath == "" {
		openaiPath = filepath.Join(dataDir, "openai_endpoints.json")
	}
	openaiCfg, err := LoadOpenAIConfig(openaiPath)
	if err != nil {
		return nil, nil, nil, err
	}

	llamaCfg, err := loadLlamaConfigFile(filepath.Join(dataDir, "llama_models.json"))
	if err != nil {
		return nil, nil, nil, err
	}

	return openaiCfg, llamaCfg, nil, nil
}

// parseUnifiedConfig parses the unified config.json into separate configs.
func parseUnifiedConfig(data []byte, source string) (*OpenAIConfig, *LlamaConfig, map[string]TerminalTemplate, error) {
	// Use a raw intermediate so llama-server's model entries stay as
	// map[string]any for the generic CLI flag translation.
	var raw struct {
		OpenAI      *OpenAIConfig               `json:"openai"`
		LlamaServer *json.RawMessage            `json:"llama-server"`
		PTY         map[string]TerminalTemplate `json:"pty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", source, err)
	}

	openaiCfg := &OpenAIConfig{}
	if raw.OpenAI != nil {
		openaiCfg = raw.OpenAI
		normalizeOpenAI(openaiCfg)
	}

	llamaCfg := &LlamaConfig{}
	if raw.LlamaServer != nil {
		if err := json.Unmarshal(*raw.LlamaServer, llamaCfg); err != nil {
			return nil, nil, nil, fmt.Errorf("parse %s llama-server: %w", source, err)
		}
		if err := parseLlamaRawModels(llamaCfg, source); err != nil {
			return nil, nil, nil, err
		}
	}

	return openaiCfg, llamaCfg, raw.PTY, nil
}

// loadOpenAIConfigFile reads an OpenAI config from a specific file path.
// Does not fall back to env vars.
func loadOpenAIConfigFile(path string) (*OpenAIConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg OpenAIConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	normalizeOpenAI(&cfg)
	return &cfg, nil
}

// loadLlamaConfigFile reads a standalone llama_models.json file.
func loadLlamaConfigFile(path string) (*LlamaConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &LlamaConfig{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg LlamaConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := parseLlamaRawModels(&cfg, path); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadOpenAIConfig reads OpenAI config from a file path, falling back to
// OPENAI_BASE_URL / OPENAI_API_KEY env vars if the file is absent.
// Used by the fallback path when config.json doesn't exist.
func LoadOpenAIConfig(path string) (*OpenAIConfig, error) {
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			var cfg OpenAIConfig
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			normalizeOpenAI(&cfg)
			return &cfg, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	// Env var fallback.
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
		normalizeOpenAI(cfg)
		return cfg, nil
	}

	return &OpenAIConfig{}, nil
}

// normalizeOpenAI trims trailing slashes from base URLs and defaults Group to Name.
func normalizeOpenAI(cfg *OpenAIConfig) {
	for i := range cfg.Endpoints {
		cfg.Endpoints[i].BaseURL = strings.TrimRight(cfg.Endpoints[i].BaseURL, "/")
		if cfg.Endpoints[i].Group == "" {
			cfg.Endpoints[i].Group = cfg.Endpoints[i].Name
		}
	}
}

// WriteConfigPTY persists the pty section of config.json atomically. Other
// top-level sections (openai, llama-server, future unknown keys) are read as
// raw JSON and re-emitted untouched so this writer never loses data it doesn't
// understand. Values are stripped of their ID (it's the map key) and BuiltIn
// (computed at API time) before serialization.
func WriteConfigPTY(dataDir string, pty map[string]TerminalTemplate) error {
	configPath := filepath.Join(dataDir, "config.json")

	// Preserve unknown sections by reading into a raw map.
	raw := make(map[string]json.RawMessage)
	if data, err := os.ReadFile(configPath); err == nil {
		if len(data) > 0 {
			if err := json.Unmarshal(data, &raw); err != nil {
				return fmt.Errorf("parse %s: %w", configPath, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}

	// Strip ID (it's the map key) and BuiltIn (computed) before persisting.
	cleaned := make(map[string]TerminalTemplate, len(pty))
	for k, v := range pty {
		v.ID = ""
		v.BuiltIn = false
		cleaned[k] = v
	}

	ptyBytes, err := json.MarshalIndent(cleaned, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pty: %w", err)
	}
	raw["pty"] = ptyBytes

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(configPath), err)
	}

	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s: %w", tmpPath, err)
	}
	return nil
}
