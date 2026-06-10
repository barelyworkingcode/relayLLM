package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// AutoRegen* are the "always | skipIfExists | never" mode values used by
// PiProjectOverlay.Mode (config.go / pi_overlay.go). relayLLM does not
// regenerate skills — relay owns skill generation (relay ADR-004), and the
// ResolvePtyEnv bridge call no longer carries a regen field. Empty == never.
const (
	AutoRegenAlways       = "always"
	AutoRegenSkipIfExists = "skipIfExists"
	AutoRegenNever        = "never"
)

// applyEnvPassthrough copies each key in keys from os.Environ() into env
// (using setEnv to avoid duplicates). Shared by the PTY launcher and the
// LLM-pi provider so the same env_passthrough semantics apply to both.
func applyEnvPassthrough(env []string, keys []string) []string {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			env = setEnv(env, key, v)
		}
	}
	return env
}

// relaySecretEnvKeys are the relay credentials relay injected into THIS relayLLM
// process. They must never be inherited by a spawned child (shell, LLM CLI, or
// the `relay mcp` subprocess): a child gets only a project-scoped
// RELAY_PROJECT_TOKEN, injected explicitly. Inheriting the service token would
// hand the child full bridge access; inheriting the frontend token would hand it
// relay's front-door bearer. (relayLLM keeps these in its OWN env to call the
// bridge — childBaseEnv only strips them from what we hand to children.)
var relaySecretEnvKeys = []string{
	envServiceToken,       // RELAY_SERVICE_TOKEN — full bridge access
	envServiceTokenLegacy, // RELAY_MCP_TOKEN — legacy alias of the above
	envFrontendToken,      // RELAY_FRONTEND_TOKEN — relay front-door bearer (unused by relayLLM)
	// Project-token names too: relayLLM resolves and injects the correct
	// per-child project token explicitly (setProjectTokenEnv). Stripping any
	// inherited value first means a stale/cross-project token in relayLLM's own
	// env can never leak into a child we don't inject one for (e.g. an ad-hoc
	// terminal).
	envProjectToken,       // RELAY_PROJECT_TOKEN
	envProjectTokenLegacy, // RELAY_TOKEN
}

// setProjectTokenEnv sets the project-scoped token on a child env under both the
// current and legacy names. Dual-write is a transition shim (drop the legacy
// name once nothing reads RELAY_TOKEN) that keeps existing user skills/scripts
// referencing RELAY_TOKEN working. No-op for an empty token.
func setProjectTokenEnv(env []string, token string) []string {
	if token == "" {
		return env
	}
	env = setEnv(env, envProjectToken, token)
	env = setEnv(env, envProjectTokenLegacy, token)
	return env
}

// childBaseEnv returns os.Environ() with relayLLM's own relay credentials
// stripped. Use it as the base environment for every spawned child instead of
// os.Environ() directly, so relay secrets never leak into a shell/LLM/mcp child.
func childBaseEnv() []string {
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
// while sharing the bridge-call + substitution logic. Skill regeneration is
// no longer requested here; relay owns it (see relay ADR-004).
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
// any directory passed to --skill).
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

	resp, err := resolveRelayPtyEnv(RelayPtyEnvRequest{
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

// resolveProjectToken returns the project-scoped token for a session, resolved
// just-in-time from relay's bridge by project id. Relay is the sole token
// authority: relayLLM never persists the token and never accepts it from eve —
// it asks relay for it at spawn time, injects it, and discards it.
//
// Fails closed: returns "" when this process is not relay-managed (no service
// token in the env → standalone/dev run) or when resolution fails. Callers must
// spawn without a token rather than substitute the full-access service token —
// handing a child the service token would grant it god-mode bridge access.
func resolveProjectToken(session *Session) string {
	if session == nil {
		return ""
	}
	if serviceToken() == "" {
		return "" // standalone: no relay bridge to ask
	}
	resp, err := resolveRelayPtyEnv(RelayPtyEnvRequest{
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
