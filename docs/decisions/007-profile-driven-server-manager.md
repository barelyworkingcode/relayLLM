# 007 — Profile-driven ServerManager (mlx-serve as a second managed provider)

**Status**: accepted (2026-06-10)

## Context

We wanted a native MLX provider for Apple Silicon: relayLLM launches an
inference server per configured model on demand, exactly like the llama.cpp
provider. Candidates for the server binary:

- `mlx_lm.server` (Apple's own) — Python; needs a pip/uv-managed environment.
- SwiftLM — Swift-native, but no Homebrew distribution; manual tarball/source builds.
- **mlx-serve** (ddalcu/mlx-serve) — Zig + mlx-c FFI, zero Python. OpenAI-compatible
  `/v1/chat/completions` with SSE, `GET /health`, accepts local MLX model
  directories, and its CLI flags follow the same `--key value` convention as
  llama-server. Distributed via Homebrew tap *and* pre-built arm64 release
  tarballs (the latter matters: the brew formula builds from source and pins a
  minimum Xcode, which a beta-OS machine may not satisfy).

`LlamaServerManager` was already ~95% generic: a config of
`alias + map[string]any` args translated 1:1 to CLI flags, lazy launch with
per-alias locking, port allocation, `/health` polling, instance
listing/stopping. The only llama-specific parts were the binary name, the
`llama/` routing prefix, the UI group label, and the base port.

## Decision

Generalize instead of duplicate. `llama_manager.go` became `server_manager.go`;
`LlamaServerManager` became `ServerManager`, parameterized by a `ServerProfile`:

```go
type ServerProfile struct {
    Kind            string   // "llama" | "mlx" — routing prefix, Provider string, log prefix
    DefaultBinary   string   // PATH fallback ("llama-server" | "mlx-serve")
    Group           string   // Eve UI model group ("llama.cpp" | "MLX")
    FixedArgs       []string // always-on args (mlx-serve needs "--serve")
    DefaultBasePort int      // 8090 | 9400
}
```

Everything else stays shared: `ServerConfig`/`ServerModelConfig` parsing
(`llama-server` and `mlx-serve` settings.json sections have identical shapes),
launch/health/stop lifecycle, and the OpenAI transport the sessions ride on.

Knock-on integrations, all additive:

- **Sessions**: `mlx/{alias}` derives provider type `"mlx"`; `initProvider`
  handles `"llama", "mlx"` in one case selecting the right manager.
- **relay-router**: takes `[]*ServerManager` in priority order (llama first,
  then mlx) — first `HasAlias` match wins, so llama keeps winning alias
  collisions, matching the existing llama-vs-endpoint rule. Bare mlx aliases
  are router-routable and flow into the pi project overlay's model snapshot.
- **Status/manifest**: `/api/status` gains a `mlxInstances` array (separate
  from `instances` because the manifest's `ForEach` binds an action to one
  array — see ADR-005); new `stop-mlx` action targets the new
  `DELETE /api/mlx/instances/{alias}` route. Settings schema gains an
  `mlx-serve` section mirroring `llama-server`.

## Consequences

- Adding a third managed server (vLLM-style, future MLX forks) is a profile
  var + a config section, not another 500-line manager.
- The `ServerInstanceInfo` JSON shape is shared and unchanged, so relay's
  Service Inspector renders mlx rows with zero relay-side changes.
- mlx-serve is installed out-of-band (pre-built release tarball at
  `~/.local/mlx-serve/`, referenced via `binaryPath`); relayLLM only ever
  spawns the configured binary — same trust model as llama-server.
- The `mmproj` attachment detection is llama-specific but harmless under the
  mlx profile (mlx configs simply don't carry that key). Revisit if mlx
  vision models need per-model attachment flags.
