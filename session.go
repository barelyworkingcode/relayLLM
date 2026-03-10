package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session represents an active LLM conversation.
type Session struct {
	ID            string          `json:"sessionId"`
	ProjectID     string          `json:"projectId"`
	Name          string          `json:"name"`
	Directory     string          `json:"directory"`
	Model         string          `json:"model"`
	CreatedAt     string          `json:"createdAt"`
	Messages      []Message       `json:"messages"`
	Stats         SessionStats    `json:"stats"`
	ProviderState json.RawMessage `json:"providerState,omitempty"`

	provider   Provider
	processing bool
	mu         sync.Mutex
}

// SessionStore handles session persistence to disk.
type SessionStore struct {
	dir string
}

func NewSessionStore(dir string) *SessionStore {
	return &SessionStore{dir: dir}
}

// EventSink receives events from sessions and routes them to clients.
type EventSink interface {
	SendToSession(sessionID string, msg map[string]interface{})
}

// SessionManager manages all active sessions.
type SessionManager struct {
	mu           sync.RWMutex
	sessions     map[string]*Session
	projectStore *ProjectStore
	sessionStore *SessionStore
	perms        *PermissionManager
	sink         EventSink
	hookURL      string
}

func NewSessionManager(projects *ProjectStore, sessionStore *SessionStore, perms *PermissionManager) *SessionManager {
	return &SessionManager{
		sessions:     make(map[string]*Session),
		projectStore: projects,
		sessionStore: sessionStore,
		perms:        perms,
	}
}

func (m *SessionManager) SetEventSink(sink EventSink) {
	m.sink = sink
}

func (m *SessionManager) SetHookURL(url string) {
	m.hookURL = url
}

