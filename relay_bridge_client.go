package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Minimal client for relay's bridge Unix socket. Used to resolve a
// project's runtime env (token, working dir, skill path) at PTY spawn time.
// Authenticates with the service token relay injected via RELAY_MCP_TOKEN
// when it spawned this process. The wire format mirrors relay/bridge —
// newline-delimited JSON, one request, one response.

const (
	relayBridgeSocketName = "relay.sock"
	relayBridgeTimeout    = 5 * time.Second
)

// RelayPtyEnvRequest mirrors relay/bridge.PtyEnvRequest. Kept inline to
// avoid a cross-repo Go module dependency.
type RelayPtyEnvRequest struct {
	Project     string `json:"project,omitempty"`
	Directory   string `json:"directory,omitempty"`
	RegenSkills string `json:"regen_skills"`
	SkillPath   string `json:"skill_path"`
}

// RelayPtyEnvResponse mirrors relay/bridge.PtyEnvResponse.
type RelayPtyEnvResponse struct {
	RelayToken string `json:"relay_token"`
	WorkingDir string `json:"working_dir"`
	SkillPath  string `json:"skill_path"`
}

// relayBridgeRequest is the on-wire request envelope.
type relayBridgeRequest struct {
	Type      string          `json:"type"`
	Token     string          `json:"token,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// relayBridgeResponse is the on-wire response envelope.
type relayBridgeResponse struct {
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data,omitempty"`
	Code    int             `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

// relayBridgeSocketPath returns the path where relay's bridge listens.
// Mirrors relay/bridge.SocketPath() so a fresh relay install resolves to
// the same location.
func relayBridgeSocketPath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir, _ = os.UserHomeDir()
	}
	return filepath.Join(configDir, "relay", relayBridgeSocketName)
}

// resolveRelayPtyEnv calls relay's bridge ResolvePtyEnv. Returns an error
// if relay is not running, the project cannot be resolved, or the auth
// token is missing. Service-token authentication via RELAY_MCP_TOKEN.
func resolveRelayPtyEnv(req RelayPtyEnvRequest) (RelayPtyEnvResponse, error) {
	token := os.Getenv("RELAY_MCP_TOKEN")
	if token == "" {
		return RelayPtyEnvResponse{}, fmt.Errorf("RELAY_MCP_TOKEN not set in environment (relay-managed templates require a service token)")
	}

	args, err := json.Marshal(req)
	if err != nil {
		return RelayPtyEnvResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	envelope := relayBridgeRequest{
		Type:      "ResolvePtyEnv",
		Token:     token,
		Arguments: args,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return RelayPtyEnvResponse{}, fmt.Errorf("marshal envelope: %w", err)
	}

	sockPath := relayBridgeSocketPath()
	conn, err := net.DialTimeout("unix", sockPath, relayBridgeTimeout)
	if err != nil {
		return RelayPtyEnvResponse{}, fmt.Errorf("dial relay bridge at %s: %w (is Relay tray app running?)", sockPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(relayBridgeTimeout))

	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return RelayPtyEnvResponse{}, fmt.Errorf("write to relay bridge: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return RelayPtyEnvResponse{}, fmt.Errorf("read from relay bridge: %w", err)
		}
		return RelayPtyEnvResponse{}, fmt.Errorf("relay bridge closed connection without responding")
	}

	var resp relayBridgeResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return RelayPtyEnvResponse{}, fmt.Errorf("parse relay response: %w", err)
	}
	if resp.Type == "Error" {
		return RelayPtyEnvResponse{}, fmt.Errorf("relay bridge error (code %d): %s", resp.Code, resp.Message)
	}
	if resp.Type != "PtyEnv" {
		return RelayPtyEnvResponse{}, fmt.Errorf("unexpected relay response type: %s", resp.Type)
	}

	var out RelayPtyEnvResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return RelayPtyEnvResponse{}, fmt.Errorf("parse PtyEnv data: %w", err)
	}
	return out, nil
}
