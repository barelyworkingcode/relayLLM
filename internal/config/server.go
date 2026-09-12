package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ServerProfile parameterizes a managed-server process manager for a specific
// binary. Kind doubles as the model routing prefix ("{kind}/{alias}"), the
// types.ModelInfo.Provider string, and the log/error prefix.
type ServerProfile struct {
	Kind            string   // "llama" | "mlx"
	DefaultBinary   string   // PATH fallback when config/flag give no path
	Group           string   // Eve UI model group label
	FixedArgs       []string // injected after --port/--host, before per-model flags (e.g. --serve)
	DefaultBasePort int
}

// ServerModelConfig describes one managed-server model. Alias is the routing
// name (users select "{kind}/{alias}"). Args holds every other key from the
// JSON entry — each maps 1:1 to a CLI flag.
type ServerModelConfig struct {
	Alias string
	Args  map[string]any // key → value, translated to --key [value]
}

// ServerConfig is the top-level config structure for a managed-server section
// (llama-server or mlx-serve).
type ServerConfig struct {
	BinaryPath string              `json:"binaryPath,omitempty"`
	ModelDir   string              `json:"modelDir,omitempty"` // prepended to relative model paths
	BasePort   int                 `json:"basePort,omitempty"`
	Models     []ServerModelConfig `json:"-"` // custom unmarshal
	RawModels  []map[string]any    `json:"models"`

	// Resource budget. All optional; zero means "no limit" for the caps and
	// "no reclaim" for the idle timeout, which is the pre-budget behavior.
	//
	// MaxLoaded caps instance count; MaxMemoryGB caps the sum of estimated
	// resident memory (see server_memory.go). Either can trigger eviction —
	// the count cap is exact but blunt, the memory cap tracks the fact that
	// two loaded models can differ by 6x. Models whose size cannot be
	// estimated count toward MaxLoaded but not MaxMemoryGB.
	MaxLoaded          int     `json:"maxLoaded,omitempty"`
	MaxMemoryGB        float64 `json:"maxMemoryGB,omitempty"`
	IdleTimeoutMinutes int     `json:"idleTimeoutMinutes,omitempty"`

	// MemoryHeadroomPercent pads each model's estimate to cover compute
	// buffers and allocator slack, which are not modelled directly.
	// Defaults to defaultMemoryHeadroomPercent.
	MemoryHeadroomPercent int `json:"memoryHeadroomPercent,omitempty"`

	// AdmissionTimeoutSeconds bounds how long a request waits for a busy
	// instance to go idle when the budget is full. Defaults to
	// defaultAdmissionTimeout.
	AdmissionTimeoutSeconds int `json:"admissionTimeoutSeconds,omitempty"`
}

// FindByAlias returns the config for the given alias, or nil.
func (c *ServerConfig) FindByAlias(alias string) *ServerModelConfig {
	if c == nil {
		return nil
	}
	for i := range c.Models {
		if c.Models[i].Alias == alias {
			return &c.Models[i]
		}
	}
	return nil
}

// ParseServerRawModels converts RawModels entries into typed ServerModelConfig
// values. Each raw entry must have an "alias" key; all other keys become Args.
// If modelDir is set, relative "model" paths are resolved against it.
func ParseServerRawModels(cfg *ServerConfig, source string) error {
	modelDir := ExpandHome(cfg.ModelDir)

	for i, raw := range cfg.RawModels {
		alias, _ := raw["alias"].(string)
		if alias == "" {
			return fmt.Errorf("parse %s: models[%d] missing \"alias\"", source, i)
		}
		args := make(map[string]any, len(raw)-1)
		for k, v := range raw {
			if k == "alias" {
				continue
			}
			args[k] = v
		}
		// Resolve relative file paths against modelDir.
		if modelDir != "" {
			for _, k := range []string{"model", "mmproj"} {
				if v, ok := args[k].(string); ok && !filepath.IsAbs(v) {
					args[k] = filepath.Join(modelDir, v)
				}
			}
		}
		cfg.Models = append(cfg.Models, ServerModelConfig{Alias: alias, Args: args})
	}
	cfg.RawModels = nil
	return nil
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "~/") || path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}
