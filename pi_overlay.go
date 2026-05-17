package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// PiOverlayInputs bundles everything MaterializePiOverlay needs to translate
// relayLLM's curated provider set into pi's models.json schema. Built once at
// startup in main.go and threaded to both the LLM and PTY spawn paths.
type PiOverlayInputs struct {
	OpenAI         *OpenAIConfig
	LlamaModels    []LlamaModelConfig
	LlamaProxyPort string // e.g. "8091"; empty disables the relay-llama provider entry
}

// piOverlayFileMode is 0o600 to match pi's own auth.json perms — these files
// can hold API keys when IncludeUserProviders=true copies them over.
const piOverlayFileMode = 0o600

// piRelayLlamaProvider is the canonical name relayLLM registers for its
// llama-server proxy in the overlay's models.json. Exposed as a constant so
// settings.DefaultProvider can be set to this without typos.
const piRelayLlamaProvider = "relay-llama"

// applyPiOverlayEnv materializes the overlay and, when one is written, sets
// PI_CODING_AGENT_DIR in env. Returned env replaces the caller's. Single
// hook used by both LLM RPC spawns (provider_pi.go) and PTY spawns
// (terminal_session.go) so the two surfaces stay in sync.
func applyPiOverlayEnv(env []string, projectDir string, cfg *PiConfig, inputs PiOverlayInputs) ([]string, error) {
	overlayDir, err := MaterializePiOverlay(projectDir, cfg, inputs)
	if err != nil {
		return env, err
	}
	if overlayDir != "" {
		env = setEnv(env, "PI_CODING_AGENT_DIR", overlayDir)
	}
	return env, nil
}

// MaterializePiOverlay writes <projectDir>/<dirName>/{models,settings,auth}.json
// reflecting relayLLM's curated provider set and returns the overlay dir path
// for use as PI_CODING_AGENT_DIR. Returns ("", nil) when overlay is disabled
// or projectDir is empty.
func MaterializePiOverlay(projectDir string, cfg *PiConfig, inputs PiOverlayInputs) (string, error) {
	if cfg == nil || !cfg.ProjectOverlay.Enabled() {
		return "", nil
	}
	if projectDir == "" || projectDir == "/" {
		return "", nil
	}

	overlay := cfg.ProjectOverlay
	dirName := overlay.DirName
	if dirName == "" {
		dirName = ".pi"
	}
	overlayDir := filepath.Join(projectDir, dirName)

	if err := os.MkdirAll(overlayDir, 0o700); err != nil {
		return "", fmt.Errorf("pi overlay: mkdir %s: %w", overlayDir, err)
	}

	rewriteAll := overlay.Mode == AutoRegenAlways
	skipIfExists := overlay.Mode == AutoRegenSkipIfExists

	modelsPath := filepath.Join(overlayDir, "models.json")
	settingsPath := filepath.Join(overlayDir, "settings.json")

	globalAgent := globalPiAgentDir()

	// models.json
	if rewriteAll || (skipIfExists && !fileExists(modelsPath)) {
		models := buildPiModelsJSON(inputs, overlay, globalAgent)
		if err := writePiOverlayJSON(modelsPath, models); err != nil {
			return "", err
		}
	}

	// settings.json
	if rewriteAll || (skipIfExists && !fileExists(settingsPath)) {
		settings := buildPiSettingsJSON(projectDir, overlay, globalAgent)
		if err := writePiOverlayJSON(settingsPath, settings); err != nil {
			return "", err
		}
	}

	// Symlink everything we don't own ourselves back to the user's global
	// pi agent dir so credentials stay centrally managed and pi's
	// lazily-downloaded helper binaries (fd, ripgrep, …) are shared instead
	// of re-downloaded per project. authStrategy:"none" opts out of auth.
	passthroughs := []struct {
		name     string
		required bool // fail closed if global is missing
		skip     bool
	}{
		{"auth.json", true, overlay.AuthStrategy == PiAuthStrategyNone},
		{"bin", false, false},
	}
	for _, p := range passthroughs {
		if p.skip {
			continue
		}
		err := ensurePiOverlaySymlink(
			filepath.Join(overlayDir, p.name),
			filepath.Join(globalAgent, p.name),
			p.required,
		)
		if err == nil {
			continue
		}
		if p.required {
			return "", err
		}
		slog.Warn("pi overlay: passthrough symlink failed", "name", p.name, "error", err)
	}

	// .gitignore append (opt-in).
	if overlay.Gitignore {
		if err := ensureGitignoreEntry(projectDir, dirName); err != nil {
			slog.Warn("pi overlay: gitignore append failed", "project", projectDir, "error", err)
		}
	}

	slog.Info("pi overlay materialized", "dir", overlayDir, "mode", overlay.Mode)
	return overlayDir, nil
}

