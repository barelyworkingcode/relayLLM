package main

import (
	"context"
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

	SystemPrompt  string `json:"systemPrompt,omitempty"`
	Headless      bool   `json:"headless,omitempty"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"` // pi-only: off/minimal/low/medium/high/xhigh
	McpToken      string `json:"-"`                       // project-scoped MCP token, not serialized

	// Per-session Claude permission policy (parsed from Settings at create
	// time). PermissionMode is the live mode — may be mutated by
	// SetPermissionMode for mid-session toggle. Policy is the per-project
	// allow/deny rule set forwarded by Eve.
	PermissionMode string            `json:"permissionMode,omitempty"`
	Policy         *PermissionPolicy `json:"policy,omitempty"`

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
	sessionStore *SessionStore
	perms        *PermissionManager
	sink         EventSink
	hookSocket   string
	hookToken    string
	ollamaURL    string
	openaiConfig *OpenAIConfig
	llamaManager *LlamaServerManager
	builtinTools         *BuiltinToolRegistry
	dataDir              string
	piConfig             *PiConfig
	comfyuiEnabled       bool   // true when /api/generate-image is mounted; toggles pi skill+env wiring
	piImageGenSkillDir   string // dir to add to pi's skills array when comfyui is enabled

	routerPort    string
	proxyRegistry *ProxyRegistry

	// providerFactory, when non-nil, fully replaces the built-in provider
	// switch in initProvider. Test-only seam — production never sets this.
	providerFactory func(session *Session, handler EventHandler) (Provider, error)

	// mcpClientFactory, when non-nil, overrides the MCPClient that
	// BaseChatProvider would otherwise build from session settings. Lets
	// tests inject a FakeMCP without going through the settings JSON.
	// Test-only seam — production never sets this.
	mcpClientFactory func(session *Session) MCPClient
}

// SetProviderFactory installs a function that constructs the Provider for a
// new session. When set, it short-circuits the built-in switch on
// session.ProviderType. Test-only.
func (m *SessionManager) SetProviderFactory(f func(*Session, EventHandler) (Provider, error)) {
	m.providerFactory = f
}

// SetMCPClientFactory installs a function that produces the MCPClient for
// each chat-based session. When set, BaseChatProvider's settings-driven MCP
// is replaced after construction. Test-only.
func (m *SessionManager) SetMCPClientFactory(f func(*Session) MCPClient) {
	m.mcpClientFactory = f
}

func NewSessionManager(sessionStore *SessionStore, perms *PermissionManager) *SessionManager {
	return &SessionManager{
		sessions:     make(map[string]*Session),
		collectors:   make(map[string]*ResponseCollector),
		sessionStore: sessionStore,
		perms:        perms,
	}
}

func (m *SessionManager) SetEventSink(sink EventSink) {
	m.sink = sink
}

// SetHookSocket sets the Unix socket the hook subprocess dials for
// /api/permission. Same uid + 0600 perms + token authenticate the call.
func (m *SessionManager) SetHookSocket(socketPath string) {
	m.hookSocket = socketPath
}

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

// SetComfyUIEnabled records whether /api/generate-image is mounted.
// When true, the pi overlay attaches its image-gen skill + the
// RELAY_LLM_SOCKET/RELAY_LLM_TOKEN env vars pi's bash+curl path needs.
// Claude sessions reach image-gen via the relay MCP proxy instead, so
// they aren't gated by this flag.
func (m *SessionManager) SetComfyUIEnabled(enabled bool) {
	m.comfyuiEnabled = enabled
}

// SetPiImageGenSkillDir records the dir holding the pi image-gen SKILL.md.
// Empty leaves pi's skill set unchanged. The pi overlay appends this dir
// to settings.json's skills array on every spawn.
func (m *SessionManager) SetPiImageGenSkillDir(dir string) {
	m.piImageGenSkillDir = dir
}

// SetLlamaManager injects the llama-server process manager. Pass nil to
// disable the llama.cpp provider.
func (m *SessionManager) SetLlamaManager(mgr *LlamaServerManager) {
	m.llamaManager = mgr
}

// SetDataDir records relayLLM's data directory. The pi provider stores its
// session JSONLs under {dataDir}/pi-sessions so cleanup is predictable.
func (m *SessionManager) SetDataDir(dir string) {
	m.dataDir = dir
}

// SetPiConfig injects the pi.dev provider configuration (binary path,
// extra args, …). Pass nil for defaults.
func (m *SessionManager) SetPiConfig(cfg *PiConfig) {
	m.piConfig = cfg
}

func (m *SessionManager) SetRouterPort(port string) {
	m.routerPort = port
}

