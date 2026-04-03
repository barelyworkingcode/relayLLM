package main

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// TerminalManager manages terminal session lifecycle.
type TerminalManager struct {
	mu        sync.RWMutex
	terminals map[string]*TerminalSession
	templates *TemplateStore

	onOutput func(terminalID string, data []byte)
	onExit   func(terminalID string, exitCode int)
}

func NewTerminalManager(templates *TemplateStore) *TerminalManager {
	return &TerminalManager{
		terminals: make(map[string]*TerminalSession),
		templates: templates,
	}
}

func (m *TerminalManager) SetOutputHandler(fn func(terminalID string, data []byte)) {
	m.onOutput = fn
}

func (m *TerminalManager) SetExitHandler(fn func(terminalID string, exitCode int)) {
	m.onExit = fn
}

// Create starts a new terminal session from a template.
func (m *TerminalManager) Create(templateID, name, directory string, cols, rows uint16) (*TerminalSession, error) {
	tmpl, ok := m.templates.Get(templateID)
	if !ok {
		return nil, fmt.Errorf("terminal template not found: %s", templateID)
	}

	if directory == "" {
		directory = defaultHomeDir()
	}

	if name == "" {
		name = tmpl.Name
	}

	// Idle timeout: use template setting or default to 24 hours.
	idleMinutes := tmpl.IdleTimeout
	if idleMinutes <= 0 {
		idleMinutes = 24 * 60 // 24 hours
	}

	session := &TerminalSession{
		ID:          uuid.New().String(),
		TemplateID:  templateID,
		Name:        name,
		Directory:   directory,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		cols:        cols,
		rows:        rows,
		idleTimeout: time.Duration(idleMinutes) * time.Minute,
		onOutput:    m.onOutput,
		onExit:      m.onExit,
		onIdle:      func(id string) { m.Close(id) },
	}

	if err := session.Start(tmpl); err != nil {
		return nil, fmt.Errorf("start terminal: %w", err)
	}

	m.mu.Lock()
	m.terminals[session.ID] = session
	m.mu.Unlock()

	slog.Info("terminal session created", "id", session.ID, "template", templateID, "directory", directory)
	return session, nil
}

// Get returns a terminal session by ID.
func (m *TerminalManager) Get(id string) (*TerminalSession, bool) {
	m.mu.RLock()
	s, ok := m.terminals[id]
	m.mu.RUnlock()
	return s, ok
}

// List returns metadata for all terminal sessions.
func (m *TerminalManager) List() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]map[string]interface{}, 0, len(m.terminals))
	for _, s := range m.terminals {
		state, exitCode := s.Snapshot()
		list = append(list, map[string]interface{}{
			"id":         s.ID,
			"templateId": s.TemplateID,
			"name":       s.Name,
			"directory":  s.Directory,
			"state":      state,
			"createdAt":  s.CreatedAt,
			"exitCode":   exitCode,
		})
	}
	return list
}

// Write sends input to a terminal.
func (m *TerminalManager) Write(id string, data []byte) error {
	m.mu.RLock()
	s, ok := m.terminals[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("terminal not found: %s", id)
	}
	return s.Write(data)
}

// Resize changes a terminal's PTY dimensions.
func (m *TerminalManager) Resize(id string, cols, rows uint16) error {
	m.mu.RLock()
	s, ok := m.terminals[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("terminal not found: %s", id)
	}
	return s.Resize(cols, rows)
}

// Close kills a terminal and removes it from the manager.
func (m *TerminalManager) Close(id string) {
	m.mu.Lock()
	s, ok := m.terminals[id]
	if ok {
		delete(m.terminals, id)
	}
	m.mu.Unlock()

	if ok {
		s.Close()
	}
}

// NotifyViewerChange is called when the viewer count for a terminal changes.
// When count drops to 0, the idle timer starts. When it goes above 0, the timer is cancelled.
func (m *TerminalManager) NotifyViewerChange(id string, viewers int) {
	m.mu.RLock()
	s, ok := m.terminals[id]
	m.mu.RUnlock()
	if !ok {
		return
	}

	if viewers == 0 {
		s.StartIdleTimer()
	} else {
		s.CancelIdleTimer()
	}
}

// ListTemplates returns all available terminal templates.
func (m *TerminalManager) ListTemplates() []TerminalTemplate {
	return m.templates.List()
}

// StopAll closes all running terminals. Called during shutdown.
func (m *TerminalManager) StopAll() {
	m.mu.Lock()
	sessions := make([]*TerminalSession, 0, len(m.terminals))
	for _, s := range m.terminals {
		sessions = append(sessions, s)
	}
	m.terminals = make(map[string]*TerminalSession)
	m.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}
}

func defaultHomeDir() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/tmp"
	}
	return home
}
