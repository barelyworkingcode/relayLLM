package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"

	"github.com/google/uuid"
)

// TerminalTemplate defines a launchable terminal type.
//
// On disk inside settings.json's `pty` map the entries omit `id` (the map key
// IS the id) and `builtIn` (computed from protectedTemplateIDs at API time).
// In-memory copies returned by Get/List have both populated for consumers.
//
// Relay-managed fields (UseRelayToken, AutoRegenSkills, SkillPath, EnvPassthrough)
// opt the template into spawn-time resolution via relay's bridge ResolvePtyEnv.
// Args may reference ${SKILL_PATH}, ${RELAY_TOKEN}, ${PROJECT_PATH}; SkillPath
// may reference ${project.path}. See terminal_session.go:Start for the
// substitution rules.
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
	UseRelayToken   bool     `json:"useRelayToken,omitempty"`
	AutoRegenSkills string   `json:"autoRegenSkills,omitempty"` // "always" | "skipIfExists" | "never"
	SkillPath       string   `json:"skillPath,omitempty"`       // supports ${project.path}
	EnvPassthrough  []string `json:"env_passthrough,omitempty"`
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
			Name:            "Claude Code",
			Command:         "claude",
			Icon:            "terminal",
			Description:     "Claude Code CLI agent (relay-managed: token + skills injected at launch)",
			UseRelayToken:   true,
			AutoRegenSkills: AutoRegenAlways,
			SkillPath:       "${project.path}/.claude/skills/relay",
			EnvPassthrough:  []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"},
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

// Create adds a new custom terminal template.
func (s *TemplateStore) Create(tmpl TerminalTemplate) (TerminalTemplate, error) {
	if tmpl.Name == "" || tmpl.Command == "" {
		return TerminalTemplate{}, fmt.Errorf("name and command are required")
	}

	tmpl.ID = ""
	tmpl.BuiltIn = false
	if tmpl.Args == nil {
		tmpl.Args = []string{}
	}
	id := uuid.New().String()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.templates[id] = tmpl
	if err := s.persist(); err != nil {
		delete(s.templates, id)
		return TerminalTemplate{}, err
	}
	return hydrate(id, tmpl), nil
}

// TemplateUpdate holds optional fields for partial template updates.
type TemplateUpdate struct {
	Name        *string            `json:"name,omitempty"`
	Command     *string            `json:"command,omitempty"`
	Description *string            `json:"description,omitempty"`
	Icon        *string            `json:"icon,omitempty"`
	Args        *[]string          `json:"args,omitempty"`
	Env         *map[string]string `json:"env,omitempty"`
}

// Update modifies an existing custom template.
func (s *TemplateStore) Update(id string, u TemplateUpdate) (TerminalTemplate, error) {
	if protectedTemplateIDs[id] {
		return TerminalTemplate{}, fmt.Errorf("cannot update built-in template: %s", id)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.templates[id]
	if !ok {
		return TerminalTemplate{}, fmt.Errorf("template not found: %s", id)
	}
	original := t

	if u.Name != nil && *u.Name != "" {
		t.Name = *u.Name
	}
	if u.Command != nil && *u.Command != "" {
		t.Command = *u.Command
	}
	if u.Description != nil {
		t.Description = *u.Description
	}
	if u.Icon != nil {
		t.Icon = *u.Icon
	}
	if u.Args != nil {
		t.Args = *u.Args
	}
	if u.Env != nil {
		t.Env = *u.Env
	}
	s.templates[id] = t

	if err := s.persist(); err != nil {
		s.templates[id] = original
		return TerminalTemplate{}, err
	}
	return hydrate(id, t), nil
}

// Delete removes a custom terminal template. Built-in templates cannot be deleted.
func (s *TemplateStore) Delete(id string) error {
	if protectedTemplateIDs[id] {
		return fmt.Errorf("cannot delete built-in template: %s", id)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	original, ok := s.templates[id]
	if !ok {
		return fmt.Errorf("template not found: %s", id)
	}
	delete(s.templates, id)

	if err := s.persist(); err != nil {
		s.templates[id] = original
		return err
	}
	return nil
}
