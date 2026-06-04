package main

import (
	"encoding/json"
	"testing"
)

func TestClaudeRelayMCPConfig_Disabled(t *testing.T) {
	p := &ClaudeProvider{session: &Session{}}
	if got := p.relayMCPConfigJSON(""); got != "" {
		t.Errorf("disabled config = %q, want empty", got)
	}
}

// Refuse to inject the relay MCP without a token — better a loud log
// than Claude silently auth-failing every tool call.
func TestClaudeRelayMCPConfig_NoToken(t *testing.T) {
	t.Setenv("RELAY_MCP_COMMAND", "/usr/local/bin/relay")
	t.Setenv("RELAY_MCP_TOKEN", "")

	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	p := &ClaudeProvider{session: &Session{Settings: settings}}
	if got := p.relayMCPConfigJSON(""); got != "" {
		t.Errorf("no-token config = %q, want empty", got)
	}
}

func TestClaudeRelayMCPConfig_Enabled(t *testing.T) {
	t.Setenv("RELAY_MCP_COMMAND", "/usr/local/bin/relay")

	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	p := &ClaudeProvider{session: &Session{
		Settings: settings,
		McpToken: "proj-token-abc",
	}}
	raw := p.relayMCPConfigJSON(p.session.McpToken)
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

// The in-memory McpToken wins and the bridge is never consulted — the common
// path for a freshly created session.
func TestClaudeResolveMCPToken_PrefersSessionToken(t *testing.T) {
	fb := NewFakeBridge(t)
	data, _ := json.Marshal(RelayPtyEnvResponse{RelayToken: "bridge-token"})
	fb.SetResponse(relayBridgeResponse{Type: respPtyEnv, Data: data})
	withBridgeEnv(t, fb.SocketPath(), "relay-llm", "svc-token")

	p := &ClaudeProvider{session: &Session{ID: "s1", McpToken: "session-token"}, directory: "/proj"}
	if got := p.resolveMCPToken(); got != "session-token" {
		t.Errorf("token = %q, want session-token", got)
	}
	if n := len(fb.Requests()); n != 0 {
		t.Errorf("bridge consulted %d times, want 0 (session token should short-circuit)", n)
	}
}

// Regression: a session resumed after a relayLLM restart has no in-memory
// McpToken (it's json:"-", never persisted). resolveMCPToken must re-fetch the
// project token from relay's bridge so Bash-driven `relay mcp call` skills keep
// authenticating instead of failing with "RELAY_TOKEN not set".
func TestClaudeResolveMCPToken_FallsBackToBridge(t *testing.T) {
	fb := NewFakeBridge(t)
	data, _ := json.Marshal(RelayPtyEnvResponse{RelayToken: "resumed-token-xyz", WorkingDir: "/proj"})
	fb.SetResponse(relayBridgeResponse{Type: respPtyEnv, Data: data})
	withBridgeEnv(t, fb.SocketPath(), "relay-llm", "svc-token")

	p := &ClaudeProvider{session: &Session{ID: "s1"}, directory: "/proj"}
	if got := p.resolveMCPToken(); got != "resumed-token-xyz" {
		t.Errorf("token = %q, want resumed-token-xyz", got)
	}
	reqs := fb.Requests()
	if len(reqs) != 1 || reqs[0].Type != reqResolvePtyEnv {
		t.Fatalf("bridge requests = %+v, want one ResolvePtyEnv", reqs)
	}
}

// Standalone relayLLM (no service token in env) has no bridge to ask; return
// empty rather than dialing and warning.
func TestClaudeResolveMCPToken_StandaloneReturnsEmpty(t *testing.T) {
	t.Setenv(envMcpToken, "")
	p := &ClaudeProvider{session: &Session{ID: "s1"}, directory: "/proj"}
	if got := p.resolveMCPToken(); got != "" {
		t.Errorf("token = %q, want empty (standalone)", got)
	}
}
