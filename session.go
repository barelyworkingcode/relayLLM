package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	ProviderType  string          `json:"providerType"`
	Settings      json.RawMessage `json:"settings,omitempty"`
	CreatedAt     string          `json:"createdAt"`
	Messages      []Message       `json:"messages"`
	Stats         SessionStats    `json:"stats"`
	ProviderState json.RawMessage `json:"providerState,omitempty"`

	SystemPrompt string `json:"systemPrompt,omitempty"`
	Headless     bool   `json:"headless,omitempty"`

	provider   Provider
	processing bool
	mu         sync.Mutex
}

// getProvider returns the current provider, safe for concurrent use.
func (s *Session) getProvider() Provider {
	s.mu.Lock()
	p := s.provider
	s.mu.Unlock()
	return p
}

// setProvider sets the provider, safe for concurrent use.
func (s *Session) setProvider(p Provider) {
	s.mu.Lock()
	s.provider = p
	s.mu.Unlock()
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
	collectors   map[string]*ResponseCollector // sessionID → active collector
	projectStore *ProjectStore
	sessionStore *SessionStore
	perms        *PermissionManager
	sink         EventSink
	hookURL      string
	hookToken    string
	ollamaURL    string
	openaiConfig *OpenAIConfig
	llamaManager *LlamaServerManager
	builtinTools *BuiltinToolRegistry
}

