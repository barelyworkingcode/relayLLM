package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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
	Folder        string          `json:"folder,omitempty"` // UI-only grouping label within a project; empty = ungrouped
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
	// Project-scoped MCP tokens are NOT stored on the session. Relay is the
	// sole token authority: providers resolve the token just-in-time from
	// relay's bridge by ProjectID at spawn time (see resolveProjectToken).

	// Per-session Claude permission policy (parsed from Settings at create
	// time). PermissionMode is the live mode — may be mutated by
	// SetPermissionMode for mid-session toggle. Policy is the per-project
	// allow/deny rule set forwarded by Eve.
	PermissionMode string            `json:"permissionMode,omitempty"`
	Policy         *PermissionPolicy `json:"policy,omitempty"`

	// Host is non-nil when this session's project lives on an SSH host
	// (../relay/docs/ssh-hosts.md) rather than the console. Resolved via
	// relay's bridge at create time and re-resolved at each provider spawn;
	// the stored value is the fallback when the bridge is unavailable, so a
	// persisted host session survives a relayLLM restart.
	Host *HostSpec `json:"host,omitempty"`

	provider   Provider
	processing bool
	mu         sync.Mutex
}

// getHost returns Host, safe for concurrent use (Host is refreshed from a
// provider's spawn goroutine while other goroutines — WS join, ListSessions —
// may read it concurrently).
func (s *Session) getHost() *HostSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Host
}

