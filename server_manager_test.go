package main

// Unit tests for the ServerManager helpers that don't need a real
// managed-server subprocess. Specifically:
//
//   - buildServerArgs: CLI flag translation (bool, number, string) + profile FixedArgs injection
//   - parseServerRawModels: alias extraction + modelDir path resolution
//   - ServerConfig.FindByAlias / ServerManager.HasAlias: lookups
//   - allocatePort: returns a port that bind() will accept
//   - expandHome: tilde substitution
//
// Process-lifecycle tests live in provider_llama_live_test.go (//go:build llm)
// because they require a real model.

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// ---------------------------------------------------------------------------
// buildServerArgs — CLI flag translation
// ---------------------------------------------------------------------------

func TestBuildServerArgs_PortAndHostAlwaysFirst(t *testing.T) {
	got := buildServerArgs(llamaProfile, map[string]any{}, 9090)
	if len(got) < 4 {
		t.Fatalf("expected at least port + host flags, got %v", got)
	}
	if got[0] != "--port" || got[1] != "9090" {
		t.Errorf("expected --port 9090 first; got %v", got[:2])
	}
	if got[2] != "--host" || got[3] != "127.0.0.1" {
		t.Errorf("expected --host 127.0.0.1 second; got %v", got[2:4])
	}
}

func TestBuildServerArgs_HostFromArgsOverridesDefault(t *testing.T) {
	got := buildServerArgs(llamaProfile, map[string]any{"host": "0.0.0.0"}, 8000)
	if !slices.Contains(got, "0.0.0.0") {
		t.Errorf("custom host missing: %v", got)
	}
	// The port/host pair in args shouldn't be re-emitted as a flag below.
	hostCount := 0
	for _, a := range got {
		if a == "--host" {
			hostCount++
		}
	}
	if hostCount != 1 {
		t.Errorf("--host emitted %d times, want 1: %v", hostCount, got)
	}
}

func TestBuildServerArgs_BoolTrueEmitsFlagBoolFalseOmits(t *testing.T) {
	got := buildServerArgs(llamaProfile, map[string]any{
		"flash-attn": true,
		"verbose":    false,
		"kv-unified": true,
	}, 8000)
	if !slices.Contains(got, "--flash-attn") {
		t.Errorf("--flash-attn missing for true bool: %v", got)
	}
	if !slices.Contains(got, "--kv-unified") {
		t.Errorf("--kv-unified missing for true bool: %v", got)
	}
	if slices.Contains(got, "--verbose") {
		t.Errorf("--verbose should be omitted for false bool: %v", got)
	}
}

func TestBuildServerArgs_IntegerAndFloat(t *testing.T) {
	// JSON numbers come in as float64; whole numbers must render without a
	// decimal point so the server's flag parser accepts them.
	got := buildServerArgs(llamaProfile, map[string]any{
		"ctx-size": float64(131072), // integer-valued
		"temp":     float64(0.6),    // fractional
		"top-p":    float64(0.95),
	}, 8000)

	mustFollow(t, got, "--ctx-size", "131072")
	mustFollow(t, got, "--temp", "0.6")
	mustFollow(t, got, "--top-p", "0.95")
}

func TestBuildServerArgs_StringValue(t *testing.T) {
	got := buildServerArgs(llamaProfile, map[string]any{
		"model":        "/models/qwen.gguf",
		"cache-type-k": "q8_0",
	}, 8000)
	mustFollow(t, got, "--model", "/models/qwen.gguf")
	mustFollow(t, got, "--cache-type-k", "q8_0")
}

func TestBuildServerArgs_DeterministicOrder(t *testing.T) {
	// Sort-by-key keeps logs comparable across runs. Two calls with the same
	// input should produce identical output.
	args := map[string]any{"zeta": "z", "alpha": "a", "beta": "b"}
	first := buildServerArgs(llamaProfile, args, 8000)
	second := buildServerArgs(llamaProfile, args, 8000)
	if !slices.Equal(first, second) {
		t.Errorf("output not deterministic:\n  first=%v\n  second=%v", first, second)
	}
	// Alpha should appear before zeta in the output.
	posAlpha := slices.Index(first, "--alpha")
	posZeta := slices.Index(first, "--zeta")
	if posAlpha < 0 || posZeta < 0 || posAlpha > posZeta {
		t.Errorf("expected alphabetical key order; got %v", first)
	}
}

// ---------------------------------------------------------------------------
// buildServerArgs — profile FixedArgs injection
// ---------------------------------------------------------------------------

func TestBuildServerArgs_MlxProfile_InjectsServeFlag(t *testing.T) {
	got := buildServerArgs(mlxProfile, map[string]any{
		"model": "/models/mistral",
	}, 9500)
	// --serve should appear right after --port/--host, before sorted map flags.
	if len(got) < 5 {
		t.Fatalf("too few args: %v", got)
	}
	if got[4] != "--serve" {
		t.Errorf("expected --serve at index 4 (after port/host); got %v", got)
	}
	// --model should come after --serve.
	mustFollow(t, got, "--model", "/models/mistral")
}