func NewSessionManager(projects *ProjectStore, sessionStore *SessionStore, perms *PermissionManager) *SessionManager {
	return &SessionManager{
		sessions:     make(map[string]*Session),
		collectors:   make(map[string]*ResponseCollector),
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

// SetHookToken sets the bearer token the permission hook binary will send
// when POSTing to /api/permission. Must match the relayLLM API token so the
// hook can authenticate against its own parent server.
func (m *SessionManager) SetHookToken(token string) {
	m.hookToken = token
}

func (m *SessionManager) SetOllamaURL(url string) {
	m.ollamaURL = url
}

// SetOpenAIConfig injects the OpenAI-compatible endpoint config. Pass nil to
// disable all OpenAI-compatible providers.
func (m *SessionManager) SetOpenAIConfig(cfg *OpenAIConfig) {
	m.openaiConfig = cfg
}

// SetBuiltinTools injects the registry of built-in tools (e.g. generate_image)
// that will be available to all Ollama/OpenAI sessions alongside MCP tools.
func (m *SessionManager) SetBuiltinTools(r *BuiltinToolRegistry) {
	m.builtinTools = r
}

// SetLlamaManager injects the llama-server process manager. Pass nil to
// disable the llama.cpp provider.
func (m *SessionManager) SetLlamaManager(mgr *LlamaServerManager) {
	m.llamaManager = mgr
}

// llamaConfig returns the LlamaConfig from the manager, or nil if no
// manager is configured. Used by deriveProviderType for routing.
func (m *SessionManager) llamaConfig() *LlamaConfig {
	if m.llamaManager == nil {
		return nil
	}
	return m.llamaManager.config
}

func (m *SessionManager) CreateSession(projectID, directory, name, model, systemPrompt string, appendClaudeMd bool, providerType string, settings json.RawMessage) (*Session, error) {
	var dir string

	if projectID != "" {
		project, ok := m.projectStore.Get(projectID)
		if !ok {
			return nil, fmt.Errorf("project not found: %s", projectID)
		}
		dir = project.Path
	} else {
		// Ungrouped session - directory is required
		if directory == "" {
			return nil, fmt.Errorf("directory is required for sessions without a project")
		}
		dir = directory
	}

	if model == "" {
		model = "sonnet"
	}
	if name == "" {
		name = "New Session"
	}

	if providerType == "" {
		providerType = deriveProviderType(model, m.openaiConfig, m.llamaConfig())
	}

	// For non-Claude providers, prepend CLAUDE.md content to system prompt if requested.
	if appendClaudeMd && providerType != "claude" && dir != "" {
		if content, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); err == nil {
			if systemPrompt != "" {
				systemPrompt = string(content) + "\n---\n" + systemPrompt
			} else {
				systemPrompt = string(content)
			}
		}
	}

	var parsedSettings struct {
		Headless bool `json:"headless"`
	}
	if settings != nil {
		json.Unmarshal(settings, &parsedSettings)
	}

	session := &Session{
		ID:           uuid.New().String(),
		ProjectID:    projectID,
		Name:         name,
		Directory:    dir,
		Model:        model,
		ProviderType: providerType,
		Settings:     settings,
		SystemPrompt: systemPrompt,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		Messages:     []Message{},
		Stats:        SessionStats{},
		Headless:     parsedSettings.Headless,
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

	slog.Info("session created", "id", session.ID, "project", projectID, "model", model)
	return session, nil
}

// deriveProviderType picks a provider for the given model identifier.
//
// Claude model aliases (haiku/sonnet/opus) route to the Claude subprocess
// provider. A model of the form "{endpoint}/{model-id}" where {endpoint} is
// a configured OpenAI-compatible endpoint routes to the generic openai
// provider. Everything else falls through to Ollama's native provider.
func deriveProviderType(model string, openaiCfg *OpenAIConfig, llamaCfg *LlamaConfig) string {
	switch model {
	case "haiku", "sonnet", "opus":
		return "claude"
	}
	if idx := strings.Index(model, "/"); idx > 0 {
		prefix := model[:idx]
		if prefix == "llama" && llamaCfg.FindByAlias(model[idx+1:]) != nil {
			return "llama"
		}
		if openaiCfg.Find(prefix) != nil {
			return "openai"
		}
	}
	return "ollama"
}

func (m *SessionManager) initProvider(session *Session) error {
	handler := func(eventType string, data json.RawMessage) {
		m.handleProviderEvent(session, eventType, data)
	}

	var provider Provider

	switch session.ProviderType {
	case "ollama":
		transport := NewOllamaChatTransport(m.ollamaURL, session.Model, session.Settings, nil)
		provider = NewBaseChatProvider(session, handler, transport, session.Settings, m.builtinTools)

	case "openai":
		prefix, modelID, ok := strings.Cut(session.Model, "/")
		if !ok || modelID == "" {
			return fmt.Errorf("openai: model %q missing endpoint prefix", session.Model)
		}
		endpoint := m.openaiConfig.Find(prefix)
		if endpoint == nil {
			return fmt.Errorf("openai: unknown endpoint %q (model %q)", prefix, session.Model)
		}
		transport := NewOpenAIChatTransport(*endpoint, modelID, session.Settings, nil)
		provider = NewBaseChatProvider(session, handler, transport, session.Settings, m.builtinTools)

	case "llama":
		_, modelID, ok := strings.Cut(session.Model, "/")
		if !ok || modelID == "" {
			return fmt.Errorf("llama: model %q missing llama/ prefix", session.Model)
		}
		if m.llamaManager == nil {
			return fmt.Errorf("llama: manager not configured")
		}
		endpoint, err := m.llamaManager.GetOrLaunch(modelID)
		if err != nil {
			return fmt.Errorf("llama: %w", err)
		}
		transport := NewOpenAIChatTransport(*endpoint, modelID, session.Settings, nil)
		provider = NewBaseChatProvider(session, handler, transport, session.Settings, m.builtinTools)

	default: // "claude" or unset (backward compat)
		if err := m.ensureHookConfig(session.Directory); err != nil {
			slog.Warn("failed to write hook config", "dir", session.Directory, "error", err)
		}
		p := NewClaudeProvider(session, handler, m.hookURL, m.hookToken)
		if session.ProviderState != nil {
			p.RestoreState(session.ProviderState)
		}
		provider = p
	}

	session.setProvider(provider)
	return provider.Start()
}

// ensureHookConfig writes .claude/settings.local.json in the project directory
// to register the hook binary as a PreToolUse hook. Without this, Claude CLI
// has no idea the hook exists and will never invoke it.
func (m *SessionManager) ensureHookConfig(projectDir string) error {
	hookPath, err := resolveHookPath()
	if err != nil {
		return fmt.Errorf("resolve hook path: %w", err)
	}

	claudeDir := filepath.Join(projectDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}

	settingsPath := filepath.Join(claudeDir, "settings.local.json")

	// Read existing settings to preserve other config.
	var settings map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = make(map[string]interface{})
	}

	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = make(map[string]interface{})
	}
	hooks["PreToolUse"] = []interface{}{
		map[string]interface{}{
			"matcher": "",
			"hooks": []interface{}{
				map[string]interface{}{
					"type":    "command",
					"command": hookPath,
					"timeout": 120,
				},
			},
		},
	}
	settings["hooks"] = hooks

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}

	slog.Info("hook config written", "path", settingsPath, "hookBinary", hookPath)
	return nil
}

// resolveHookPath returns the absolute path to the hook binary,
// located at cmd/hook/hook relative to the relayLLM executable.
func resolveHookPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	hookPath := filepath.Join(filepath.Dir(exe), "cmd", "hook", "hook")
	if _, err := os.Stat(hookPath); err != nil {
		return "", fmt.Errorf("hook binary not found at %s", hookPath)
	}
	return hookPath, nil
}

