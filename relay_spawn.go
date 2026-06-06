package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// AutoRegenSkills values. Shared by the PTY template (TerminalTemplate)
// and the LLM-pi config (PiConfig). Empty == AutoRegenNever.
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
// regenerate a project skill via relay's bridge, fetch a project-scoped
// token, and expose those plus the project path back as substitution
// values. Both call sites build this from their own config and call
// Resolve() — keeping the two surfaces decoupled while sharing the
// bridge-call + substitution logic.
type RelayManagedSpec struct {
	ProjectID       string // authoritative project key; relay validates Directory is within the project. A non-empty value makes the spawn project-scoped (gets a token).
	Directory       string // session/terminal cwd; relay validates it against ProjectID, or (legacy) uses it to infer the project
	SkillPath       string // template — supports ${project.path}
	AutoRegenSkills string // "always" | "skipIfExists" | "never" (empty = "never")
	UseRelayToken   bool
	Label           string // human-readable identifier (template id / "pi") used in error messages
}

// SpawnSubs holds the resolved substitution values from a relay bridge
// ResolvePtyEnv call. For non-relay-managed spawns only ProjectPath is set.
type SpawnSubs struct {
	ProjectPath string // resolved via bridge (or Directory if not relay-managed)
	SkillPath   string // path where SKILL.md was written
	RelayToken  string // project plaintext token (empty if !UseRelayToken)
}

// Expand substitutes ${PROJECT_PATH}, ${SKILL_PATH}, ${SKILLS_ROOT},
// ${RELAY_TOKEN} and the lowercase ${project.path} into s.
//
// ${SKILLS_ROOT} resolves to the parent directory of ${SKILL_PATH}. Pi (and
// Claude Code) discover skills recursively under any directory passed to
// --skill, so pointing at the parent surfaces every sibling skill alongside
// the relay-managed one in a single flag.
func (s SpawnSubs) Expand(in string) string {
	skillsRoot := ""
	if s.SkillPath != "" {
		skillsRoot = filepath.Dir(s.SkillPath)
	}
	r := strings.NewReplacer(
		"${PROJECT_PATH}", s.ProjectPath,
		"${SKILL_PATH}", s.SkillPath,
		"${SKILLS_ROOT}", skillsRoot,
		"${RELAY_TOKEN}", s.RelayToken,
		"${project.path}", s.ProjectPath,
	)
	return r.Replace(in)
}

// Resolve computes substitution values for a spawn. For non-relay-managed
// specs it returns SpawnSubs{ProjectPath: Directory} without contacting
// the bridge. For relay-managed specs it calls relay's bridge
// ResolvePtyEnv, which also regenerates SKILL.md as a side effect when
// AutoRegenSkills requires it. Fails closed: returns error rather than
// spawning with unresolved placeholders.
func (r RelayManagedSpec) Resolve() (SpawnSubs, error) {
	// A project-scoped spawn (explicit ProjectID) or one that opted into the
	// relay token gets a project-scoped token. The legacy UseRelayToken flag
	// is kept working for templates that don't carry a project id.
	wantToken := r.ProjectID != "" || r.UseRelayToken
	managed := wantToken || (r.AutoRegenSkills != "" && r.AutoRegenSkills != AutoRegenNever)
	if !managed {
		return SpawnSubs{ProjectPath: r.Directory}, nil
	}

	// SkillPath's ${project.path} is resolved against the launch directory
	// before sending over the bridge — relay treats the path as opaque.
	skillPath := (SpawnSubs{ProjectPath: r.Directory}).Expand(r.SkillPath)

	regen := r.AutoRegenSkills
	if regen == "" {
		regen = AutoRegenNever
	}
	if regen != AutoRegenNever && skillPath == "" {
		return SpawnSubs{}, fmt.Errorf("%s: skillPath required when autoRegenSkills != never", r.labelOrDefault())
	}

	resp, err := resolveRelayPtyEnv(RelayPtyEnvRequest{
		ProjectID:   r.ProjectID,
		Directory:   r.Directory,
		RegenSkills: regen,
		SkillPath:   skillPath,
	})
	if err != nil {
		return SpawnSubs{}, fmt.Errorf("relay-managed %s: resolve env: %w", r.labelOrDefault(), err)
	}

	subs := SpawnSubs{
		ProjectPath: resp.WorkingDir,
		SkillPath:   resp.SkillPath,
	}
	if wantToken {
		subs.RelayToken = resp.RelayToken
	}
	if subs.ProjectPath == "" {
		subs.ProjectPath = r.Directory
	}
	return subs, nil
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
		ProjectID:   session.ProjectID,
		Directory:   session.Directory,
		RegenSkills: AutoRegenNever,
	})
	if err != nil {
		slog.Warn("resolve project token from relay failed",
			"session", session.ID, "project", session.ProjectID, "error", err)
		return ""
	}
	return resp.RelayToken
}
