package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Resident-memory estimation for managed-server models.
//
// The budget in ServerManager needs a per-model weight before the model is
// ever launched, so estimation reads metadata off disk rather than measuring a
// running process. Two terms dominate:
//
//	weights — the model file(s). llama.cpp mmaps the GGUF, and with
//	          n-gpu-layers -1 (plus mlock) all of it is resident, so the file
//	          size on disk is exact rather than approximate.
//	kv      — the KV cache, a function of context length, per-layer attention
//	          geometry, and the cache quantization. Computed from the GGUF
//	          header (see ggufKVCacheBytes) or, for MLX, from config.json.
//
// What is deliberately not modelled is the compute/graph buffer, which scales
// with ubatch-size and the graph's widest node. Rather than pretend to compute
// it, a flat headroom fraction is applied on top. A per-model memoryGB in
// settings.json overrides the whole estimate when the guess is wrong.

const bytesPerGB = 1024 * 1024 * 1024

// defaultMemoryHeadroomPercent pads every estimate to cover compute buffers,
// allocator slack, and the server process itself.
const defaultMemoryHeadroomPercent = 10

// estimateModelMemory returns the estimated resident bytes for one configured
// model. A zero return means "unknown" — the caller must not treat that as
// "free", but as "cannot be budgeted by size".
//
// Precedence: an explicit memoryGB in the model's config wins outright (and
// skips headroom, since a hand-set number is taken at face value). Otherwise
// the profile-specific estimator runs and headroom is applied.
func estimateModelMemory(profile ServerProfile, cfg ServerModelConfig, headroomPercent int) int64 {
	if gb, ok := numericArg(cfg.Args, "memoryGB"); ok && gb > 0 {
		return int64(gb * bytesPerGB)
	}

	modelPath, _ := cfg.Args["model"].(string)
	if modelPath == "" {
		slog.Warn("memory estimate: model entry has no \"model\" path",
			"kind", profile.Kind, "alias", cfg.Alias)
		return 0
	}

	var weights, kv int64
	var err error
	switch profile.Kind {
	case "mlx":
		weights, kv, err = estimateMLXMemory(modelPath, cfg)
	default:
		weights, kv, err = estimateLlamaMemory(modelPath, cfg)
	}
	if err != nil {
		slog.Warn("memory estimate unavailable; model will not count against the size budget",
			"kind", profile.Kind, "alias", cfg.Alias, "error", err)
		return 0
	}

	if headroomPercent <= 0 {
		headroomPercent = defaultMemoryHeadroomPercent
	}
	total := weights + kv
	return total + total*int64(headroomPercent)/100
}

// estimateLlamaMemory sizes a GGUF model: the file itself plus its KV cache at
// the configured ctx-size and cache quantization.
func estimateLlamaMemory(modelPath string, cfg ServerModelConfig) (weights, kv int64, err error) {
	info, err := os.Stat(modelPath)
	if err != nil {
		return 0, 0, fmt.Errorf("stat model: %w", err)
	}
	weights = info.Size()

	// Sharded models ship as name-00001-of-000NN.gguf; the header lives in the
	// first shard but the weights are the sum of all of them.
	if shards, shardErr := ggufShardSiblings(modelPath); shardErr == nil && len(shards) > 1 {
		weights = 0
		for _, s := range shards {
			if si, statErr := os.Stat(s); statErr == nil {
				weights += si.Size()
			}
		}
	}

	md, err := ReadGGUFMetadata(modelPath)
	if err != nil {
		return weights, 0, err
	}

	ctx := int64(0)
	if v, ok := numericArg(cfg.Args, "ctx-size"); ok {
		ctx = int64(v)
	}
	if ctx <= 0 {
		// llama-server defaults to the model's trained context when unset.
		ctx, _ = md.archInt("context_length")
	}
	if ctx <= 0 {
		ctx = 4096
	}

	cacheK, _ := cfg.Args["cache-type-k"].(string)
	cacheV, _ := cfg.Args["cache-type-v"].(string)

	kv, err = ggufKVCacheBytes(md, ctx, cacheK, cacheV)
	if err != nil {
		return weights, 0, err
	}
	return weights, kv, nil
}