func (m *SessionManager) handleProviderEvent(session *Session, eventType string, data json.RawMessage) {
	// Build the message once.
	var msg map[string]interface{}

	switch eventType {
	case "llm_event":
		msg = map[string]interface{}{
			"type":      "llm_event",
			"sessionId": session.ID,
			"event":     json.RawMessage(data),
		}

	case "stats_update":
		var stats SessionStats
		if err := json.Unmarshal(data, &stats); err != nil {
			return
		}
		session.mu.Lock()
		session.Stats.InputTokens = stats.InputTokens
		session.Stats.OutputTokens = stats.OutputTokens
		session.Stats.CacheReadTokens = stats.CacheReadTokens
		session.Stats.CacheCreationTokens = stats.CacheCreationTokens
		session.Stats.CostUsd = stats.CostUsd
		session.Stats.TimeToFirstToken = stats.TimeToFirstToken
		session.Stats.TokensPerSecond = stats.TokensPerSecond
		session.Stats.PromptEvalCount = stats.PromptEvalCount
		session.Stats.EvalDurationMs = stats.EvalDurationMs
		session.Stats.PromptEvalDurationMs = stats.PromptEvalDurationMs
		currentStats := session.Stats
		session.mu.Unlock()

		msg = map[string]interface{}{
			"type":      "stats_update",
			"sessionId": session.ID,
			"stats":     currentStats,
		}

	case "message_complete":
		session.mu.Lock()
		session.processing = false
		session.mu.Unlock()

		// If provider sent text with message_complete (LM Studio), store assistant message.
		if data != nil {
			var complete struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(data, &complete) == nil && complete.Text != "" {
				contentJSON, _ := json.Marshal([]map[string]string{{"type": "text", "text": complete.Text}})
				session.mu.Lock()
				session.Messages = append(session.Messages, Message{
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Role:      "assistant",
					Content:   contentJSON,
				})
				session.mu.Unlock()
			}
		}

		msg = map[string]interface{}{
			"type":      "message_complete",
			"sessionId": session.ID,
		}

		// Persist session state.
		m.saveSession(session)

	case "process_exited":
		session.mu.Lock()
		session.processing = false
		session.mu.Unlock()

		msg = map[string]interface{}{
			"type":      "process_exited",
			"sessionId": session.ID,
		}

		m.saveSession(session)

	case "raw_output":
		msg = map[string]interface{}{
			"type":      "raw_output",
			"sessionId": session.ID,
			"text":      string(data),
		}

	case "error":
		session.mu.Lock()
		session.processing = false
		session.mu.Unlock()

		msg = map[string]interface{}{
			"type":      "error",
			"sessionId": session.ID,
			"message":   string(data),
		}

	default:
		return
	}

	// Route to collector if one is registered for this session.
	m.mu.RLock()
	collector := m.collectors[session.ID]
	m.mu.RUnlock()

	if collector != nil {
		collector.HandleEvent(msg)
	}

	// Always forward to the main sink (WebSocket clients).
	if m.sink != nil {
		m.sink.SendToSession(session.ID, msg)
	}
}

func (m *SessionManager) SendMessage(sessionID, text string, files []FileAttachment) error {
	session, ok := m.GetSession(sessionID)
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
	provider := session.getProvider()
	if provider == nil || !provider.Alive() {
		if err := m.initProvider(session); err != nil {
			session.mu.Lock()
			session.processing = false
			session.mu.Unlock()
			return fmt.Errorf("failed to restart provider: %w", err)
		}
		provider = session.getProvider()
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

	// Broadcast user message to all viewers so passive windows can render it
	// and transition to the "generating" UI state before the first LLM token.
	if m.sink != nil {
		m.sink.SendToSession(session.ID, map[string]interface{}{
			"type":      "user_message",
			"sessionId": session.ID,
			"text":      text,
		})
	}

	if err := provider.SendMessage(text, files); err != nil {
		session.mu.Lock()
		session.processing = false
		session.mu.Unlock()
		return err
	}
	return nil
}

// StopGeneration aborts the in-flight response for a session.
func (m *SessionManager) StopGeneration(sessionID string) error {
	m.mu.RLock()
	session, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if provider := session.getProvider(); provider != nil {
		provider.StopGeneration()
	}

	// Emit message_complete so clients know the turn is definitively over.
	// provider.StopGeneration() already incremented the generation counter,
	// so the old goroutine's events (including its own message_complete)
	// are silently discarded — no double-delivery.
	m.handleProviderEvent(session, "message_complete", nil)
	return nil
}

// SendMessageSync sends a message and waits for the complete response.
// Used by HTTP API for non-streaming clients (relayTelegram, relayScheduler).
func (m *SessionManager) SendMessageSync(sessionID, text string, files []FileAttachment) (string, SessionStats, error) {
	collector := NewResponseCollector()

	// Ensure session is loaded (lazy-load from disk if needed).
	if _, ok := m.GetSession(sessionID); !ok {
		return "", SessionStats{}, fmt.Errorf("session not found: %s", sessionID)
	}

	// Register collector for this session.
	m.mu.Lock()
	m.collectors[sessionID] = collector
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.collectors, sessionID)
		m.mu.Unlock()
	}()

	if err := m.SendMessage(sessionID, text, files); err != nil {
		return "", SessionStats{}, err
	}

	return collector.Wait(5 * time.Minute)
}