// setHost sets Host, safe for concurrent use.
func (s *Session) setHost(h *HostSpec) {
	s.mu.Lock()
	s.Host = h
	s.mu.Unlock()
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
	llamaManager *ServerManager
	mlxManager   *ServerManager
	dataDir      string
	piConfig     *PiConfig

	routerPort    string
	routerHost    string
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

// SetLlamaManager injects the llama-server process manager. Pass nil to
// disable the llama.cpp provider.
func (m *SessionManager) SetLlamaManager(mgr *ServerManager) {
	m.llamaManager = mgr
}

// SetMlxManager injects the mlx-serve process manager. Pass nil to
// disable the MLX provider.
func (m *SessionManager) SetMlxManager(mgr *ServerManager) {
	m.mlxManager = mgr
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

// SetRouterHost records the relay-router's configured bind address
// (--router-bind). Used to build a URL the pi overlay can actually reach —
// see piOverlayInputs.
func (m *SessionManager) SetRouterHost(host string) {
	m.routerHost = host
}

func (m *SessionManager) SetProxyRegistry(r *ProxyRegistry) {
	m.proxyRegistry = r
}

// piOverlayInputs snapshots the inputs the pi overlay needs at spawn time.
// The Snapshot call may block ≤3s per stale endpoint after the registry's
// 15s TTL has expired.
func (m *SessionManager) piOverlayInputs() PiOverlayInputs {
	inputs := PiOverlayInputs{
		RouterPort: m.routerPort,
		RouterHost: m.routerHost,
	}
	// Copy managed models into a fresh slice — never append onto a manager's
	// config-owned slice (its spare capacity is shared; appending there races
	// concurrent callers). Aliases shadowed by a higher-priority manager
	// (router dispatch: llama before mlx) are skipped so the overlay only
	// advertises what the router actually serves.
	seen := make(map[string]bool)
	for _, mgr := range []*ServerManager{m.llamaManager, m.mlxManager} {
		if mgr == nil || mgr.config == nil {
			continue
		}
		for _, cfg := range mgr.config.Models {
			if seen[cfg.Alias] {
				continue
			}
			seen[cfg.Alias] = true
			inputs.ServerModels = append(inputs.ServerModels, cfg)
			// A configured mmproj is what makes the managed server accept
			// images — same signal the router's catalog reports.
			_, hasMmproj := cfg.Args["mmproj"]
			inputs.RouterModels = append(inputs.RouterModels, PiRouterModel{
				ID:             cfg.Alias,
				SupportsImages: hasMmproj,
			})
		}
	}
	if m.proxyRegistry != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, status := range m.proxyRegistry.Snapshot(ctx) {
			if !status.Online {
				continue
			}
			for _, m := range status.Models {
				// Only what the upstream advertised — plain OpenAI /v1/models
				// has no modality field, so a quiet endpoint reads as text.
				inputs.RouterModels = append(inputs.RouterModels, PiRouterModel{
					ID:             status.Endpoint.Name + "/" + m.ID,
					SupportsImages: m.SupportsImages,
				})
			}
		}
	}
	return inputs
}

// llamaConfig returns the ServerConfig from the llama manager, or nil if no
// manager is configured. Used by deriveProviderType for routing.
func (m *SessionManager) llamaConfig() *ServerConfig {
	if m.llamaManager == nil {
		return nil
	}
	return m.llamaManager.config
}

// mlxConfig returns the ServerConfig from the mlx manager, or nil if no
// manager is configured. Used by deriveProviderType for routing.
func (m *SessionManager) mlxConfig() *ServerConfig {
	if m.mlxManager == nil {
		return nil
	}
	return m.mlxManager.config
}

func (m *SessionManager) CreateSession(projectID, directory, name, model, systemPrompt string, appendClaudeMd bool, providerType string, settings json.RawMessage) (*Session, error) {
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
		providerType = deriveProviderType(model, m.openaiConfig, m.llamaConfig(), m.mlxConfig())
	}

	// Resolve the host (if any) up front, before any local directory access —
	// a host project's path lives on another machine and must never be
	// stat'd, realpath'd or created here (../relay/docs/ssh-hosts.md). Best
	// effort: a bridge failure degrades to "not a host" rather than blocking
	// every session create on relay's availability; a genuinely host-scoped
	// session that can't actually resolve fails later, at provider Start.
	var host *HostSpec
	if projectID != "" && serviceToken() != "" {
		if resp, err := resolveRelayPtyEnv(RelayPtyEnvRequest{ProjectID: projectID, Directory: dir}); err == nil {
			host = resp.Host
		} else {
			slog.Warn("resolve host for session create failed", "project", projectID, "error", err)
		}
	}

	// pi's overlay writes files into the project directory and symlinks into
	// the console's home — neither exists on a host (decision "pi provider on
	// a host" in ../relay/docs/ssh-hosts.md).
	if host != nil && providerType == "pi" {
		return nil, fmt.Errorf("provider \"pi\" is not available on a host project")
	}

	// For non-Claude providers, prepend CLAUDE.md content to system prompt if
	// requested. Skipped for a host project: <dir>/CLAUDE.md lives on the
	// host, not the console.
	if appendClaudeMd && providerType != "claude" && dir != "" && host == nil {
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
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Messages:       []Message{},
		Stats:          SessionStats{},
		Headless:       parsedSettings.Headless,
		PermissionMode: mode,
		Policy:         parsedSettings.PermissionPolicy,
		Host:           host,
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
// provider. "llama/{alias}" and "mlx/{alias}" route to their respective
// managed-server providers. Everything else falls through to Ollama's native
// provider.
func deriveProviderType(model string, openaiCfg *OpenAIConfig, llamaCfg, mlxCfg *ServerConfig) string {
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
		if prefix == "mlx" && mlxCfg.FindByAlias(model[idx+1:]) != nil {
			return "mlx"
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
		provider = NewBaseChatProvider(session, handler, transport, session.Settings, nil)

	case "openai":
		prefix, modelID, ok := strings.Cut(session.Model, "/")
		if !ok || modelID == "" {
			return fmt.Errorf("openai: model %q missing endpoint prefix", session.Model)
		}
		endpoint := m.openaiConfig.Find(prefix)
		if endpoint == nil {
			return fmt.Errorf("openai: unknown endpoint %q (model %q)", prefix, session.Model)
		}
		transport := NewOpenAIChatTransport(*endpoint, modelID, session.Settings, &http.Client{Transport: endpoint.Transport()})
		provider = NewBaseChatProvider(session, handler, transport, session.Settings, nil)

	case "llama", "mlx":
		mgr, kind := m.llamaManager, "llama"
		if session.ProviderType == "mlx" {
			mgr, kind = m.mlxManager, "mlx"
		}
		prefix, modelID, ok := strings.Cut(session.Model, "/")
		if !ok || modelID == "" || prefix != kind {
			return fmt.Errorf("%s: model %q must be %s/{alias}", kind, session.Model, kind)
		}
		if mgr == nil {
			return fmt.Errorf("%s: manager not configured", kind)
		}
		// Validate the alias now so a typo fails at session creation rather
		// than at the first message — the eager GetOrLaunch used to do this
		// as a side effect of launching.
		if !mgr.HasAlias(modelID) {
			return fmt.Errorf("%s: unknown model alias %q", kind, modelID)
		}

		// Resolve per turn rather than once here: the memory budget may evict
		// this model between messages, and it comes back on a new port. The
		// lease taken by each Acquire is what stops an eviction landing
		// mid-generation. Launching eagerly at session start would also pin a
		// model the user has not sent a message to yet.
		resolve := func() (OpenAIEndpoint, func(), error) {
			endpoint, release, err := mgr.Acquire(modelID)
			if err != nil {
				return OpenAIEndpoint{}, nil, err
			}
			return *endpoint, release, nil
		}
		transport := NewManagedChatTransport(resolve, modelID, session.Settings, nil)
		provider = NewBaseChatProvider(session, handler, transport, session.Settings, nil)

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
		// A host session has no PreToolUse hook (permissions ride the
		// control_request stream instead — see provider_claude.go) and its
		// directory lives on another machine, so writing .claude/settings.local.json
		// here would both be pointless and create a bogus local directory tree.
		if session.getHost() == nil {
			if err := m.ensureHookConfig(session.Directory); err != nil {
				slog.Warn("failed to write hook config", "dir", session.Directory, "error", err)
			}
		}
		p := NewClaudeProvider(session, handler, m.hookSocket, m.hookToken, m.perms)
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

		// Messages is mutated concurrently by the provider's tool loop
		// (see copyHistory in provider_chat_base.go), so it must only be
		// read under the session lock.
		s.mu.Lock()
		createdAt := s.CreatedAt
		messageCount := len(s.Messages)
		lastMsgAt := lastMessageAt(s.Messages)
		preview := sessionPreview(s.Messages)
		host := s.Host
		s.mu.Unlock()

		list = append(list, map[string]interface{}{
			"id":            s.ID,
			"projectId":     s.ProjectID,
			"name":          s.Name,
			"folder":        s.Folder,
			"directory":     s.Directory,
			"model":         s.Model,
			"active":        provider != nil && provider.Alive(),
			"createdAt":     createdAt,
			"messageCount":  messageCount,
			"lastMessageAt": lastMsgAt,
			"preview":       preview,
			"host":          host,
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
			// s was just deserialized by LoadAll and isn't reachable from
			// m.sessions yet, so no other goroutine can touch it — no lock
			// needed here.
			list = append(list, map[string]interface{}{
				"id":            s.ID,
				"projectId":     s.ProjectID,
				"name":          s.Name,
				"folder":        s.Folder,
				"directory":     s.Directory,
				"model":         s.Model,
				"active":        false,
				"createdAt":     s.CreatedAt,
				"messageCount":  len(s.Messages),
				"lastMessageAt": lastMessageAt(s.Messages),
				"preview":       sessionPreview(s.Messages),
				"host":          s.Host,
			})
		}
	}

	return list
}

// sessionPreview returns a short preview of the first substantive user turn
// in msgs: plain text, whitespace collapsed, capped at 120 runes (with "…"
// appended if truncated). Slash-command turns (e.g. "/compact") aren't
// conversation content, so they're skipped in favor of the first real user
// message. Returns "" if there is no such message.
func sessionPreview(msgs []Message) string {
	for _, msg := range msgs {
		if msg.Role != "user" {
			continue
		}
		text := strings.TrimSpace(extractTextContent(msg))
		if text == "" || strings.HasPrefix(text, "/") {
			continue
		}
		return truncatePreview(strings.Join(strings.Fields(text), " "), 120)
	}
	return ""
}

// truncatePreview caps s at max runes, appending "…" when it cuts content.
func truncatePreview(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// lastMessageAt returns the Timestamp of the last message in msgs, or "" if
// msgs is empty.
func lastMessageAt(msgs []Message) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].Timestamp
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

// SetSessionFolder updates the session's UI grouping label and persists.
// Unlike RenameSession it goes through GetSession (lazy-loads from disk), so it
// also works on inactive/persisted sessions a user organizes from the list.
// An empty folder means ungrouped.
func (m *SessionManager) SetSessionFolder(id, folder string) error {
	session, ok := m.GetSession(id)
	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}

	session.mu.Lock()
	session.Folder = folder
	session.mu.Unlock()

	m.saveSession(session)

	// Notify WS clients (any other viewers of this session).
	if m.sink != nil {
		m.sink.SendToSession(id, map[string]interface{}{
			"type":      WSMsgSessionFolderChanged,
			"sessionId": id,
			"folder":    folder,
		})
	}

	slog.Info("session folder changed", "id", id, "folder", folder)
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
