package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/jsonc"
)

// LoadedConfig bundles every section LoadConfig can produce. The config
// sections are non-nil after a successful load (empty configs, not nil);
// PTY is the raw settings.json `pty` section and may be nil when absent —
// seeding happens in TemplateStore.Load, not here.
type LoadedConfig struct {
	OpenAI  *OpenAIConfig
	Virtual *VirtualLLMConfig
	Llama   *ServerConfig
	Mlx     *ServerConfig
	Pi      *PiConfig
	PTY     map[string]TerminalTemplate
}

// PiConfig configures the pi.dev coding-agent provider. All fields optional.
// Empty BinaryPath falls back to the well-known fallback chain in
// resolvePiPath (~/.local/bin/pi, npm globals, /usr/local/bin/pi, then PATH).
//
// The relay-managed fields (UseRelayToken / EnvPassthrough) mirror the pidev
// PTY template's shape and trigger the same RelayManagedSpec.Resolve() prep
// used by terminal_session.go — see relay_spawn.go. When set, each LLM-pi
// spawn calls relay's bridge to fetch a project-scoped token and forwards
// listed env vars from the relayLLM process into pi. Skills load from the
// project's .claude/skills directory (relay generates and manages them).
type PiConfig struct {
	BinaryPath string   `json:"binaryPath,omitempty"`
	ExtraArgs  []string `json:"extraArgs,omitempty"` // appended to every pi spawn after the standard args

	UseRelayToken  bool     `json:"useRelayToken,omitempty"`
	EnvPassthrough []string `json:"env_passthrough,omitempty"` // env keys copied from os.Environ() into pi

	// ProjectOverlay opts a relay-managed project into a per-project pi
	// config dir at <projectDir>/.pi/. When enabled, relayLLM materializes
	// models.json + settings.json reflecting our curated providers and
	// symlinks auth.json back to the user's global ~/.pi/agent/auth.json,
	// then spawns pi with PI_CODING_AGENT_DIR pointing at the overlay.
	// Pi's global ~/.pi/agent/ is never written to. See pi_overlay.go.
	ProjectOverlay PiProjectOverlay `json:"projectOverlay,omitempty"`
}

// PiAuthStrategy* values for PiProjectOverlay.AuthStrategy.
const (
	PiAuthStrategySymlink = "symlink"
	PiAuthStrategyNone    = "none"
)

// PiProjectOverlay controls per-project pi config materialization.
// Mode == "never" (default) disables the feature entirely.
type PiProjectOverlay struct {
	Mode            string   `json:"mode,omitempty"`            // "always" | "skipIfExists" | "never" (default "never")
	DirName         string   `json:"dirName,omitempty"`         // default ".pi"
	AuthStrategy    string   `json:"authStrategy,omitempty"`    // "symlink" | "none" (default "symlink")
	DefaultProvider string   `json:"defaultProvider,omitempty"` // e.g. "relay-router"
	DefaultModel    string   `json:"defaultModel,omitempty"`    // e.g. "qwen3-8b"
	DefaultThinking string   `json:"defaultThinking,omitempty"` // off | minimal | low | medium | high | xhigh
	ExtraSkillDirs  []string `json:"extraSkillDirs,omitempty"`  // appended to skills array
	Gitignore       bool     `json:"gitignore,omitempty"`       // auto-append overlay dir to project .gitignore

	// Negative-semantics bools: zero-value = "merge user's global config in".
	// Set to true only to fully isolate the overlay from the user's existing
	// global pi setup.
	ExcludeUserProviders bool `json:"excludeUserProviders,omitempty"` // skip merging ~/.pi/agent/models.json providers
	ExcludeUserSettings  bool `json:"excludeUserSettings,omitempty"`  // skip basing settings.json on ~/.pi/agent/settings.json

	// ExcludeProviders names specific providers from the user's global
	// models.json to drop on the merge. Use when our overlay supersedes a
	// user-defined provider (e.g. they registered "llama-cpp" pointing at
	// individual llama-server ports and our "relay-router" now covers
	// the same models). Less invasive than ExcludeUserProviders, which
	// strips everything.
	ExcludeProviders []string `json:"excludeProviders,omitempty"`
}

// Enabled reports whether the overlay should be materialized for a spawn.
func (o PiProjectOverlay) Enabled() bool {
	return o.Mode != "" && o.Mode != AutoRegenNever
}

