package main

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// TerminalManager manages terminal session lifecycle.
type TerminalManager struct {
	mu        sync.RWMutex
	terminals map[string]*TerminalSession
	templates *TemplateStore

	// logDir is where each session's head/tail log files live. Empty disables
	// disk logging (used by tests that don't need replay).
	logDir string

	// piConfig + overlayInputsFn supply the project-overlay shape for any
	// terminal template whose command is `pi`. The PTY launcher consults
	// these only when the template carries a RelayManagedSpec; non-pi or
	// non-relay-managed templates spawn exactly as before.
	piConfig        *PiConfig
	overlayInputsFn func() PiOverlayInputs

	onOutput func(terminalID string, data []byte)
	onExit   func(terminalID string, exitCode int)
}

// NewTerminalManager constructs a manager. logDir is the directory under
// which per-session log files are written; pass "" to disable disk logging.
func NewTerminalManager(templates *TemplateStore, logDir string) *TerminalManager {
	return &TerminalManager{
		terminals: make(map[string]*TerminalSession),
		templates: templates,
		logDir:    logDir,
	}
}

// LogDir returns the directory where per-session log files are written. Used
// by the /api/terminals/{id}/log handler to read post-exit replays.
func (m *TerminalManager) LogDir() string {
	return m.logDir
}

// SetPiOverlay supplies the pi project-overlay config + inputs accessor so
// PTY templates that run `pi` against a relay-managed directory get the same
// per-project models.json/settings.json/auth.json the LLM provider uses. Pass
// (nil, nil) to disable the overlay for PTY sessions.
func (m *TerminalManager) SetPiOverlay(cfg *PiConfig, inputsFn func() PiOverlayInputs) {
	m.piConfig = cfg
	m.overlayInputsFn = inputsFn
}

func (m *TerminalManager) SetOutputHandler(fn func(terminalID string, data []byte)) {
	m.onOutput = fn
}

func (m *TerminalManager) SetExitHandler(fn func(terminalID string, exitCode int)) {
	m.onExit = fn
}

// Create starts a new terminal session from a template. extraArgs is
// appended to the template's resolved Args (after substitution) — used by
// scheduled tasks to pass per-task commands through a shared template. A
// non-empty projectID makes the terminal project-scoped: relay resolves a
// project-scoped token (validating directory against the project) and injects
// it as RELAY_PROJECT_TOKEN. An empty projectID yields a token-free terminal.
func (m *TerminalManager) Create(templateID, name, directory, projectID string, cols, rows uint16, extraArgs []string) (*TerminalSession, error) {
	tmpl, ok := m.templates.Get(templateID)
	if !ok {
		// Global-store miss. If this is a project-scoped launch, the template may
		// be private to the project — resolve its definition from relay over the
		// bridge (global-first precedence: a global id always wins). An ad-hoc
		// terminal with no project can't have a private template, so fail fast.
		if projectID == "" {
			return nil, fmt.Errorf("terminal template not found: %s", templateID)
		}
		pt, err := resolveRelayProjectTemplate(projectID, templateID)
		if err != nil {
			return nil, fmt.Errorf("resolve project template %s: %w", templateID, err)
		}
		tmpl = pt
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
		ID:              uuid.New().String(),
		TemplateID:      templateID,
		Name:            name,
		Directory:       directory,
		projectID:       projectID,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		cols:            cols,
		rows:            rows,
		idleTimeout:     time.Duration(idleMinutes) * time.Minute,
		extraArgs:       extraArgs,
		logDir:          m.logDir,
		piConfig:        m.piConfig,
		overlayInputsFn: m.overlayInputsFn,
		onOutput:        m.onOutput,
		onExit:          m.onExit,
		onIdle:          func(id string) { m.Close(id) },
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

// TerminalSummary is the slim per-row shape relay's Service Inspector
// renders. `id` feeds the manifest-declared `stop-terminal` action's
// {id} placeholder; the other fields are display-only.
type TerminalSummary struct {
	ID         string `json:"id"`
	TemplateID string `json:"templateId"` // matches the key used by List()/Eve
	Name       string `json:"name"`
	State      string `json:"state"`     // "running" or "stopped"
	StartedAt  string `json:"startedAt"` // RFC3339; UI renders as relative time
}

// ListSummary returns one TerminalSummary per session, sorted by ID for
// stable UI iteration. Distinct from List() (which Eve consumes with a
// richer per-terminal payload) so neither caller's wire shape constrains
// the other.
func (m *TerminalManager) ListSummary() []TerminalSummary {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]TerminalSummary, 0, len(m.terminals))
	for _, s := range m.terminals {
		state, _ := s.Snapshot()
		out = append(out, TerminalSummary{
			ID:         s.ID,
			TemplateID: s.TemplateID,
			Name:       s.Name,
			State:      state,
			StartedAt:  s.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