func (m *SessionManager) SetProxyRegistry(r *ProxyRegistry) {
	m.proxyRegistry = r
}

// piOverlayInputs snapshots the inputs the pi overlay needs at spawn time.
// The Snapshot call may block ≤3s per stale endpoint after the registry's
// 15s TTL has expired.
func (m *SessionManager) piOverlayInputs() PiOverlayInputs {
	inputs := PiOverlayInputs{
		RouterPort:       m.routerPort,
		ImageGenSkillDir: m.piImageGenSkillDir,
		RelayLLMSocket:   m.hookSocket, // hookSocket is the same Unix socket pi can dial
		RelayLLMToken:    m.hookToken,
		HasImageGen:      m.comfyuiEnabled,
	}
	if m.llamaManager != nil && m.llamaManager.config != nil {
		inputs.LlamaModels = m.llamaManager.config.Models
		for _, cfg := range m.llamaManager.config.Models {
			inputs.RouterModels = append(inputs.RouterModels, cfg.Alias)
		}
	}
	if m.proxyRegistry != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, status := range m.proxyRegistry.Snapshot(ctx) {
			if !status.Online {
				continue
			}
			for _, id := range status.Models {
				inputs.RouterModels = append(inputs.RouterModels, status.Endpoint.Name+"/"+id)
			}
		}
	}
	return inputs
}

// llamaConfig returns the LlamaConfig from the manager, or nil if no
// manager is configured. Used by deriveProviderType for routing.
func (m *SessionManager) llamaConfig() *LlamaConfig {
	if m.llamaManager == nil {
		return nil
	}
	return m.llamaManager.config
}

func (m *SessionManager) CreateSession(projectID, directory, name, model, systemPrompt string, appendClaudeMd bool, providerType string, settings json.RawMessage, mcpToken string) (*Session, error) {
	if directory == "" {
		return nil, fmt.Errorf("directory is required")
	}
	dir := directory

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
		Headless         bool              `json:"headless"`
		PermissionMode   string            `json:"permissionMode"`
		PermissionPolicy *PermissionPolicy `json:"permissionPolicy"`
	}
	if settings != nil {
		json.Unmarshal(settings, &parsedSettings)
	}

	// permissionMode is the live mode used at spawn time. Headless is a
	// legacy synonym for "bypassPermissions" and stays the dominant signal
	// when set, so existing call sites keep working.
	mode := parsedSettings.PermissionMode
	if parsedSettings.Headless {
		mode = "bypassPermissions"
	}
	if parsedSettings.PermissionPolicy != nil && mode == "" {
		mode = parsedSettings.PermissionPolicy.DefaultMode
	}

	session := &Session{
		ID:             uuid.New().String(),
		ProjectID:      projectID,
		Name:           name,
		Directory:      dir,
		Model:          model,
		ProviderType:   providerType,
		Settings:       settings,
		SystemPrompt:   systemPrompt,
		McpToken:       mcpToken,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Messages:       []Message{},
		Stats:          SessionStats{},
		Headless:       parsedSettings.Headless,
		PermissionMode: mode,
		Policy:         parsedSettings.PermissionPolicy,
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
	if strings.HasPrefix(model, "pi/") {
		return "pi"
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

	if m.providerFactory != nil {
		provider, err := m.providerFactory(session, handler)
		if err != nil {
			return err
		}
		session.setProvider(provider)
		return provider.Start()
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

	case "pi":
		// Expected model format: pi/<provider>/<modelId>
		rest := strings.TrimPrefix(session.Model, "pi/")
		upstreamProvider, modelID, ok := strings.Cut(rest, "/")
		if !ok || upstreamProvider == "" || modelID == "" {
			return fmt.Errorf("pi: model %q malformed (want pi/<provider>/<modelId>)", session.Model)
		}
		// Parse pi-specific session settings so thinkingLevel set on
		// CreateSession lands on the Session before Start() reads it.
		if session.ThinkingLevel == "" && session.Settings != nil {
			var s struct {
				ThinkingLevel string `json:"thinkingLevel"`
			}
			if json.Unmarshal(session.Settings, &s) == nil {
				session.ThinkingLevel = s.ThinkingLevel
			}
		}
		p := NewPiProvider(session, handler, upstreamProvider, modelID, m.dataDir, m.piConfig, m.piOverlayInputs())
		if session.ProviderState != nil {
			p.RestoreState(session.ProviderState)
		}
		provider = p

	default: // "claude" or unset (backward compat)
		if err := m.ensureHookConfig(session.Directory); err != nil {
			slog.Warn("failed to write hook config", "dir", session.Directory, "error", err)
		}
		p := NewClaudeProvider(session, handler, m.hookSocket, m.hookToken)
		if session.ProviderState != nil {
			p.RestoreState(session.ProviderState)
		}
		provider = p
	}

	// Test-only MCP injection: replace the settings-driven MCP client on
	// chat-based providers before Start runs (Start dials MCP servers).
	if m.mcpClientFactory != nil {
		if bp, ok := provider.(*BaseChatProvider); ok {
			bp.SetMCPClient(m.mcpClientFactory(session))
		}
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
	case HandlerLLMEvent:
		msg = map[string]interface{}{
			"type":      HandlerLLMEvent,
			"sessionId": session.ID,
			"event":     json.RawMessage(data),
		}

	case HandlerStatsUpdate:
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
			"type":      HandlerStatsUpdate,
			"sessionId": session.ID,
			"stats":     currentStats,
		}

	case HandlerMessageComplete:
		session.mu.Lock()
		session.processing = false
		session.mu.Unlock()

		// Contract: all providers persist their own assistant turns (chat-base
		// via turnStreamState.blocks, pi via allBlocks, Claude via its CLI's
		// JSONL replayed by readClaudeHistory). message_complete data is
		// always nil; there is no fallback save path.
		msg = map[string]interface{}{
			"type":      HandlerMessageComplete,
			"sessionId": session.ID,
		}

		m.saveSession(session)

	case "process_exited":
		session.mu.Lock()
		session.processing = false
		session.mu.Unlock()

		msg = map[string]interface{}{
			"type":      WSMsgProcessExited,
			"sessionId": session.ID,
		}

		m.saveSession(session)

	case "raw_output":
		msg = map[string]interface{}{
			"type":      WSMsgRawOutput,
			"sessionId": session.ID,
			"text":      string(data),
		}

	case "error":
		session.mu.Lock()
		session.processing = false
		session.mu.Unlock()

		msg = map[string]interface{}{
			"type":      WSMsgError,
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
			"type":      WSMsgUserMessage,
			"sessionId": session.ID,
			"text":      text,
		})
	}

	if err := provider.SendMessage(text, files); err != nil {
		session.mu.Lock()
		session.processing = false
		// Roll back the just-appended user message when it carried attachments
		// and the provider rejected it synchronously — otherwise a rejected
		// image stays in history and poisons every subsequent request.
		if len(files) > 0 {
			if n := len(session.Messages); n > 0 && session.Messages[n-1].Role == "user" {
				session.Messages = session.Messages[:n-1]
			}
		}
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
	m.handleProviderEvent(session, HandlerMessageComplete, nil)
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
			"type":      WSMsgClearMessages,
			"sessionId": id,
		})
		m.sink.SendToSession(id, map[string]interface{}{
			"type":      HandlerStatsUpdate,
			"sessionId": id,
			"stats":     SessionStats{},
		})
		m.sink.SendToSession(id, map[string]interface{}{
			"type":      WSMsgSystemMessage,
			"sessionId": id,
			"message":   "Conversation history cleared",
		})
	}

	slog.Info("session cleared", "id", id)
	return nil
}