// LoadConfig loads provider configuration. It tries sources in order:
//
//  1. {dataDir}/settings.json — unified config (preferred)
//  2. Separate files — {dataDir}/openai_endpoints.json, {dataDir}/llama_models.json
//  3. Environment variables — OPENAI_BASE_URL / OPENAI_API_KEY (OpenAI only)
//
// The openaiConfigOverride flag (--openai-config) bypasses all of the above
// for the OpenAI section and reads that file directly.
//
// The returned LoadedConfig bundles every section; all fields are non-nil.
func LoadConfig(dataDir string, openaiConfigOverride string) (*LoadedConfig, error) {
	configPath := filepath.Join(dataDir, "settings.json")

	// Try unified settings.json first.
	data, err := os.ReadFile(configPath)
	if err == nil {
		cfg, err := parseUnifiedConfig(data, configPath)
		if err != nil {
			return nil, err // parse error — don't silently fall back
		}

		// --openai-config flag overrides the unified config's openai section.
		if openaiConfigOverride != "" {
			if override, err := loadOpenAIConfigFile(openaiConfigOverride); err == nil {
				cfg.OpenAI = override
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		}

		slog.Info("loaded settings.json", "path", configPath)
		return cfg, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", configPath, err)
	}

	// settings.json not found — fall back to separate files + env vars.
	openaiPath := openaiConfigOverride
	if openaiPath == "" {
		openaiPath = filepath.Join(dataDir, "openai_endpoints.json")
	}
	openaiCfg, err := LoadOpenAIConfig(openaiPath)
	if err != nil {
		return nil, err
	}

	llamaCfg, err := loadLlamaConfigFile(filepath.Join(dataDir, "llama_models.json"))
	if err != nil {
		return nil, err
	}

	return &LoadedConfig{
		OpenAI:  openaiCfg,
		Virtual: &VirtualLLMConfig{},
		Llama:   llamaCfg,
		Mlx:     &ServerConfig{},
		Pi:      &PiConfig{},
	}, nil
}

// parseUnifiedConfig parses the unified settings.json into a LoadedConfig.
// Strips JSONC comments (// and /* */) before unmarshalling so users can
// toggle sections with comment blocks. Comments don't survive writes.
func parseUnifiedConfig(data []byte, source string) (*LoadedConfig, error) {
	// Use a raw intermediate so llama-server's and mlx-serve's model entries
	// stay as map[string]any for the generic CLI flag translation.
	var raw struct {
		OpenAI      *OpenAIConfig               `json:"openai"`
		VirtualLLMs *VirtualLLMConfig           `json:"virtual-llms"`
		LlamaServer *json.RawMessage            `json:"llama-server"`
		MlxServer   *json.RawMessage            `json:"mlx-serve"`
		Pi          *PiConfig                   `json:"pi"`
		PTY         map[string]TerminalTemplate `json:"pty"`
	}
	if err := json.Unmarshal(jsonc.ToJSON(data), &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", source, err)
	}

	openaiCfg := &OpenAIConfig{}
	if raw.OpenAI != nil {
		openaiCfg = raw.OpenAI
		normalizeOpenAI(openaiCfg)
	}
	virtualCfg := &VirtualLLMConfig{}
	if raw.VirtualLLMs != nil {
		virtualCfg = raw.VirtualLLMs
	}

	llamaCfg := &ServerConfig{}
	if raw.LlamaServer != nil {
		if err := json.Unmarshal(*raw.LlamaServer, llamaCfg); err != nil {
			return nil, fmt.Errorf("parse %s llama-server: %w", source, err)
		}
		if err := parseServerRawModels(llamaCfg, source); err != nil {
			return nil, err
		}
	}

	mlxCfg := &ServerConfig{}
	if raw.MlxServer != nil {
		if err := json.Unmarshal(*raw.MlxServer, mlxCfg); err != nil {
			return nil, fmt.Errorf("parse %s mlx-serve: %w", source, err)
		}
		if err := parseServerRawModels(mlxCfg, source); err != nil {
			return nil, err
		}
	}

	piCfg := &PiConfig{}
	if raw.Pi != nil {
		piCfg = raw.Pi
		piCfg.BinaryPath = expandHome(piCfg.BinaryPath)
	}

	return &LoadedConfig{
		OpenAI:  openaiCfg,
		Virtual: virtualCfg,
		Llama:   llamaCfg,
		Mlx:     mlxCfg,
		Pi:      piCfg,
		PTY:     raw.PTY,
	}, nil
}

// readJSONCFile reads path and unmarshals into out, stripping JSONC comments
// first so users can hand-edit with comment toggles. The raw os.ReadFile error
// is returned unwrapped (callers use os.IsNotExist); parse errors are wrapped
// with the source path since json errors don't include it.
func readJSONCFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(jsonc.ToJSON(data), out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// loadOpenAIConfigFile reads an OpenAI config from a specific file path.
// Does not fall back to env vars.
func loadOpenAIConfigFile(path string) (*OpenAIConfig, error) {
	var cfg OpenAIConfig
	if err := readJSONCFile(path, &cfg); err != nil {
		return nil, err
	}
	normalizeOpenAI(&cfg)
	return &cfg, nil
}

// loadLlamaConfigFile reads a standalone llama_models.json file.
func loadLlamaConfigFile(path string) (*ServerConfig, error) {
	var cfg ServerConfig
	if err := readJSONCFile(path, &cfg); err != nil {
		if os.IsNotExist(err) {
			return &ServerConfig{}, nil
		}
		return nil, err
	}
	if err := parseServerRawModels(&cfg, path); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadOpenAIConfig reads OpenAI config from a file path, falling back to
// OPENAI_BASE_URL / OPENAI_API_KEY env vars if the file is absent.
// Used by the fallback path when settings.json doesn't exist.
func LoadOpenAIConfig(path string) (*OpenAIConfig, error) {
	if path != "" {
		var cfg OpenAIConfig
		err := readJSONCFile(path, &cfg)
		if err == nil {
			normalizeOpenAI(&cfg)
			return &cfg, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
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

// WriteConfigPTY persists the pty section of settings.json atomically. Other
// top-level sections (openai, llama-server, future unknown keys) are read as
// raw JSON and re-emitted untouched so this writer never loses data it doesn't
// understand. Values are stripped of their ID (it's the map key) and BuiltIn
// (computed at API time) before serialization.
func WriteConfigPTY(dataDir string, pty map[string]TerminalTemplate) error {
	configPath := filepath.Join(dataDir, "settings.json")

	// Preserve unknown sections by reading into a raw map. Strip JSONC
	// comments first — they'll be erased anyway since we re-marshal below,
	// but parsing must not fail when the user has comment blocks in place.
	raw := make(map[string]json.RawMessage)
	if data, err := os.ReadFile(configPath); err == nil {
		if len(data) > 0 {
			if err := json.Unmarshal(jsonc.ToJSON(data), &raw); err != nil {
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
