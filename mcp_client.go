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
}

// NewMCPManager creates a manager from MCP server configs. Does not connect yet.
func NewMCPManager(configs map[string]MCPServerConfig) *MCPManager {
	return &MCPManager{
		configs: configs,
		servers: make(map[string]*mcpServerConn),
		toolMap: make(map[string]string),
	}
}

// Start connects to all configured MCP servers and discovers their tools.
func (m *MCPManager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "relayLLM",
		Version: "1.0.0",
	}, nil)

	for name, cfg := range m.configs {
		if cfg.Command == "" {
			slog.Warn("mcp: skipping server with empty command", "server", name)
			continue
		}

		cmd := exec.Command(cfg.Command, cfg.Args...)
		// Inherit environment, then overlay config-specific vars.
		cmd.Env = os.Environ()
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

// CallTool executes a tool by name via the appropriate MCP server.
func (m *MCPManager) CallTool(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
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

	result, err := conn.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return "", fmt.Errorf("mcp: call %q: %w", name, err)
	}

	return extractToolResultText(result), nil
}

// HasTools returns true if any tools were discovered.
func (m *MCPManager) HasTools() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tools) > 0
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
