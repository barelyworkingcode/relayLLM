package main

import (
	"fmt"
	"os"
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

// RelayManagedSpec is the subset of fields any spawnable (PTY template,
// LLM-pi provider) needs to participate in relay-managed env resolution:
// regenerate a project skill via relay's bridge, fetch a project-scoped
// token, and expose those plus the project path back as substitution
// values. Both call sites build this from their own config and call
// Resolve() — keeping the two surfaces decoupled while sharing the
// bridge-call + substitution logic.
type RelayManagedSpec struct {
	Directory       string // session/terminal cwd; bridge uses this to infer the project
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

// Expand substitutes ${PROJECT_PATH}, ${SKILL_PATH}, ${RELAY_TOKEN} and
// the lowercase ${project.path} into s.
func (s SpawnSubs) Expand(in string) string {
	r := strings.NewReplacer(
		"${PROJECT_PATH}", s.ProjectPath,
		"${SKILL_PATH}", s.SkillPath,
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
	managed := r.UseRelayToken || (r.AutoRegenSkills != "" && r.AutoRegenSkills != AutoRegenNever)
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
	if r.UseRelayToken {
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