// ggufShardSiblings returns every shard of a multi-part GGUF, given any one of
// them. Returns just the input path when the name is not a shard.
func ggufShardSiblings(modelPath string) ([]string, error) {
	base := filepath.Base(modelPath)
	idx := strings.LastIndex(base, "-of-")
	if idx < 0 {
		return []string{modelPath}, nil
	}
	// "name-00001-of-00006.gguf" -> prefix "name-", suffix "-of-00006.gguf"
	head := base[:idx]
	dash := strings.LastIndex(head, "-")
	if dash < 0 {
		return []string{modelPath}, nil
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(modelPath), head[:dash+1]+"*"+base[idx:]))
	if err != nil || len(matches) == 0 {
		return []string{modelPath}, nil
	}
	return matches, nil
}

// mlxConfig is the subset of a Hugging Face config.json needed to size a KV
// cache. MLX model directories ship the original config alongside the weights.
type mlxConfig struct {
	NumHiddenLayers   int `json:"num_hidden_layers"`
	NumKeyValueHeads  int `json:"num_key_value_heads"`
	NumAttentionHeads int `json:"num_attention_heads"`
	HiddenSize        int `json:"hidden_size"`
	HeadDim           int `json:"head_dim"`
	MaxPositions      int `json:"max_position_embeddings"`
}

// estimateMLXMemory sizes an MLX model directory: the sum of its weight shards
// plus a KV cache derived from config.json. MLX serves at f16 KV.
func estimateMLXMemory(modelDir string, cfg ServerModelConfig) (weights, kv int64, err error) {
	info, err := os.Stat(modelDir)
	if err != nil {
		return 0, 0, fmt.Errorf("stat model dir: %w", err)
	}
	if !info.IsDir() {
		return info.Size(), 0, nil
	}

	err = filepath.WalkDir(modelDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".safetensors", ".npz", ".bin", ".gguf":
			if fi, statErr := d.Info(); statErr == nil {
				weights += fi.Size()
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("walk model dir: %w", err)
	}
	if weights == 0 {
		return 0, 0, fmt.Errorf("no weight files found in %s", modelDir)
	}

	raw, err := os.ReadFile(filepath.Join(modelDir, "config.json"))
	if err != nil {
		// Weights alone are still a useful budget signal.
		return weights, 0, nil
	}
	var mc mlxConfig
	if json.Unmarshal(raw, &mc) != nil || mc.NumHiddenLayers == 0 {
		return weights, 0, nil
	}

	headDim := mc.HeadDim
	if headDim == 0 && mc.NumAttentionHeads > 0 {
		headDim = mc.HiddenSize / mc.NumAttentionHeads
	}
	kvHeads := mc.NumKeyValueHeads
	if kvHeads == 0 {
		kvHeads = mc.NumAttentionHeads
	}
	if headDim == 0 || kvHeads == 0 {
		return weights, 0, nil
	}

	ctx := 0
	if v, ok := numericArg(cfg.Args, "ctx-size"); ok {
		ctx = int(v)
	}
	if ctx <= 0 {
		ctx = mc.MaxPositions
	}
	if ctx <= 0 {
		ctx = 4096
	}

	// K and V, f16, across every layer.
	kv = int64(ctx) * int64(mc.NumHiddenLayers) * int64(kvHeads) * int64(headDim) * 2 * 2
	return weights, kv, nil
}

// modelTrainedContext returns the context length the model was trained with,
// read from its metadata. Used as the n_ctx_train figure in catalog listings
// so a model with no explicit ctx-size still reports a real number instead of
// letting clients fall back to a generic default.
//
// Returns 0 when the metadata cannot be read; callers omit the field rather
// than substituting a guess.
func modelTrainedContext(profile ServerProfile, cfg ServerModelConfig) int64 {
	modelPath, _ := cfg.Args["model"].(string)
	if modelPath == "" {
		return 0
	}

	if profile.Kind == "mlx" {
		raw, err := os.ReadFile(filepath.Join(modelPath, "config.json"))
		if err != nil {
			return 0
		}
		var mc mlxConfig
		if json.Unmarshal(raw, &mc) != nil {
			return 0
		}
		return int64(mc.MaxPositions)
	}

	md, err := ReadGGUFMetadata(modelPath)
	if err != nil {
		return 0
	}
	trained, _ := md.archInt("context_length")
	return trained
}

// numericArg reads a JSON-decoded numeric config value. Values arrive as
// float64 from encoding/json, but hand-built test configs may use int.
func numericArg(args map[string]any, key string) (float64, bool) {
	switch v := args[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// formatGB renders bytes as a human-readable GB string for logs and API output.
func formatGB(b int64) string {
	return fmt.Sprintf("%.2fGB", float64(b)/bytesPerGB)
}
