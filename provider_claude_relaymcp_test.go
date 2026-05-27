package main

import (
	"encoding/json"
	"testing"
)

// TestClaudeRelayMCPConfig_Disabled: no opt-in → no --mcp-config flag.
// Sessions without useRelayTools must not have a relay MCP injected,
// otherwise users without a project token would see Claude CLI spawn
// fail at startup.
func TestClaudeRelayMCPConfig_Disabled(t *testing.T) {
	p := &ClaudeProvider{session: &Session{}}
	if got := p.relayMCPConfigJSON(); got != "" {
		t.Errorf("disabled config = %q, want empty", got)
	}
}

// TestClaudeRelayMCPConfig_NoToken: opt-in without a project token is
// a misconfiguration; the provider must refuse to inject the MCP so
// the failure is loud (log line) instead of silent (claude sees the
// 'relay' MCP server, can't auth, every tool call fails opaquely).
func TestClaudeRelayMCPConfig_NoToken(t *testing.T) {
	t.Setenv("RELAY_MCP_COMMAND", "/usr/local/bin/relay")
	t.Setenv("RELAY_MCP_TOKEN", "")

	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	p := &ClaudeProvider{session: &Session{Settings: settings}}
	if got := p.relayMCPConfigJSON(); got != "" {
		t.Errorf("no-token config = %q, want empty", got)
	}
}

// TestClaudeRelayMCPConfig_Enabled exercises the happy path: opt-in
// session settings + project mcpToken → an inline --mcp-config that
// spawns `relay mcp` with RELAY_TOKEN populated. The structure here
// is what Claude CLI parses on every session start, so a shape change
// would break image-gen + every other relay MCP for Claude users.
func TestClaudeRelayMCPConfig_Enabled(t *testing.T) {
	t.Setenv("RELAY_MCP_COMMAND", "/usr/local/bin/relay")

	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	p := &ClaudeProvider{session: &Session{
		Settings: settings,
		McpToken: "proj-token-abc",
	}}
	raw := p.relayMCPConfigJSON()
	if raw == "" {
		t.Fatal("config empty despite useRelayTools + mcpToken")
	}

	var parsed struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	server, ok := parsed.MCPServers["relay"]
	if !ok {
		t.Fatalf("missing relay entry; got %v", parsed.MCPServers)
	}
	if server.Command != "/usr/local/bin/relay" {
		t.Errorf("command = %q", server.Command)
	}
	if len(server.Args) != 1 || server.Args[0] != "mcp" {
		t.Errorf("args = %v, want [mcp]", server.Args)
	}
	if server.Env["RELAY_TOKEN"] != "proj-token-abc" {
		t.Errorf("RELAY_TOKEN = %q, want proj-token-abc", server.Env["RELAY_TOKEN"])
	}
}
