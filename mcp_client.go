package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPServerConfig mirrors the JSON config format for a single MCP server.
type MCPServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// MCPTool pairs a tool definition with its source server for call routing.
type MCPTool struct {
	ServerName string
	Name       string
	Tool       *mcp.Tool
}

// MCPClient is the surface that BaseChatProvider needs from MCP. Decoupling
// chat_base from the concrete MCPManager lets tests substitute a fake without
// spawning real subprocesses.
type MCPClient interface {
	Start(ctx context.Context) error
	HasTools() bool
	ToolCount() int
	ChatToolDefs() []map[string]interface{}
	CallTool(ctx context.Context, name string, arguments json.RawMessage, onProgress func(message string)) (string, error)
	ToolNames() []string
	ServerNames() []string
	Close()
}

// mcpServerConn holds a single MCP server's active session.
type mcpServerConn struct {
	session *mcp.ClientSession
}

// MCPManager manages MCP server connections and tool routing for a session.
type MCPManager struct {
	configs map[string]MCPServerConfig
	servers map[string]*mcpServerConn
	tools   []MCPTool
	toolMap map[string]string // tool name → server name
	mu      sync.Mutex

	// progress maps an in-flight call's progressToken → its per-call sink.
	// The shared client's ProgressNotificationHandler routes inbound
	// notifications/progress here. Guarded by its own mutex so a progress
	// callback (fired on the SDK's read goroutine) never contends with the
	// main connection/tool lock held during Start/Close.
	progressMu sync.Mutex
	progress   map[string]func(message string)
	progressID atomic.Int64
}

// NewMCPManager creates a manager from MCP server configs. Does not connect yet.
func NewMCPManager(configs map[string]MCPServerConfig) *MCPManager {
	return &MCPManager{
		configs:  configs,
		servers:  make(map[string]*mcpServerConn),
		toolMap:  make(map[string]string),
		progress: make(map[string]func(message string)),
	}
}

// newMCPClient builds the shared MCP client. Its ProgressNotificationHandler
// routes inbound notifications/progress to the in-flight call's sink (registered
// by CallTool) so MCP tools stream status to the frontend exactly like the
// in-process builtin tools do. Extracted from Start so a hermetic test can wire
// this same client (and thus the real progress routing) to an in-memory server.
func (m *MCPManager) newMCPClient() *mcp.Client {
	return mcp.NewClient(&mcp.Implementation{
		Name:    "relayLLM",
		Version: "1.0.0",
	}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			if req == nil || req.Params == nil || req.Params.Message == "" {
				return
			}
			token := progressTokenString(req.Params.ProgressToken)
			m.progressMu.Lock()
			fn := m.progress[token]
			m.progressMu.Unlock()
			if fn != nil {
				fn(req.Params.Message)
			}
		},
	})
}

// Start connects to all configured MCP servers and discovers their tools.
func (m *MCPManager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	client := m.newMCPClient()

	for name, cfg := range m.configs {
		if cfg.Command == "" {
			slog.Warn("mcp: skipping server with empty command", "server", name)
			continue
		}

		cmd := exec.Command(cfg.Command, cfg.Args...)
		// Inherit environment (minus relay's own credentials), then overlay
		// config-specific vars. childBaseEnv strips the service/frontend tokens
		// so the relay mcp child gets only the project token we set in cfg.Env.
		cmd.Env = childBaseEnv()
		for k, v := range cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stderr = os.Stderr

		transport := &mcp.CommandTransport{Command: cmd}
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			slog.Error("mcp: failed to connect to server", "server", name, "error", err)
			continue
		}

		m.servers[name] = &mcpServerConn{session: session}

		result, err := session.ListTools(ctx, nil)
		if err != nil {
			slog.Error("mcp: failed to list tools", "server", name, "error", err)
			continue
		}

		for _, tool := range result.Tools {
			m.tools = append(m.tools, MCPTool{
				ServerName: name,
				Name:       tool.Name,
				Tool:       tool,
			})
			m.toolMap[tool.Name] = name
		}

		slog.Info("mcp: server connected", "server", name, "tools", len(result.Tools))
	}

	if len(m.servers) == 0 {
		return fmt.Errorf("mcp: no servers connected")
	}

	slog.Info("mcp: ready", "servers", len(m.servers), "tools", len(m.tools))
	return nil
}

