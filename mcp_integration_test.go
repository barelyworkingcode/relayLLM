package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestIntegration_MCPToolCalling verifies end-to-end MCP tool calling:
// Ollama asks for tools → relayLLM discovers them via MCP → model invokes
// a tool → relayLLM executes it → result fed back → model responds.
//
// Prerequisites:
//   - Running Ollama with gemma4:latest pulled
//   - Relay MCP server binary at /Applications/Relay.app/Contents/MacOS/relay
//   - RELAY_TOKEN env var set
//
// Run: RELAY_TOKEN=<token> go test -v -run TestIntegration_MCPToolCalling -timeout 5m
func TestIntegration_MCPToolCalling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MCP integration test (requires Ollama + Relay MCP)")
	}

	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}

	if !checkEndpoint(ollamaURL+"/api/tags", "") {
		t.Skip("Ollama not reachable at " + ollamaURL)
	}

	relayBinary := "/Applications/Relay.app/Contents/MacOS/relay"
	if _, err := os.Stat(relayBinary); err != nil {
		t.Skipf("Relay binary not found at %s", relayBinary)
	}

	relayToken := os.Getenv("RELAY_TOKEN")
	if relayToken == "" {
		t.Skip("RELAY_TOKEN not set")
	}

	ts := newTestServer(t)
	ts.Sessions.SetOllamaURL(ollamaURL)

	settings := map[string]interface{}{
		"temperature": 0.1,
		"num_ctx":     32768,
		"mcpServers": map[string]interface{}{
			"relay": map[string]interface{}{
				"command": relayBinary,
				"args":    []string{"mcp"},
				"env": map[string]string{
					"RELAY_TOKEN": relayToken,
				},
			},
		},
	}
	settingsJSON, _ := json.Marshal(settings)

	model := os.Getenv("BENCHMARK_OLLAMA_MODEL")
	if model == "" {
		model = "gemma4:latest"
	}

	session, err := ts.Sessions.CreateSession(
		"", t.TempDir(), "mcp-test",
		model,
		"You are a helpful assistant with access to tools. When asked about emails, use the available tools to fetch them. Always use tools when they are relevant to the request.",
		false, "ollama", settingsJSON,
	)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	t.Logf("session created: %s (model=%s)", session.ID, model)

	response, stats, err := ts.Sessions.SendMessageSync(
		session.ID,
		`Get my most recent email from my Inbox. Show me the subject and sender. Use the mail_get_emails tool.`,
		nil,
	)
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	t.Logf("Response:\n%s", response)
	t.Logf("Stats: in=%d out=%d tps=%.1f", stats.InputTokens, stats.OutputTokens, stats.TokensPerSecond)

	if response == "" {
		t.Fatal("empty response")
	}

	// Verify the model actually used tools (not just claimed it can't).
	lower := strings.ToLower(response)
	if strings.Contains(lower, "i don't have access") ||
		strings.Contains(lower, "i cannot access") ||
		strings.Contains(lower, "i'm unable to") {
		t.Fatalf("model did not use tools, got: %s", truncate(response, 200))
	}

	// Verify tool messages exist in session history.
	session.mu.Lock()
	msgCount := len(session.Messages)
	hasToolMsg := false
	hasToolCallAssistant := false
	for _, msg := range session.Messages {
		if msg.Role == "tool" {
			hasToolMsg = true
		}
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			hasToolCallAssistant = true
		}
	}
	session.mu.Unlock()

	t.Logf("Messages in history: %d (hasToolMsg=%v hasToolCallAssistant=%v)", msgCount, hasToolMsg, hasToolCallAssistant)

	if !hasToolMsg {
		t.Error("expected at least one tool result message in session history")
	}
	if !hasToolCallAssistant {
		t.Error("expected at least one assistant message with tool_calls in session history")
	}
}

// TestIntegration_MCPToolDiscovery verifies that MCP server connection
// and tool discovery works without sending any messages.
func TestIntegration_MCPToolDiscovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MCP discovery test")
	}

	relayBinary := "/Applications/Relay.app/Contents/MacOS/relay"
	if _, err := os.Stat(relayBinary); err != nil {
		t.Skipf("Relay binary not found at %s", relayBinary)
	}

	relayToken := os.Getenv("RELAY_TOKEN")
	if relayToken == "" {
		t.Skip("RELAY_TOKEN not set")
	}

	configs := map[string]MCPServerConfig{
		"relay": {
			Command: relayBinary,
			Args:    []string{"mcp"},
			Env:     map[string]string{"RELAY_TOKEN": relayToken},
		},
	}

	mgr := NewMCPManager(configs)
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("start MCP manager: %v", err)
	}
	defer mgr.Close()

	if !mgr.HasTools() {
		t.Fatal("expected at least one tool to be discovered")
	}

	defs := mgr.ChatToolDefs()
	t.Logf("Discovered %d tools:", len(defs))
	for _, def := range defs {
		fn, _ := def["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		t.Logf("  - %s: %s", name, truncate(desc, 80))
	}
}