func (m *SessionManager) GetSession(id string) (*Session, bool) {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if ok {
		return s, true
	}

	// Lazy-load from disk.
	s, err := m.sessionStore.Load(id)
	if err != nil {
		return nil, false
	}

	m.mu.Lock()
	// Check again in case another goroutine loaded it.
	if existing, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		return existing, true
	}
	m.sessions[id] = s
	m.mu.Unlock()

	slog.Info("session restored from disk", "id", id)
	return s, true
}

func (m *SessionManager) ListSessions() []map[string]interface{} {
	// Merge in-memory sessions with persisted sessions from disk.
	m.mu.RLock()
	seen := make(map[string]bool, len(m.sessions))
	list := make([]map[string]interface{}, 0, len(m.sessions))
	for _, s := range m.sessions {
		seen[s.ID] = true
		if s.Headless {
			continue
		}
		provider := s.getProvider()
		list = append(list, map[string]interface{}{
			"id":        s.ID,
			"projectId": s.ProjectID,
			"name":      s.Name,
			"directory": s.Directory,
			"model":     s.Model,
			"active":    provider != nil && provider.Alive(),
		})
	}
	m.mu.RUnlock()

	// Add persisted sessions not already in memory.
	persisted, err := m.sessionStore.LoadAll()
	if err == nil {
		for _, s := range persisted {
			if seen[s.ID] || s.Headless {
				continue
			}
			list = append(list, map[string]interface{}{
				"id":        s.ID,
				"projectId": s.ProjectID,
				"name":      s.Name,
				"directory": s.Directory,
				"model":     s.Model,
				"active":    false,
			})
		}
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

	if provider := session.getProvider(); provider != nil {
		provider.Kill()
	}

	m.saveSession(session)
	slog.Info("session ended", "id", id)
}

// DeleteSession kills the provider, removes from memory, and deletes persisted file.
func (m *SessionManager) DeleteSession(id string) {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	if ok {
		if provider := session.getProvider(); provider != nil {
			if err := provider.DeleteSession(); err != nil {
				slog.Warn("failed to delete provider session data", "id", id, "error", err)
			}
			provider.Kill()
		}
	}

	// Always delete the persisted file — the session may have been saved
	// to disk by EndSession but removed from memory.
	if err := m.sessionStore.Delete(id); err != nil {
		slog.Warn("failed to delete session file", "id", id, "error", err)
	}

	slog.Info("session deleted", "id", id)
}

// ClearSession kills the provider, clears messages/stats, and restarts the provider.
func (m *SessionManager) ClearSession(id string) error {
	m.mu.RLock()
	session, ok := m.sessions[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}

	// Kill existing provider
	session.mu.Lock()
	provider := session.provider
	session.provider = nil
	session.Messages = []Message{}
	session.Stats = SessionStats{}
	session.ProviderState = nil
	session.processing = false
	session.mu.Unlock()

	if provider != nil {
		provider.Kill()
	}

	// Persist cleared state
	m.saveSession(session)

	// Restart provider
	if err := m.initProvider(session); err != nil {
		return fmt.Errorf("failed to restart provider: %w", err)
	}

	// Send clear events to WS client
	if m.sink != nil {
		m.sink.SendToSession(id, map[string]interface{}{
			"type":      "clear_messages",
			"sessionId": id,
		})
		m.sink.SendToSession(id, map[string]interface{}{
			"type":      "stats_update",
			"sessionId": id,
			"stats":     SessionStats{},
		})
		m.sink.SendToSession(id, map[string]interface{}{
			"type":      "system_message",
			"sessionId": id,
			"message":   "Conversation history cleared",
		})
	}

	slog.Info("session cleared", "id", id)
	return nil
}

// RenameSession updates the session name in memory and persists.
func (m *SessionManager) RenameSession(id, name string) error {
	m.mu.RLock()
	session, ok := m.sessions[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}

	session.mu.Lock()
	session.Name = name
	session.mu.Unlock()

	m.saveSession(session)

	// Notify WS clients
	if m.sink != nil {
		m.sink.SendToSession(id, map[string]interface{}{
			"type":      "session_renamed",
			"sessionId": id,
			"name":      name,
		})
	}

	slog.Info("session renamed", "id", id, "name", name)
	return nil
}

func (m *SessionManager) StopAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		if provider := s.getProvider(); provider != nil {
			provider.Kill()
		}
		m.saveSession(s)
	}
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