func TestBuildServerArgs_LlamaProfile_NoServeFlag(t *testing.T) {
	got := buildServerArgs(llamaProfile, map[string]any{
		"model": "/models/qwen.gguf",
	}, 8090)
	if slices.Contains(got, "--serve") {
		t.Errorf("llama profile should NOT inject --serve: %v", got)
	}
}

// ---------------------------------------------------------------------------
// parseServerRawModels — config translation
// ---------------------------------------------------------------------------

func TestParseServerRawModels_ExtractsAliasAndArgs(t *testing.T) {
	cfg := &ServerConfig{
		RawModels: []map[string]any{
			{
				"alias":    "qwen-8b",
				"model":    "/abs/Qwen3-8B.gguf",
				"ctx-size": 131072.0,
				"temp":     0.6,
			},
		},
	}
	if err := parseServerRawModels(cfg, "test"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Models) != 1 {
		t.Fatalf("Models count: got %d, want 1", len(cfg.Models))
	}
	m := cfg.Models[0]
	if m.Alias != "qwen-8b" {
		t.Errorf("Alias: %q", m.Alias)
	}
	if _, ok := m.Args["alias"]; ok {
		t.Errorf("alias should not leak into Args: %v", m.Args)
	}
	if m.Args["model"] != "/abs/Qwen3-8B.gguf" {
		t.Errorf("abs model path mangled: %v", m.Args["model"])
	}
}

func TestParseServerRawModels_RejectsMissingAlias(t *testing.T) {
	cfg := &ServerConfig{
		RawModels: []map[string]any{{"model": "/x.gguf"}}, // no alias
	}
	if err := parseServerRawModels(cfg, "test"); err == nil {
		t.Error("expected error for missing alias, got nil")
	}
}

func TestParseServerRawModels_ResolvesRelativeModelPathAgainstModelDir(t *testing.T) {
	cfg := &ServerConfig{
		ModelDir: "/opt/models",
		RawModels: []map[string]any{
			{"alias": "x", "model": "unsloth/Qwen3-8B.gguf"},
		},
	}
	if err := parseServerRawModels(cfg, "test"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := cfg.Models[0].Args["model"]
	want := "/opt/models/unsloth/Qwen3-8B.gguf"
	if got != want {
		t.Errorf("resolved path: got %q, want %q", got, want)
	}
}

func TestParseServerRawModels_AbsolutePathLeftAlone(t *testing.T) {
	cfg := &ServerConfig{
		ModelDir: "/opt/models",
		RawModels: []map[string]any{
			{"alias": "x", "model": "/elsewhere/model.gguf"},
		},
	}
	_ = parseServerRawModels(cfg, "test")
	if cfg.Models[0].Args["model"] != "/elsewhere/model.gguf" {
		t.Errorf("absolute path should not be reanchored: %v", cfg.Models[0].Args["model"])
	}
}

func TestParseServerRawModels_MmprojResolvedToo(t *testing.T) {
	cfg := &ServerConfig{
		ModelDir: "/opt/models",
		RawModels: []map[string]any{
			{"alias": "vision", "model": "v/m.gguf", "mmproj": "v/mm.gguf"},
		},
	}
	_ = parseServerRawModels(cfg, "test")
	if cfg.Models[0].Args["mmproj"] != "/opt/models/v/mm.gguf" {
		t.Errorf("mmproj not resolved: %v", cfg.Models[0].Args["mmproj"])
	}
}

// ---------------------------------------------------------------------------
// FindByAlias / HasAlias / Aliases — lookups
// ---------------------------------------------------------------------------

func TestServerConfig_FindByAlias_NilConfigSafe(t *testing.T) {
	var cfg *ServerConfig
	if cfg.FindByAlias("anything") != nil {
		t.Error("nil-receiver FindByAlias should return nil, not panic")
	}
}

func TestServerConfig_FindByAlias_HitAndMiss(t *testing.T) {
	cfg := &ServerConfig{
		Models: []ServerModelConfig{
			{Alias: "a"}, {Alias: "b"}, {Alias: "c"},
		},
	}
	if cfg.FindByAlias("b") == nil {
		t.Error("FindByAlias miss for existing alias")
	}
	if cfg.FindByAlias("nope") != nil {
		t.Error("FindByAlias should return nil for missing alias")
	}
}

func TestServerManager_HasAlias_NilSafeAndMatching(t *testing.T) {
	var m *ServerManager
	if m.HasAlias("anything") {
		t.Error("nil-receiver HasAlias should return false")
	}

	cfg := &ServerConfig{
		Models: []ServerModelConfig{{Alias: "qwen-8b"}, {Alias: "qwen-30b"}},
	}
	mgr := NewServerManager(llamaProfile, cfg, "")
	if !mgr.HasAlias("qwen-8b") {
		t.Error("HasAlias should return true for configured alias")
	}
	if mgr.HasAlias("nonexistent") {
		t.Error("HasAlias should return false for unknown alias")
	}
}

func TestServerManager_Aliases_ReturnsAll(t *testing.T) {
	cfg := &ServerConfig{
		Models: []ServerModelConfig{{Alias: "m1"}, {Alias: "m2"}},
	}
	mgr := NewServerManager(llamaProfile, cfg, "")
	got := mgr.Aliases()
	if len(got) != 2 || !slices.Contains(got, "m1") || !slices.Contains(got, "m2") {
		t.Errorf("Aliases: got %v, want [m1 m2]", got)
	}
}

// ---------------------------------------------------------------------------
// allocatePort — actually binds to verify the port is usable
// ---------------------------------------------------------------------------

func TestServerManager_AllocatePort_ReturnsBindablePort(t *testing.T) {
	mgr := NewServerManager(llamaProfile, &ServerConfig{BasePort: 18000}, "")
	port := mgr.allocatePort()
	if port == 0 {
		t.Fatal("allocatePort returned 0")
	}
	// We should be able to bind it right after allocation (TOCTOU is fine
	// here — we're proving the picker hands out plausible ports, not that
	// the OS guarantees it across calls).
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Errorf("port %d not bindable: %v", port, err)
	} else {
		ln.Close()
	}
}

