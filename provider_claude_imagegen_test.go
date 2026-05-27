package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestClaudeImageGenMCPConfig_Disabled confirms the provider returns
// the empty string when ComfyUI isn't configured. An empty config means
// the Claude argv never gets --mcp-config, which keeps the Claude CLI
// MCP surface clean for users without image generation.
func TestClaudeImageGenMCPConfig_Disabled(t *testing.T) {
	p := &ClaudeProvider{}
	if got := p.imageGenMCPConfigJSON(); got != "" {
		t.Errorf("disabled config = %q, want empty", got)
	}
}

// TestClaudeImageGenMCPConfig_Enabled checks the JSON shape Claude CLI
// expects. The structure here is load-bearing: Claude CLI errors out at
// startup on malformed mcp-config, which the user sees as the model
// silently lacking the tool.
func TestClaudeImageGenMCPConfig_Enabled(t *testing.T) {
	p := &ClaudeProvider{
		imageGenComfyURL: "http://localhost:8188",
		imageGenDataDir:  "/var/relayllm",
	}
	got := p.imageGenMCPConfigJSON()
	if got == "" {
		t.Fatal("config empty even with comfyuiURL set")
	}

	var parsed struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("unmarshal config: %v\nbody: %s", err, got)
	}

	server, ok := parsed.MCPServers["relayllm-image-gen"]
	if !ok {
		t.Fatalf("server entry missing; got keys: %v", parsed.MCPServers)
	}
	if len(server.Args) != 1 || server.Args[0] != "mcp-image-gen" {
		t.Errorf("args = %v, want [mcp-image-gen]", server.Args)
	}
	if server.Env["COMFYUI_URL"] != "http://localhost:8188" {
		t.Errorf("COMFYUI_URL env = %q", server.Env["COMFYUI_URL"])
	}
	if server.Env["RELAY_LLM_DATA"] != "/var/relayllm" {
		t.Errorf("RELAY_LLM_DATA env = %q", server.Env["RELAY_LLM_DATA"])
	}
	// command should point to a real, runnable path. os.Executable can
	// produce different shapes in test contexts (the test binary path),
	// so just sanity-check it's non-empty and absolute.
	if server.Command == "" || !strings.HasPrefix(server.Command, "/") {
		t.Errorf("command not absolute: %q", server.Command)
	}
}
