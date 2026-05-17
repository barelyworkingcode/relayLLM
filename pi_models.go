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

// FetchPiModels returns the list of models advertised by the pi CLI, wrapped
// as relayLLM ModelInfo entries with the `pi/<provider>/<modelId>` value
// convention. Results are cached for 5 minutes; the cache also clears
// whenever the configured binary path changes.
//
// configuredPath, if non-empty, overrides the well-known-location lookup —
// useful when pi is installed under a Node version manager or non-standard
// prefix and recorded in config.json's `pi.binaryPath`.
//
// If pi is not on PATH, returns nil — the /api/models endpoint silently
// drops the pi section.
func FetchPiModels(ctx context.Context, configuredPath string) []ModelInfo {
	piPath := resolvePiPath(configuredPath)

	// Cheap path: TTL still valid and binary path unchanged → serve cached.
	// Skips the os.Stat and the upstream exec on the common hot path.
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
		// Pi not installed; degrade gracefully.
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

// parsePiListModels scans `pi --list-models` output. Pi prints a whitespace-
// separated table:
//
//	provider   model                  context  max-out  thinking  images
//	llama-cpp  Qwen3.6-35B-A3B-UD-Q4  128K     16.4K    no        no
//	anthropic  claude-sonnet-4-...    200K     16K      yes       yes
//
// We skip the header row (first field == "provider"), take the first two
// fields of each subsequent row as <provider> <model>, and wrap them in
// the relayLLM ModelInfo shape.
func parsePiListModels(raw []byte) []ModelInfo {
	seen := make(map[string]bool)
	var models []ModelInfo

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(stripANSI(scanner.Text()))
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		provider := strings.ToLower(fields[0])
		// Skip the column header.
		if provider == "provider" {
			continue
		}
		modelID := fields[1]
		// Defensive: skip non-table lines (e.g. error messages, blank rows).
		if !looksLikeProvider(provider) || !looksLikeModelID(modelID) {
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

// looksLikeModelID accepts the broad set of characters pi model handles use
// (alphanumerics, `-`, `_`, `.`, `:`, `/`).
func looksLikeModelID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':' || r == '/':
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