// buildPiModelsJSON emits the {providers: {...}} shape pi's model-registry.js
// expects. Each relayLLM endpoint becomes one provider; the user's global
// providers are merged in (relayLLM's entries win on key collision) unless
// ExcludeUserProviders is set.
func buildPiModelsJSON(inputs PiOverlayInputs, overlay PiProjectOverlay, globalAgent string) map[string]any {
	providers := map[string]any{}

	// 1. Start from user's global models.json (so their custom providers
	//    stay reachable in relay projects). ExcludeProviders names specific
	//    entries to drop — useful when our relay-llama supersedes a
	//    user-defined "llama-cpp" pointing at the same models.
	if !overlay.ExcludeUserProviders {
		excluded := make(map[string]bool, len(overlay.ExcludeProviders))
		for _, name := range overlay.ExcludeProviders {
			excluded[name] = true
		}
		if existing := readPiModelsJSON(filepath.Join(globalAgent, "models.json")); existing != nil {
			for name, entry := range existing {
				if excluded[name] {
					continue
				}
				providers[name] = entry
			}
		}
	}

	// 2. relay-llama: the OpenAI-compat proxy in front of llama-server.
	if inputs.LlamaProxyPort != "" && len(inputs.LlamaModels) > 0 {
		models := make([]map[string]any, 0, len(inputs.LlamaModels))
		for _, m := range inputs.LlamaModels {
			models = append(models, map[string]any{"id": m.Alias})
		}
		providers[piRelayLlamaProvider] = map[string]any{
			"baseUrl": fmt.Sprintf("http://localhost:%s/v1", strings.TrimPrefix(inputs.LlamaProxyPort, ":")),
			"api":     "openai-completions",
			"apiKey":  "none",
			"models":  models,
		}
	}

	// 3. One provider per configured OpenAI-compatible endpoint.
	if inputs.OpenAI != nil {
		for _, ep := range inputs.OpenAI.Endpoints {
			name := sanitizePiProviderName(ep.Name)
			if name == "" {
				continue
			}
			entry := map[string]any{
				"baseUrl": ep.BaseURL,
				"api":     "openai-completions",
			}
			if ep.APIKey != "" {
				entry["apiKey"] = ep.APIKey
			} else {
				entry["apiKey"] = "none"
			}
			providers[name] = entry
		}
	}

	return map[string]any{"providers": providers}
}

// buildPiSettingsJSON emits a minimal settings.json carrying our defaults and
// skill paths. When IncludeUserSettings (the default), the user's global
// settings.json is read and merged underneath so theme/keybindings/etc carry
// over.
func buildPiSettingsJSON(projectDir string, overlay PiProjectOverlay, globalAgent string) map[string]any {
	settings := map[string]any{}

	if !overlay.ExcludeUserSettings {
		if existing := readJSONCMap(filepath.Join(globalAgent, "settings.json")); existing != nil {
			for k, v := range existing {
				settings[k] = v
			}
		}
	}

	if overlay.DefaultProvider != "" {
		settings["defaultProvider"] = overlay.DefaultProvider
	}
	if overlay.DefaultModel != "" {
		settings["defaultModel"] = overlay.DefaultModel
	}
	if overlay.DefaultThinking != "" {
		settings["defaultThinkingLevel"] = overlay.DefaultThinking
	}

	// Skills: project's relay skill dir + any extras + whatever was in the
	// user's global settings (already copied above). Dedup while preserving
	// order so the user's customizations stay visible.
	var skills []string
	if existing, ok := settings["skills"].([]any); ok {
		for _, v := range existing {
			if s, ok := v.(string); ok {
				skills = append(skills, s)
			}
		}
	}
	relaySkillDir := filepath.Join(projectDir, ".claude", "skills")
	if info, err := os.Stat(relaySkillDir); err == nil && info.IsDir() {
		skills = appendUnique(skills, relaySkillDir)
	}
	for _, extra := range overlay.ExtraSkillDirs {
		if extra != "" {
			skills = appendUnique(skills, extra)
		}
	}
	if len(skills) > 0 {
		settings["skills"] = skills
	}

	return settings
}

