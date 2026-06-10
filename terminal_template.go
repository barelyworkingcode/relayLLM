package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
)

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

// protectedTemplateIDs are seeded built-ins that cannot be deleted or updated
// via the API. Users can still hand-edit settings.json to fully remove them.
// This set also drives the `builtIn` field on API responses.
var protectedTemplateIDs = map[string]bool{
	"claude-code": true,
	"opencode":    true,
	"shell":       true,
}

// ResolveCommand returns the absolute path to the command, checking
// well-known locations before falling back to PATH lookup. Routing is keyed
// off the command (not template ID) so custom user-created templates that
// invoke `claude` get the same well-known-locations resolution as the
// built-in claude-code template — important when relay is launched from
// launchd/Spotlight where ~/.local/bin isn't on PATH.
func (t TerminalTemplate) ResolveCommand() string {
	if t.ID == "shell" || t.Command == "" {
		return resolveShell()
	}
	if filepath.Base(t.Command) == "claude" {
		return resolveClaudePath()
	}
	if p, err := exec.LookPath(t.Command); err == nil {
		return p
	}
	return t.Command
}

// seedDefaultPTYConfig returns the initial pty map written to settings.json on
// first run. Lean shape — only the fields users care to see and edit.
func seedDefaultPTYConfig() map[string]TerminalTemplate {
	return map[string]TerminalTemplate{
		"claude-code": {
			Name:           "Claude Code",
			Command:        "claude",
			Icon:           "terminal",
			Description:    "Claude Code CLI agent (relay-managed: project token injected at launch; skills managed by relay)",
			UseRelayToken:  true,
			EnvPassthrough: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"},
		},
		"opencode":    {Name: "OpenCode", Command: "opencode", Icon: "terminal", Description: "OpenCode CLI agent"},
		"shell":       {Name: "Shell", Icon: "shell", Description: "Default system shell"},
	}
}

// resolveShell returns the user's default shell.
func resolveShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/zsh"
}

// TemplateStore manages terminal templates persisted in settings.json's pty section.
type TemplateStore struct {
	mu        sync.RWMutex
	dataDir   string
	templates map[string]TerminalTemplate // keyed by ID; entries do NOT carry their ID inside
}

func NewTemplateStore(dataDir string) *TemplateStore {
	return &TemplateStore{
		dataDir:   dataDir,
		templates: make(map[string]TerminalTemplate),
	}
}

// Load initializes the store from the pty map loaded out of settings.json.
// If the map is nil/empty, the store seeds defaults and persists them.
func (s *TemplateStore) Load(initial map[string]TerminalTemplate) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(initial) > 0 {
		s.templates = make(map[string]TerminalTemplate, len(initial))
		for k, v := range initial {
			s.templates[k] = v
		}
	}

	if len(s.templates) > 0 {
		return nil
	}

	s.templates = seedDefaultPTYConfig()
	if err := s.persist(); err != nil {
		return fmt.Errorf("seed pty config: %w", err)
	}
	slog.Info("seeded default pty templates into settings.json", "count", len(s.templates))
	return nil
}

// persist writes the current pty map back to settings.json.
// MUST be called with s.mu held — serializes the read-modify-write to
// settings.json so concurrent Creates/Updates/Deletes can't lose each other.
func (s *TemplateStore) persist() error {
	return WriteConfigPTY(s.dataDir, s.templates)
}

// hydrate fills in the synthetic ID and BuiltIn fields on a template copy
// before returning it to API consumers.
func hydrate(id string, t TerminalTemplate) TerminalTemplate {
	t.ID = id
	t.BuiltIn = protectedTemplateIDs[id]
	return t
}

// List returns all templates, sorted by ID for deterministic UI ordering.
func (s *TemplateStore) List() []TerminalTemplate {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := make([]string, 0, len(s.templates))
	for id := range s.templates {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]TerminalTemplate, 0, len(ids))
	for _, id := range ids {
		out = append(out, hydrate(id, s.templates[id]))
	}
	return out
}

// Get looks up a template by ID.
func (s *TemplateStore) Get(id string) (TerminalTemplate, bool) {
	s.mu.RLock()
	t, ok := s.templates[id]
	s.mu.RUnlock()
	if !ok {
		return TerminalTemplate{}, false
	}
	return hydrate(id, t), true
}

// Template mutation (create/update/delete) is no longer served by relayLLM:
// relay's config editor edits the `pty` section of settings.json directly and
// restarts the service. TemplateStore is now read-mostly — it loads templates
// at startup (seeding defaults on first run) and serves List/Get.