// SetPiModel switches a pi-backed session to a different model mid-flight.
// Returns an error if the session isn't pi-backed or the underlying RPC
// rejects the change.
func (m *SessionManager) SetPiModel(id, upstreamProvider, modelID string) error {
	session, ok := m.GetSession(id)
	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}
	if session.ProviderType != "pi" {
		return fmt.Errorf("model switch is only supported for pi sessions")
	}
	pi, ok := session.getProvider().(*PiProvider)
	if !ok {
		return fmt.Errorf("session has no live pi provider")
	}
	if err := pi.SetModel(upstreamProvider, modelID); err != nil {
		return err
	}
	m.saveSession(session)
	if m.sink != nil {
		m.sink.SendToSession(id, map[string]interface{}{
			"type":      WSMsgModelChanged,
			"sessionId": id,
			"model":     session.Model,
		})
	}
	return nil
}

// SetPiThinkingLevel changes pi's reasoning depth mid-session.
func (m *SessionManager) SetPiThinkingLevel(id, level string) error {
	session, ok := m.GetSession(id)
	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}
	if session.ProviderType != "pi" {
		return fmt.Errorf("thinking level is only supported for pi sessions")
	}
	pi, ok := session.getProvider().(*PiProvider)
	if !ok {
		return fmt.Errorf("session has no live pi provider")
	}
	if err := pi.SetThinkingLevel(level); err != nil {
		return err
	}
	m.saveSession(session)
	if m.sink != nil {
		m.sink.SendToSession(id, map[string]interface{}{
			"type":          WSMsgThinkingLevelChanged,
			"sessionId":     id,
			"thinkingLevel": level,
		})
	}
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
			"type":      WSMsgSessionRenamed,
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