func TestServerManager_AllocatePort_AdvancesPastBoundPort(t *testing.T) {
	mgr := NewServerManager(llamaProfile, &ServerConfig{BasePort: 18100}, "")
	// Pre-bind 18100 to force the allocator to skip it.
	blocker, err := net.Listen("tcp", "127.0.0.1:18100")
	if err != nil {
		t.Skip("port 18100 already in use on this host; skipping")
	}
	defer blocker.Close()

	port := mgr.allocatePort()
	if port == 18100 {
		t.Errorf("allocator returned a port we already bound: %d", port)
	}
	if port == 0 {
		t.Errorf("allocator returned 0")
	}
}

// ---------------------------------------------------------------------------
// preflightPortFree — regression for the "external process holds the port"
// silent-failure mode that the live tests originally surfaced
// ---------------------------------------------------------------------------

func TestPreflightPortFree_FailsWhenPortHeld(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if err := preflightPortFree(port); err == nil {
		t.Errorf("expected preflight to fail when port %d is held; got nil", port)
	}
}

func TestPreflightPortFree_SucceedsForFreePort(t *testing.T) {
	// Bind+release to get a port that was free *just now*.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	if err := preflightPortFree(port); err != nil {
		t.Errorf("preflight on (just-released) free port: %v", err)
	}
}

// ---------------------------------------------------------------------------
// expandHome
// ---------------------------------------------------------------------------

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	cases := map[string]string{
		"":         "",
		"~":        home,
		"~/models": filepath.Join(home, "models"),
		"/abs":     "/abs",
		"rel/path": "rel/path",
	}
	for in, want := range cases {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// parseUnifiedConfig — mlx-serve section parsing
// ---------------------------------------------------------------------------

func TestParseUnifiedConfig_MlxServeSection(t *testing.T) {
	data := []byte(`{
		"mlx-serve": {
			"modelDir": "/base",
			"models": [
				{"alias": "q4", "model": "sub/dir", "temp": 0.7}
			]
		}
	}`)

	cfg, err := parseUnifiedConfig(data, "test.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Mlx section should be parsed.
	if cfg.Mlx == nil {
		t.Fatal("cfg.Mlx is nil")
	}
	if len(cfg.Mlx.Models) != 1 {
		t.Fatalf("Mlx models count: got %d, want 1", len(cfg.Mlx.Models))
	}
	m := cfg.Mlx.Models[0]
	if m.Alias != "q4" {
		t.Errorf("alias: got %q, want q4", m.Alias)
	}
	// Relative model path should be resolved against modelDir.
	if m.Args["model"] != "/base/sub/dir" {
		t.Errorf("model: got %v, want /base/sub/dir", m.Args["model"])
	}
	// temp arg should be preserved as a float64 from JSON.
	if v, ok := m.Args["temp"].(float64); !ok || v != 0.7 {
		t.Errorf("temp: got %v, want 0.7", m.Args["temp"])
	}

	// Llama section absent → empty non-nil config.
	if cfg.Llama == nil {
		t.Fatal("cfg.Llama should be non-nil even when absent")
	}
	if len(cfg.Llama.Models) != 0 {
		t.Errorf("Llama models count: got %d, want 0", len(cfg.Llama.Models))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mustFollow asserts that `flag` appears in args followed immediately by `value`.
func mustFollow(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) && args[i+1] == value {
				return
			}
			t.Errorf("flag %q followed by %q, want %q", flag, args[i+1], value)
			return
		}
	}
	t.Errorf("flag %q not found in args %v", flag, args)
}
