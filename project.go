package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ChatTemplate defines a reusable session preset for a project.
type ChatTemplate struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Model          string          `json:"model"`
	Mode           string          `json:"mode"`                            // "text" | "voice"
	Voice          string          `json:"voice,omitempty"`                 // voice ID for voice mode
	SystemPrompt   string          `json:"systemPrompt,omitempty"`
	AppendClaudeMd bool            `json:"appendClaudeMd,omitempty"`        // inject CLAUDE.md into system prompt (non-Claude only)
	Settings       json.RawMessage `json:"settings,omitempty"`              // provider-specific (temp, MCP tools, etc.)
}

type Project struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Path          string         `json:"path"`
	AllowedTools  []string       `json:"allowedTools"`
	ChatTemplates []ChatTemplate `json:"chatTemplates,omitempty"`
	CreatedAt     string         `json:"createdAt"`
}

type projectFile struct {
	Projects []Project `json:"projects"`
}

type ProjectStore struct {
	mu       sync.RWMutex
	path     string
	projects []Project
}

func NewProjectStore(path string) *ProjectStore {
	return &ProjectStore{path: path, projects: []Project{}}
}

func (s *ProjectStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f projectFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	s.projects = f.Projects
	return nil
}

func (s *ProjectStore) save() error {
	data, err := json.MarshalIndent(projectFile{Projects: s.projects}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

func (s *ProjectStore) List() []Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Project, len(s.projects))
	copy(out, s.projects)
	return out
}

func (s *ProjectStore) Get(id string) (Project, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.projects {
		if p.ID == id {
			return p, true
		}
	}
	return Project{}, false
}

func (s *ProjectStore) Create(name, path string, allowedTools []string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if name == "" || path == "" {
		return Project{}, fmt.Errorf("name and path are required")
	}
	if allowedTools == nil {
		allowedTools = []string{}
	}

	p := Project{
		ID:           uuid.New().String(),
		Name:         name,
		Path:         path,
		AllowedTools: allowedTools,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	s.projects = append(s.projects, p)
	return p, s.save()
}

func (s *ProjectStore) Update(id string, updates map[string]interface{}) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.projects {
		if s.projects[i].ID != id {
			continue
		}
		if v, ok := updates["name"].(string); ok && v != "" {
			s.projects[i].Name = v
		}
		if v, ok := updates["path"].(string); ok && v != "" {
			s.projects[i].Path = v
		}
		if v, ok := updates["allowedTools"]; ok {
			if tools, ok := v.([]interface{}); ok {
				strs := make([]string, 0, len(tools))
				for _, t := range tools {
					if str, ok := t.(string); ok {
						strs = append(strs, str)
					}
				}
				s.projects[i].AllowedTools = strs
			}
		}
		if v, ok := updates["chatTemplates"]; ok {
			raw, err := json.Marshal(v)
			if err != nil {
				return Project{}, fmt.Errorf("invalid chatTemplates: %w", err)
			}
			var templates []ChatTemplate
			if err := json.Unmarshal(raw, &templates); err != nil {
				return Project{}, fmt.Errorf("invalid chatTemplates: %w", err)
			}
			s.projects[i].ChatTemplates = templates
		}
		return s.projects[i], s.save()
	}
	return Project{}, fmt.Errorf("project not found: %s", id)
}

func (s *ProjectStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]Project, 0, len(s.projects))
	found := false
	for _, p := range s.projects {
		if p.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, p)
	}
	if !found {
		return fmt.Errorf("project not found: %s", id)
	}
	s.projects = filtered
	return s.save()
}
