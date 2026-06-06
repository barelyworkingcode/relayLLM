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
// than Claude silently auth-failing every tool call. Critically, we must NOT
// fall back to the full-access service token here.
func TestClaudeRelayMCPConfig_NoToken(t *testing.T) {
	t.Setenv("RELAY_MCP_COMMAND", "/usr/local/bin/relay")

	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	p := &ClaudeProvider{session: &Session{Settings: settings}}
	if got := p.relayMCPConfigJSON(""); got != "" {
		t.Errorf("no-token config = %q, want empty", got)
	}
}

func TestClaudeRelayMCPConfig_Enabled(t *testing.T) {
	t.Setenv("RELAY_MCP_COMMAND", "/usr/local/bin/relay")

	settings, _ := json.Marshal(map[string]any{"useRelayTools": true})
	p := &ClaudeProvider{session: &Session{Settings: settings}}
	raw := p.relayMCPConfigJSON("proj-token-abc")
	if raw == "" {
		t.Fatal("config empty despite useRelayTools + project token")
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
	// The child must carry the project-scoped token under the new env name.
	if server.Env[envProjectToken] != "proj-token-abc" {
		t.Errorf("%s = %q, want proj-token-abc", envProjectToken, server.Env[envProjectToken])
	}
	if _, leaked := server.Env["RELAY_MCP_TOKEN"]; leaked {
		t.Errorf("relay MCP child env leaked a service-token slot: %v", server.Env)
	}
}

// The token is always resolved just-in-time from relay's bridge by project id.
// Relay is the sole authority — there is no stored/eve-supplied token to prefer,
// which makes this restart- and rotation-safe.
func TestClaudeResolveMCPToken_ResolvesFromBridge(t *testing.T) {
	fb := NewFakeBridge(t)
	data, _ := json.Marshal(RelayPtyEnvResponse{RelayToken: "resolved-token-xyz", WorkingDir: "/proj"})
	fb.SetResponse(relayBridgeResponse{Type: respPtyEnv, Data: data})
	withBridgeEnv(t, fb.SocketPath(), "relay-llm", "svc-token")

	p := &ClaudeProvider{session: &Session{ID: "s1", ProjectID: "proj-1", Directory: "/proj"}, directory: "/proj"}
	if got := p.resolveMCPToken(); got != "resolved-token-xyz" {
		t.Errorf("token = %q, want resolved-token-xyz", got)
	}
	reqs := fb.Requests()
	if len(reqs) != 1 || reqs[0].Type != reqResolvePtyEnv {
		t.Fatalf("bridge requests = %+v, want one ResolvePtyEnv", reqs)
	}
	// The request must resolve by the authoritative project id, not just a dir.
	var got RelayPtyEnvRequest
	if err := json.Unmarshal(reqs[0].Arguments, &got); err != nil {
		t.Fatalf("decode request args: %v", err)
	}
	if got.ProjectID != "proj-1" {
		t.Errorf("request project_id = %q, want proj-1", got.ProjectID)
	}
}

// Standalone relayLLM (no service token in env) has no bridge to ask; return
// empty rather than dialing and warning — and never escalate to a service token.
func TestClaudeResolveMCPToken_StandaloneReturnsEmpty(t *testing.T) {
	t.Setenv(envServiceToken, "")
	t.Setenv(envServiceTokenLegacy, "")
	p := &ClaudeProvider{session: &Session{ID: "s1", ProjectID: "proj-1"}, directory: "/proj"}
	if got := p.resolveMCPToken(); got != "" {
		t.Errorf("token = %q, want empty (standalone)", got)
	}
}