// ensurePiOverlaySymlink points overlayPath at globalPath. Idempotent:
// replaces stale symlinks (target moved or file became a regular file). When
// requireTarget is true, returns an error if globalPath doesn't exist (used
// for auth.json so we fail closed before spawning pi without credentials);
// when false, creates the global target dir if absent and proceeds — pi
// will populate it lazily (used for bin/, which is initially empty).
func ensurePiOverlaySymlink(overlayPath, globalPath string, requireTarget bool) error {
	if _, err := os.Stat(globalPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("pi overlay: stat %s: %w", globalPath, err)
		}
		if requireTarget {
			return fmt.Errorf("pi overlay: global %s missing (run `pi auth login` first or set authStrategy:\"none\")", globalPath)
		}
		// Best-effort: create the global dir so pi has somewhere to drop
		// downloads. Mode 0o700 matches pi's own dir perms.
		if err := os.MkdirAll(globalPath, 0o700); err != nil {
			return fmt.Errorf("pi overlay: mkdir %s: %w", globalPath, err)
		}
	}

	if info, err := os.Lstat(overlayPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if existing, _ := os.Readlink(overlayPath); existing == globalPath {
				return nil // already correct
			}
		}
		if err := os.RemoveAll(overlayPath); err != nil {
			return fmt.Errorf("pi overlay: remove stale %s: %w", overlayPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("pi overlay: lstat %s: %w", overlayPath, err)
	}

	if err := os.Symlink(globalPath, overlayPath); err != nil {
		return fmt.Errorf("pi overlay: symlink %s → %s: %w", overlayPath, globalPath, err)
	}
	return nil
}

// ensureGitignoreEntry appends a line for the overlay dir to <projectDir>/.gitignore
// if not already present. Creates the file if missing.
func ensureGitignoreEntry(projectDir, dirName string) error {
	path := filepath.Join(projectDir, ".gitignore")
	entry := "/" + strings.TrimPrefix(dirName, "/") + "/"

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == entry || strings.TrimSpace(line) == strings.TrimSuffix(entry, "/") {
			return nil
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + entry + "\n")
	return err
}

// globalPiAgentDir returns ~/.pi/agent/. Honors PI_CODING_AGENT_DIR if the
// user has redirected pi globally — we want to merge from whatever they're
// actually using as their global config, not the literal home path.
func globalPiAgentDir() string {
	if env := os.Getenv("PI_CODING_AGENT_DIR"); env != "" {
		return expandHome(env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

// readPiModelsJSON loads the providers map from a pi models.json on disk.
// Returns nil if the file is missing or malformed; callers treat that as
// "no providers to merge".
func readPiModelsJSON(path string) map[string]any {
	var parsed struct {
		Providers map[string]any `json:"providers"`
	}
	if err := readJSONCFile(path, &parsed); err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("pi overlay: failed to read global models.json", "path", path, "error", err)
		}
		return nil
	}
	return parsed.Providers
}

// readJSONCMap reads a JSONC file into a generic map. Returns nil on any
// error so callers gracefully skip absent or malformed files.
func readJSONCMap(path string) map[string]any {
	out := map[string]any{}
	if err := readJSONCFile(path, &out); err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("pi overlay: failed to read JSONC file", "path", path, "error", err)
		}
		return nil
	}
	return out
}

// writeJSON marshals v with 2-space indent and writes it atomically (tmp +
// rename) at mode 0o600 to match pi's own files.
func writePiOverlayJSON(path string, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("pi overlay: marshal %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, piOverlayFileMode); err != nil {
		return fmt.Errorf("pi overlay: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pi overlay: rename %s: %w", tmp, err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// sanitizePiProviderName strips characters pi's registry would reject. The
// schema requires minLength:1, no other constraints documented, but our
// endpoint names sometimes contain spaces or slashes that confuse the model
// picker. Be conservative.
func sanitizePiProviderName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ', r == '/':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// appendUnique returns the slice with v appended only if it isn't already
// present. Preserves insertion order (callers rely on user-global entries
// staying ahead of relay additions for predictable picker ordering).
func appendUnique(s []string, v string) []string {
	for _, existing := range s {
		if existing == v {
			return s
		}
	}
	return append(s, v)
}

