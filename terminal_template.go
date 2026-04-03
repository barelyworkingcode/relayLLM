package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
)

// TerminalTemplate defines a launchable terminal type.
type TerminalTemplate struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
	Icon        string            `json:"icon,omitempty"`
	BuiltIn     bool              `json:"builtIn"`
	IdleTimeout int               `json:"idleTimeout,omitempty"` // minutes, 0 = default (1440 = 24h)
}

// ResolveCommand returns the absolute path to the command, checking
// well-known locations before falling back to PATH lookup.
func (t TerminalTemplate) ResolveCommand() string {
	switch t.ID {
	case "claude-code":
		return resolveClaudePath()
	case "shell":
		return resolveShell()
	default:
		if p, err := exec.LookPath(t.Command); err == nil {
			return p
		}
		return t.Command
	}
}

func builtinTemplates() []TerminalTemplate {
	return []TerminalTemplate{
		{
			ID:          "claude-code",
			Name:        "Claude Code",
			Command:     "claude",
			Args:        []string{},
			Description: "Claude Code CLI agent",
			Icon:        "terminal",
			BuiltIn:     true,
		},
		{
			ID:          "opencode",
			Name:        "OpenCode",
			Command:     "opencode",
			Args:        []string{},
			Description: "OpenCode CLI agent",
			Icon:        "terminal",
			BuiltIn:     true,
		},
		{
			ID:          "shell",
			Name:        "Shell",
			Command:     "",
			Args:        []string{},
			Description: "Default system shell",
			Icon:        "shell",
			BuiltIn:     true,
		},
	}
}

// resolveShell returns the user's default shell.
func resolveShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/zsh"
}

type templateFile struct {
	Templates []TerminalTemplate `json:"templates"`
}

// TemplateStore manages terminal templates with JSON file persistence.
type TemplateStore struct {
	mu     sync.RWMutex
	path   string
	custom []TerminalTemplate
}

func NewTemplateStore(path string) *TemplateStore {
	return &TemplateStore{path: path, custom: []TerminalTemplate{}}
}

func (s *TemplateStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f templateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	s.custom = f.Templates
	return nil
}

func (s *TemplateStore) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(templateFile{Templates: s.custom}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

// List returns built-in templates merged with custom templates.
func (s *TemplateStore) List() []TerminalTemplate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]TerminalTemplate, 0, len(builtinTemplates())+len(s.custom))
	result = append(result, builtinTemplates()...)
	result = append(result, s.custom...)
	return result
}

// Get looks up a template by ID, checking built-in first then custom.
func (s *TemplateStore) Get(id string) (TerminalTemplate, bool) {
	for _, t := range builtinTemplates() {
		if t.ID == id {
			return t, true
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.custom {
		if t.ID == id {
			return t, true
		}
	}
	return TerminalTemplate{}, false
}

// Create adds a new custom terminal template.
func (s *TemplateStore) Create(tmpl TerminalTemplate) (TerminalTemplate, error) {
	if tmpl.Name == "" || tmpl.Command == "" {
		return TerminalTemplate{}, fmt.Errorf("name and command are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tmpl.ID = uuid.New().String()
	tmpl.BuiltIn = false
	if tmpl.Args == nil {
		tmpl.Args = []string{}
	}
	s.custom = append(s.custom, tmpl)
	return tmpl, s.save()
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
	for _, t := range builtinTemplates() {
		if t.ID == id {
			return TerminalTemplate{}, fmt.Errorf("cannot update built-in template: %s", id)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.custom {
		if s.custom[i].ID != id {
			continue
		}
		if u.Name != nil && *u.Name != "" {
			s.custom[i].Name = *u.Name
		}
		if u.Command != nil && *u.Command != "" {
			s.custom[i].Command = *u.Command
		}
		if u.Description != nil {
			s.custom[i].Description = *u.Description
		}
		if u.Icon != nil {
			s.custom[i].Icon = *u.Icon
		}
		if u.Args != nil {
			s.custom[i].Args = *u.Args
		}
		if u.Env != nil {
			s.custom[i].Env = *u.Env
		}
		return s.custom[i], s.save()
	}
	return TerminalTemplate{}, fmt.Errorf("template not found: %s", id)
}

// Delete removes a custom terminal template. Built-in templates cannot be deleted.
func (s *TemplateStore) Delete(id string) error {
	for _, t := range builtinTemplates() {
		if t.ID == id {
			return fmt.Errorf("cannot delete built-in template: %s", id)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]TerminalTemplate, 0, len(s.custom))
	found := false
	for _, t := range s.custom {
		if t.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, t)
	}
	if !found {
		return fmt.Errorf("template not found: %s", id)
	}
	s.custom = filtered
	return s.save()
}

