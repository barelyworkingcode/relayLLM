package config

// TerminalTemplate defines a launchable terminal type.
//
// On disk inside settings.json's `pty` map the entries omit `id` (the map key
// IS the id) and `builtIn` (computed from protectedTemplateIDs at API time).
// In-memory copies returned by Get/List have both populated for consumers.
//
// Relay-managed fields (UseRelayToken, EnvPassthrough) opt the template into
// spawn-time resolution via relay's bridge ResolvePtyEnv. Args may reference
// ${PROJECT_PATH} and ${RELAY_TOKEN}; the skills directory is the convention
// ${PROJECT_PATH}/.claude/skills (relay generates and manages the SKILL.md
// files there). See terminal_session.go:Start for the substitution rules.
type TerminalTemplate struct {
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
	Icon        string            `json:"icon,omitempty"`
	BuiltIn     bool              `json:"builtIn,omitempty"`
	IdleTimeout int               `json:"idleTimeout,omitempty"` // minutes, 0 = default (1440 = 24h)

	// Relay-managed PTY fields. Zero values mean "not relay-managed".
	UseRelayToken  bool     `json:"useRelayToken,omitempty"`
	EnvPassthrough []string `json:"env_passthrough,omitempty"`
}
