package spawn

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"relayllm/internal/relay"
	"relayllm/internal/types"
)

// ApplyEnvPassthrough copies each key in keys from os.Environ() into env
// (using SetEnv to avoid duplicates). Shared by the PTY launcher and the
// LLM-pi provider so the same env_passthrough semantics apply to both.
func ApplyEnvPassthrough(env []string, keys []string) []string {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			env = SetEnv(env, key, v)
		}
	}
	return env
}

// relaySecretEnvKeys must never be inherited by a spawned child (shell, LLM CLI,
// or the `relay mcp` subprocess): a child gets only a project-scoped
// RELAY_PROJECT_TOKEN, injected explicitly.
var relaySecretEnvKeys = []string{
	// Removed credential names, stripped defensively so a stale value from an
	// older relay or the user's shell cannot reach a child.
	relay.EnvServiceToken,
	relay.EnvServiceTokenLegacy,
	relay.EnvFrontendToken,
	// A child must never believe it holds a launch pipe: the identity relay
	// binds is this process's, and a child presenting Hello would fail anyway.
	relay.EnvLaunchFD,
	// Project-token names too: relayLLM resolves and injects the correct
	// per-child project token explicitly (SetProjectTokenEnv). Stripping any
	// inherited value first means a stale/cross-project token in relayLLM's own
	// env can never leak into a child we don't inject one for (e.g. an ad-hoc
	// terminal).
	relay.EnvProjectToken,       // RELAY_PROJECT_TOKEN
	relay.EnvProjectTokenLegacy, // RELAY_TOKEN
}

// SetProjectTokenEnv sets the project-scoped token on a child env under both the
// current and legacy names. Dual-write is a transition shim (drop the legacy
// name once nothing reads RELAY_TOKEN) that keeps existing user skills/scripts
// referencing RELAY_TOKEN working. No-op for an empty token.
func SetProjectTokenEnv(env []string, token string) []string {
	if token == "" {
		return env
	}
	env = SetEnv(env, relay.EnvProjectToken, token)
	env = SetEnv(env, relay.EnvProjectTokenLegacy, token)
	return env
}