func (m *SessionManager) CreateSession(projectID, name, model string) (*Session, error) {
	project, ok := m.projectStore.Get(projectID)
	if !ok {
		return nil, fmt.Errorf("project not found: %s", projectID)
	}

	if model == "" {
		model = project.Model
	}
	if name == "" {
		name = "New Session"
	}

	session := &Session{
		ID:        uuid.New().String(),
		ProjectID: projectID,
		Name:      name,
		Directory: project.Path,
		Model:     model,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Messages:  []Message{},
		Stats:     SessionStats{},
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	// Initialize the provider.
	if err := m.initProvider(session); err != nil {
		m.mu.Lock()
		delete(m.sessions, session.ID)
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to start provider: %w", err)
	}

	slog.Info("session created", "id", session.ID, "project", project.Name, "model", model)
	return session, nil
}

func (m *SessionManager) initProvider(session *Session) error {
	handler := func(eventType string, data json.RawMessage) {
		m.handleProviderEvent(session, eventType, data)
	}

	provider := NewClaudeProvider(session, handler, m.hookURL)
	if session.ProviderState != nil {
		provider.RestoreState(session.ProviderState)
	}

	session.provider = provider
	return provider.Start()
}

func (m *SessionManager) handleProviderEvent(session *Session, eventType string, data json.RawMessage) {
	if m.sink == nil {
		return
	}

	switch eventType {
	case "llm_event":
		m.sink.SendToSession(session.ID, map[string]interface{}{
			"type":      "llm_event",
			"sessionId": session.ID,
			"event":     json.RawMessage(data),
		})

	case "stats_update":
		var stats SessionStats
		if err := json.Unmarshal(data, &stats); err == nil {
			session.mu.Lock()
			session.Stats.InputTokens += stats.InputTokens
			session.Stats.OutputTokens += stats.OutputTokens
			session.Stats.CacheReadTokens += stats.CacheReadTokens
			session.Stats.CacheCreationTokens += stats.CacheCreationTokens
			session.Stats.CostUsd = stats.CostUsd // total, not delta
			currentStats := session.Stats
			session.mu.Unlock()

			m.sink.SendToSession(session.ID, map[string]interface{}{
				"type":      "stats_update",
				"sessionId": session.ID,
				"stats":     currentStats,
			})
		}

	case "message_complete":
		session.mu.Lock()
		session.processing = false
		session.mu.Unlock()

		m.sink.SendToSession(session.ID, map[string]interface{}{
			"type":      "message_complete",
			"sessionId": session.ID,
		})

		// Persist session state.
		m.saveSession(session)

	case "process_exited":
		m.sink.SendToSession(session.ID, map[string]interface{}{
			"type":      "process_exited",
			"sessionId": session.ID,
		})

	case "raw_output":
		m.sink.SendToSession(session.ID, map[string]interface{}{
			"type":      "raw_output",
			"sessionId": session.ID,
			"text":      string(data),
		})

	case "error":
		m.sink.SendToSession(session.ID, map[string]interface{}{
			"type":      "error",
			"sessionId": session.ID,
			"message":   string(data),
		})
	}
}

func (m *SessionManager) SendMessage(sessionID, text string, files []FileAttachment) error {
	m.mu.RLock()
	session, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	session.mu.Lock()
	if session.processing {
		session.mu.Unlock()
		return fmt.Errorf("session is already processing a message")
	}
	session.processing = true
	session.mu.Unlock()

	// Restart provider if dead.
	if session.provider == nil || !session.provider.Alive() {
		if err := m.initProvider(session); err != nil {
			session.mu.Lock()
			session.processing = false
			session.mu.Unlock()
			return fmt.Errorf("failed to restart provider: %w", err)
		}
	}

	// Persist user message.
	contentJSON, _ := json.Marshal(text)
	session.mu.Lock()
	session.Messages = append(session.Messages, Message{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Role:      "user",
		Content:   contentJSON,
		Files:     files,
	})
	session.mu.Unlock()

	return session.provider.SendMessage(text, files)
}

// SendMessageSync sends a message and waits for the complete response.
// Used by HTTP API for non-streaming clients (relayTelegram, relayScheduler).
func (m *SessionManager) SendMessageSync(sessionID, text string, files []FileAttachment) (string, SessionStats, error) {
	collector := NewResponseCollector()

	// Temporarily redirect events to the collector.
	m.mu.RLock()
	session, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return "", SessionStats{}, fmt.Errorf("session not found: %s", sessionID)
	}

	collector.Install(session, m)

	if err := m.SendMessage(sessionID, text, files); err != nil {
		collector.Uninstall()
		return "", SessionStats{}, err
	}

	response, stats, err := collector.Wait(5 * time.Minute)
	collector.Uninstall()
	return response, stats, err
}

func (m *SessionManager) GetSession(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *SessionManager) ListSessions() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]map[string]interface{}, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, map[string]interface{}{
			"id":        s.ID,
			"projectId": s.ProjectID,
			"name":      s.Name,
			"directory": s.Directory,
			"model":     s.Model,
			"active":    s.provider != nil && s.provider.Alive(),
		})
	}
	return list
}

func (m *SessionManager) EndSession(id string) {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.sessions, id)
	m.mu.Unlock()

	if session.provider != nil {
		session.provider.Kill()
	}

	m.saveSession(session)
	slog.Info("session ended", "id", id)
}

func (m *SessionManager) StopAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		if s.provider != nil {
			s.provider.Kill()
		}
		m.saveSession(s)
	}
}

func (m *SessionManager) RestoreAll() {
	sessions, err := m.sessionStore.LoadAll()
	if err != nil {
		slog.Error("failed to load sessions", "error", err)
		return
	}

	m.mu.Lock()
	for _, s := range sessions {
		m.sessions[s.ID] = s
	}
	m.mu.Unlock()

	slog.Info("restored sessions", "count", len(sessions))
}

func (m *SessionManager) saveSession(session *Session) {
	session.mu.Lock()
	if session.provider != nil {
		session.ProviderState = session.provider.GetState()
	}
	session.mu.Unlock()

	if err := m.sessionStore.Save(session); err != nil {
		slog.Error("failed to save session", "id", session.ID, "error", err)
	}
}