// ChatToolDefs converts discovered tools into the {type:"function",
// function:{name, description, parameters}} shape accepted by both Ollama's
// /api/chat and the OpenAI /chat/completions protocol.
func (m *MCPManager) ChatToolDefs() []map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.tools) == 0 {
		return nil
	}

	defs := make([]map[string]interface{}, 0, len(m.tools))
	for _, t := range m.tools {
		fn := map[string]interface{}{
			"name":        t.Name,
			"description": t.Tool.Description,
		}
		if t.Tool.InputSchema != nil {
			fn["parameters"] = t.Tool.InputSchema
		}
		defs = append(defs, map[string]interface{}{
			"type":     "function",
			"function": fn,
		})
	}
	return defs
}

// CallTool executes a tool by name via the appropriate MCP server. If
// onProgress is non-nil, a progressToken is attached so the server streams
// notifications/progress back, each delivered to onProgress as it arrives.
func (m *MCPManager) CallTool(ctx context.Context, name string, arguments json.RawMessage, onProgress func(message string)) (string, error) {
	m.mu.Lock()
	serverName, ok := m.toolMap[name]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("mcp: unknown tool %q", name)
	}
	conn, ok := m.servers[serverName]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("mcp: server %q not connected", serverName)
	}
	m.mu.Unlock()

	// Parse arguments from json.RawMessage into map[string]any for the SDK.
	var args map[string]any
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return "", fmt.Errorf("mcp: unmarshal arguments: %w", err)
		}
	}

	params := &mcp.CallToolParams{Name: name, Arguments: args}
	if onProgress != nil {
		token := fmt.Sprintf("relayllm-%d", m.progressID.Add(1))
		params.Meta = mcp.Meta{"progressToken": token}
		m.progressMu.Lock()
		m.progress[token] = onProgress
		m.progressMu.Unlock()
		defer func() {
			m.progressMu.Lock()
			delete(m.progress, token)
			m.progressMu.Unlock()
		}()
	}

	result, err := conn.session.CallTool(ctx, params)
	if err != nil {
		return "", fmt.Errorf("mcp: call %q: %w", name, err)
	}

	return extractToolResultText(result), nil
}

// progressTokenString normalizes a JSON progressToken (string or number) to
// the string key relayLLM uses to correlate progress to its originating call.
func progressTokenString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// HasTools returns true if any tools were discovered.
func (m *MCPManager) HasTools() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tools) > 0
}

// ToolCount returns the number of discovered tools without allocating.
func (m *MCPManager) ToolCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tools)
}

// ToolNames returns the discovered tool names. Used by chat_base to populate
// system.init events.
func (m *MCPManager) ToolNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.tools))
	for _, t := range m.tools {
		out = append(out, t.Name)
	}
	return out
}

// ServerNames returns the names of currently-connected MCP servers.
func (m *MCPManager) ServerNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.servers))
	for name := range m.servers {
		out = append(out, name)
	}
	return out
}

// Close shuts down all MCP server connections.
func (m *MCPManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, conn := range m.servers {
		if err := conn.session.Close(); err != nil {
			slog.Warn("mcp: close error", "server", name, "error", err)
		}
	}
	m.servers = make(map[string]*mcpServerConn)
	m.tools = nil
	m.toolMap = make(map[string]string)
}

// extractToolResultText extracts text from a CallToolResult's content array.
func extractToolResultText(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, c := range result.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			sb.WriteString(v.Text)
		default:
			// For non-text content, marshal as JSON.
			data, err := json.Marshal(c)
			if err == nil {
				sb.WriteString(string(data))
			}
		}
	}
	return sb.String()
}
