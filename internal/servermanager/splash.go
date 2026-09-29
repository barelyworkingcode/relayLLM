package servermanager

import (
	"fmt"
	"os"
	"path/filepath"
	"relayllm/internal/config"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const splashKind = "splash"

// SplashProfile manages Splash ("splash serve"). Its launcher execs a Python
// server that spawns the native engine in the same process group, so
// KillProcessGroup is required for a stop to reach the engine. serve is a
// subcommand argparse only accepts before the flags, hence LeadingArgs.
var SplashProfile = config.ServerProfile{
	Kind: splashKind, DefaultBinary: "splash", Group: "Splash",
	LeadingArgs: []string{"serve"}, AliasFlag: "served-model-name",
	DefaultBasePort: 9500, DefaultIdleTimeoutMinutes: 60, KillProcessGroup: true,
}

// SetStopGraceForTest overrides the SIGTERM-to-SIGKILL window. Test-only seam.
func (m *ServerManager) SetStopGraceForTest(d time.Duration) {
	m.mu.Lock()
	m.stopGrace = d
	m.mu.Unlock()
}

// SetSplashModelsRootForTest points splash preflight at dir instead of
// Splash's own model store. Test-only seam.
func (m *ServerManager) SetSplashModelsRootForTest(dir string) {
	m.mu.Lock()
	m.splashModelsRoot = dir
	m.mu.Unlock()
}

// splashRoot returns the Splash model store. Splash has no override for it, so
// the default is fixed relative to the user's home.
func (m *ServerManager) splashRoot() string {
	if m.splashModelsRoot != "" {
		return m.splashModelsRoot
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "Splash", "models")
}

// splashPreflightAll checks every configured alias without holding m.mu (it
// stats the filesystem). Nil for other profiles.
func (m *ServerManager) splashPreflightAll() map[string]error {
	if m.profile.Kind != splashKind {
		return nil
	}
	m.mu.Lock()
	root := m.splashRoot()
	m.mu.Unlock()
	out := make(map[string]error)
	for _, cfg := range m.config.Models {
		if err := splashPreflight(root, cfg); err != nil {
			out[cfg.Alias] = err
		}
	}
	return out
}

// splashPreflight fails early for a model the launcher cannot serve, chiefly
// one whose weights are not downloaded: "splash serve" would start a download
// and the caller would see only a health timeout. The installed check is
// skipped when revision, language-only or draft-model is set, because those
// move the installed markers under .selections.
func splashPreflight(root string, cfg config.ServerModelConfig) error {
	id, _ := cfg.Args["model"].(string)
	if id == "" {
		return fmt.Errorf("splash: model entry %q has no \"model\"", cfg.Alias)
	}
	repo, _, _ := strings.Cut(id, ":")
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(repo[len(owner)+1:], "/\\") ||
		strings.ContainsAny(owner, "\\") || owner == "." || owner == ".." || name == "." || name == ".." {
		return fmt.Errorf("splash: model %q (alias %q) is not a Hugging Face repo id; use OWNER/REPO or OWNER/REPO:VARIANT", id, cfg.Alias)
	}
	for _, k := range []string{"revision", "language-only", "draft-model"} {
		if _, set := cfg.Args[k]; set {
			return nil
		}
	}
	dir := filepath.Join(root, owner, strings.TrimPrefix(id, owner+"/"))
	for _, marker := range []string{"model.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("splash: model not downloaded: %q (alias %q); download it first with \"splash serve --model %s\"", id, cfg.Alias, id)
}

// splashMaxContext reads max-context: a number, "NK" (N*1024), or "auto"
// (0, meaning the engine decides).
func splashMaxContext(args map[string]any) int64 {
	if n, ok := numericArg(args, "max-context"); ok && n > 0 {
		return int64(n)
	}
	s, _ := args["max-context"].(string)
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "auto" {
		return 0
	}
	mult := int64(1)
	if strings.HasSuffix(s, "k") {
		mult = 1024
		s = strings.TrimSuffix(s, "k")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n * mult
}

// killInstance force-kills a failed launch: the whole group for a profile that
// spawns its server as a group child, else just the leader.
func killInstance(profile config.ServerProfile, inst *serverInstance) {
	if profile.KillProcessGroup {
		_ = syscall.Kill(-inst.cmd.Process.Pid, syscall.SIGKILL)
		return
	}
	_ = inst.cmd.Process.Kill()
}