// ChildBaseEnv returns os.Environ() with relayLLM's own relay credentials
// stripped. Use it as the base environment for every spawned child instead of
// os.Environ() directly, so relay secrets never leak into a shell/LLM/mcp child.
func ChildBaseEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		drop := false
		for _, k := range relaySecretEnvKeys {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// RelayManagedSpec is the subset of fields any spawnable (PTY template,
// LLM-pi provider) needs to participate in relay-managed env resolution:
// fetch a project-scoped token via relay's bridge and expose the resolved
// project path back as a substitution value. Both call sites build this from
// their own config and call Resolve() — keeping the two surfaces decoupled
// while sharing the bridge-call + substitution logic. Skill generation is
// relay's responsibility entirely (see relay ADR-004); this spawn path never
// requests or triggers it.
type RelayManagedSpec struct {
	ProjectID     string // authoritative project key; relay validates Directory is within the project. A non-empty value makes the spawn project-scoped (gets a token).
	Directory     string // session/terminal cwd; relay validates it against ProjectID, or (legacy) uses it to infer the project
	UseRelayToken bool
	Label         string // human-readable identifier (template id / "pi") used in error messages
}

// SpawnSubs holds the resolved substitution values from a relay bridge
// ResolvePtyEnv call. For non-relay-managed spawns only ProjectPath is set.
type SpawnSubs struct {
	ProjectPath string // resolved via bridge (or Directory if not relay-managed)
	RelayToken  string // project plaintext token (empty if !UseRelayToken)
}

// Expand substitutes ${PROJECT_PATH}, ${RELAY_TOKEN} and the lowercase
// ${project.path} into s. The skills directory is the convention
// ${PROJECT_PATH}/.claude/skills — callers that need a --skill flag build it
// from ${PROJECT_PATH} (Pi and Claude Code discover skills recursively under
// any directory passed to --skill). There is no ${SKILL_PATH}/${SKILLS_ROOT}
// token: an older config or template that still references either reaches
// the spawned process as that literal, unexpanded text — Replacer only
// substitutes tokens it knows, and silently leaves everything else alone.
func (s SpawnSubs) Expand(in string) string {
	r := strings.NewReplacer(
		"${PROJECT_PATH}", s.ProjectPath,
		"${RELAY_TOKEN}", s.RelayToken,
		"${project.path}", s.ProjectPath,
	)
	return r.Replace(in)
}

// Resolve computes substitution values for a spawn. For non-relay-managed
// specs it returns SpawnSubs{ProjectPath: Directory} without contacting the
// bridge. For relay-managed specs (an explicit ProjectID or one that opted
// into the relay token) it calls relay's bridge ResolvePtyEnv to fetch a
// project-scoped token and the resolved project path. Fails closed: returns
// an error rather than spawning without the token it asked for.
func (r RelayManagedSpec) Resolve() (SpawnSubs, error) {
	// A project-scoped spawn (explicit ProjectID) or one that opted into the
	// relay token gets a project-scoped token. The legacy UseRelayToken flag
	// is kept working for templates that don't carry a project id.
	wantToken := r.ProjectID != "" || r.UseRelayToken
	if !wantToken {
		return SpawnSubs{ProjectPath: r.Directory}, nil
	}

	resp, err := relay.ResolvePtyEnv(relay.RelayPtyEnvRequest{
		ProjectID: r.ProjectID,
		Directory: r.Directory,
	})
	if err != nil {
		return SpawnSubs{}, fmt.Errorf("relay-managed %s: resolve env: %w", r.labelOrDefault(), err)
	}

	projectPath := resp.WorkingDir
	if projectPath == "" {
		projectPath = r.Directory
	}
	return SpawnSubs{
		ProjectPath: projectPath,
		RelayToken:  resp.RelayToken,
	}, nil
}

func (r RelayManagedSpec) labelOrDefault() string {
	if r.Label != "" {
		return r.Label
	}
	return "spawn"
}

// ResolveProjectToken returns the project-scoped token for a session, resolved
// just-in-time from relay's bridge by project id. Relay is the sole token
// authority: relayLLM never persists the token and never accepts it from eve —
// it asks relay for it at spawn time, injects it, and discards it.
//
// Fails closed: returns "" when relay did not launch this process (standalone
// or dev run) or when resolution fails. Callers must spawn without a token.
func ResolveProjectToken(session *types.Session) string {
	if session == nil {
		return ""
	}
	if !relay.Launched() {
		return "" // standalone: no relay bridge to ask
	}
	resp, err := relay.ResolvePtyEnv(relay.RelayPtyEnvRequest{
		ProjectID: session.ProjectID,
		Directory: session.Directory,
	})
	if err != nil {
		slog.Warn("resolve project token from relay failed",
			"session", session.ID, "project", session.ProjectID, "error", err)
		return ""
	}
	return resp.RelayToken
}

// SetEnv sets or replaces an environment variable in a slice.
func SetEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// ResolveClaudePath finds the claude binary, checking well-known locations
// before falling back to PATH lookup. Necessary when launched from minimal
// environments (Raycast, launchd) that don't source shell profiles.
func ResolveClaudePath() string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
		"/usr/local/bin/claude",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Fall back to PATH lookup.
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return "claude"
}

// EnsurePath adds ~/.local/bin to PATH in the environment slice if not already present.
func EnsurePath(env []string) []string {
	home, _ := os.UserHomeDir()
	localBin := filepath.Join(home, ".local", "bin")

	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			if !strings.Contains(e, localBin) {
				env[i] = e + ":" + localBin
			}
			return env
		}
	}
	// No PATH at all — set one.
	return append(env, "PATH=/usr/local/bin:/usr/bin:/bin:"+localBin)
}

// HasArg reports whether args contains a flag matching name (case-insensitive).
// Used so callers don't auto-append a flag if the user already put it in extraArgs.
func HasArg(args []string, name string) bool {
	for _, a := range args {
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}
