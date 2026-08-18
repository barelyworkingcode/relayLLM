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
// relayLLM's curated provider set into pi's models.json schema.
//
// Pi's ModelRegistry treats providers without an explicit `models` array as
// override-only (no /v1/models auto-discovery), so RouterModels must be
// snapshotted from the router's currently-routable set at spawn time —
// managed-server aliases bare (llama + mlx), OpenAI endpoint models prefixed
// with the endpoint name.
type PiOverlayInputs struct {
	ServerModels []ServerModelConfig // consumed by pi_models.go to synthesize Eve picker entries
	RouterPort   string              // empty disables the relay-router provider entry
	RouterModels []PiRouterModel
}

// PiRouterModel is one row of the router snapshot written into pi's
// models.json. Capability travels with the id because pi's `openai-completions`
// provider does no discovery: it trusts models.json and nothing else, so a
// vision model we describe as bare {"id": ...} is one pi will refuse to send
// images to ("Current model does not support images").
type PiRouterModel struct {
	ID             string
	SupportsImages bool
}

// piOverlayFileMode is 0o600 to match pi's own auth.json perms — these files
// can hold API keys when IncludeUserProviders=true copies them over.
const piOverlayFileMode = 0o600

// piRelayRouterProvider is the canonical name relayLLM registers for its
// router in the overlay's models.json. Exposed as a constant so
// settings.DefaultProvider can be set to this without typos.
const piRelayRouterProvider = "relay-router"

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
		settings := buildPiSettingsJSON(projectDir, overlay, globalAgent, inputs)
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

// buildPiModelsJSON emits the {providers: {...}} shape pi's ModelRegistry
// expects. User-global providers are merged underneath (ours wins on
// collision) unless ExcludeUserProviders is set; an empty RouterPort means
// relayLLM contributes nothing and globals carry through unchanged.
func buildPiModelsJSON(inputs PiOverlayInputs, overlay PiProjectOverlay, globalAgent string) map[string]any {
	providers := map[string]any{}

	// 1. Start from user's global models.json (so their custom providers
	//    stay reachable in relay projects). ExcludeProviders names specific
	//    entries to drop — useful when our relay-router supersedes a
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

	// 2. relay-router: the unified OpenAI-compat router fronting llama-server
	//    aliases + reachable OpenAI endpoints. Models must be enumerated
	//    explicitly — pi treats an empty models array as override-only.
	if inputs.RouterPort != "" {
		models := make([]map[string]any, 0, len(inputs.RouterModels))
		for _, rm := range inputs.RouterModels {
			// pi's schema: input is ("text"|"image")[]. Omitting it leaves the
			// model text-only, so state it either way rather than relying on
			// pi's default.
			input := []string{"text"}
			if rm.SupportsImages {
				input = append(input, "image")
			}
			models = append(models, map[string]any{"id": rm.ID, "input": input})
		}
		providers[piRelayRouterProvider] = map[string]any{
			"baseUrl": fmt.Sprintf("http://localhost:%s/v1", inputs.RouterPort),
			"api":     "openai-completions",
			"apiKey":  "none",
			"models":  models,
		}
	}

	return map[string]any{"providers": providers}
}

// buildPiSettingsJSON emits a minimal settings.json carrying our defaults and
// skill paths. When IncludeUserSettings (the default), the user's global
// settings.json is read and merged underneath so theme/keybindings/etc carry
// over.
func buildPiSettingsJSON(projectDir string, overlay PiProjectOverlay, globalAgent string, inputs PiOverlayInputs) map[string]any {
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
