package main

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const piModelsCacheTTL = 5 * time.Minute

type piModelsCache struct {
	mu        sync.Mutex
	models    []ModelInfo
	expiresAt time.Time
	binPath   string
}

var piModels piModelsCache

// FetchPiModels returns the list of models offered by the pi provider,
// wrapped as relayLLM ModelInfo entries with the `pi/<provider>/<modelId>`
// value convention.
//
// When the project overlay is enabled, the listing reflects what pi will
// actually have access to in a spawned RPC session: providers in
// ExcludeProviders are filtered out (otherwise users would pick a model
// that pi rejects on launch), and a synthetic `relay-llama` entry per
// llama-server model is added (pi would otherwise not see those until
// after spawn-time overlay materialization).
//
// The upstream `pi --list-models` exec is cached for 5 minutes; the
// filter/augment pass runs on every call (cheap) so overlay changes take
// effect immediately.
//
// If pi is not on PATH, returns nil — the /api/models endpoint silently
// drops the pi section.
func FetchPiModels(ctx context.Context, piCfg *PiConfig, inputs PiOverlayInputs) []ModelInfo {
	var configuredPath string
	if piCfg != nil {
		configuredPath = piCfg.BinaryPath
	}
	raw := fetchPiListModelsCached(ctx, configuredPath)

	if piCfg == nil || !piCfg.ProjectOverlay.Enabled() {
		return raw
	}
	return applyPiOverlayToModelList(raw, piCfg.ProjectOverlay, inputs)
}

// fetchPiListModelsCached runs `pi --list-models` (with TTL caching) and
// parses the output into ModelInfo entries. Returns nil if pi is missing or
// the exec fails — callers degrade gracefully.
func fetchPiListModelsCached(ctx context.Context, configuredPath string) []ModelInfo {
	piPath := resolvePiPath(configuredPath)

	piModels.mu.Lock()
	if !piModels.expiresAt.IsZero() &&
		time.Now().Before(piModels.expiresAt) &&
		piModels.binPath == piPath {
		cached := piModels.models
		piModels.mu.Unlock()
		return cached
	}
	piModels.mu.Unlock()

	if _, err := os.Stat(piPath); err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, piPath, "--list-models")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		slog.Warn("pi --list-models failed", "error", err)
		return nil
	}

	models := parsePiListModels(out.Bytes())

	piModels.mu.Lock()
	piModels.models = models
	piModels.expiresAt = time.Now().Add(piModelsCacheTTL)
	piModels.binPath = piPath
	piModels.mu.Unlock()

	return models
}

// applyPiOverlayToModelList drops providers the overlay excludes and appends
// overlay-added providers (currently: relay-llama proxy entries). Keeps
// ordering stable so the UI picker stays predictable.
func applyPiOverlayToModelList(raw []ModelInfo, overlay PiProjectOverlay, inputs PiOverlayInputs) []ModelInfo {
	excluded := make(map[string]bool, len(overlay.ExcludeProviders))
	for _, name := range overlay.ExcludeProviders {
		excluded[name] = true
	}

	out := make([]ModelInfo, 0, len(raw)+len(inputs.LlamaModels))
	for _, m := range raw {
		if provider := piProviderFromValue(m.Value); provider != "" && excluded[provider] {
			continue
		}
		out = append(out, m)
	}

	// relay-llama: one synthetic entry per llama-server model. Pi won't list
	// these via --list-models (they only exist in the per-project overlay
	// materialized at spawn time), so we add them ourselves so the picker
	// reflects what the user can actually pick.
	if inputs.LlamaProxyPort != "" {
		for _, llama := range inputs.LlamaModels {
			value := "pi/" + piRelayLlamaProvider + "/" + llama.Alias
			out = append(out, ModelInfo{
				Label:    value,
				Value:    value,
				Group:    "Pi · " + piRelayLlamaProvider,
				Provider: "pi",
			})
		}
	}
	return out
}

// piProviderFromValue extracts the provider segment from a `pi/<provider>/<model>`
// value string. Returns "" for malformed values rather than misclassifying.
func piProviderFromValue(value string) string {
	const prefix = "pi/"
	if len(value) <= len(prefix) || value[:len(prefix)] != prefix {
		return ""
	}
	rest := value[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return rest[:i]
		}
	}
	return ""
}

// parsePiListModels scans `pi --list-models` output. Pi prints a
// fixed-width, space-padded table whose model column can contain spaces:
//
//	provider   model               context  max-out  thinking  images
//	llama-cpp  Qwen3.6 27B Q4      128K     16.4K    no        no
//	llama-cpp  Qwen3.6 27B Q6 MTP  128K     16.4K    no        no
//	anthropic  claude-sonnet-4-... 200K     16K      yes       yes
//
// We can't tokenize on whitespace because `Qwen3.6 27B Q4` is one model
// name, not three. Instead we read the header row, locate the byte offsets
// of `provider`, `model`, and `context`, and slice each subsequent row
// against those offsets.
func parsePiListModels(raw []byte) []ModelInfo {
	seen := make(map[string]bool)
	var models []ModelInfo

	var providerStart, modelStart, modelEnd int
	headerParsed := false

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := stripANSI(scanner.Text())
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !headerParsed {
			providerStart = strings.Index(line, "provider")
			modelStart = strings.Index(line, "model")
			modelEnd = strings.Index(line, "context")
			// Without these three landmarks we can't slice rows safely.
			if providerStart < 0 || modelStart <= providerStart || modelEnd <= modelStart {
				return nil
			}
			headerParsed = true
			continue
		}

		if len(line) <= modelStart {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(line[providerStart:modelStart]))
		// Be defensive about short rows (e.g. footer text) so we don't
		// panic if a line ends before the context column.
		end := modelEnd
		if end > len(line) {
			end = len(line)
		}
		modelID := strings.TrimSpace(line[modelStart:end])
		if !looksLikeProvider(provider) || !looksLikeModelName(modelID) {
			continue
		}
		value := "pi/" + provider + "/" + modelID
		if seen[value] {
			continue
		}
		seen[value] = true
		models = append(models, ModelInfo{
			Label:    value,
			Value:    value,
			Group:    "Pi · " + provider,
			Provider: "pi",
		})
	}
	return models
}

// looksLikeProvider rejects obviously-non-provider tokens so we don't
// promote a stray log line into a model entry. Allows letters, digits, and
// `-`; rejects punctuation, slashes, etc.
func looksLikeProvider(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// looksLikeModelName accepts the broad set of characters pi model handles use
// (alphanumerics, `-`, `_`, `.`, `:`, `/`) plus spaces — pi's llama-cpp
// provider exposes human-readable names like `Qwen3.6 27B Q4`.
func looksLikeModelName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':' || r == '/' || r == ' ':
		default:
			return false
		}
	}
	return true
}

// stripANSI removes ANSI escape sequences so terminal-formatted output
// (colors, cursor moves) doesn't confuse the parser.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEsc {
			// ANSI CSI sequences end with a byte in [0x40, 0x7e].
			if c >= 0x40 && c <= 0x7e {
				inEsc = false
			}
			continue
		}
		if c == 0x1b {
			inEsc = true
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
