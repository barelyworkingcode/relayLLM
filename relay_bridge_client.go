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
// Authenticates with the full-access service token relay injected via
// RELAY_SERVICE_TOKEN when it spawned this process. The wire format mirrors
// relay/bridge — newline-delimited JSON, one request, one response.

const (
	relayBridgeSocketName = "relay.sock"
	relayBridgeTimeout    = 5 * time.Second

	// Env vars relay injects into every spawned service. Mirrors relay's
	// own injection in service_registry.go. Constants live here (the bridge
	// client) so both consumers (PTY env resolution, manifest registration)
	// reference them through the same channel.
	envBridgeSocket = "RELAY_BRIDGE_SOCKET"
	envServiceID    = "RELAY_SERVICE_ID"

	// envFrontendToken is relay's front-door bearer. relay injects it into every
	// spawned service, but relayLLM never uses it (it's a backend, not a frontend
	// consumer) — it must be stripped from child env so it never leaks into a
	// shell. Mirrors relay/bridge.EnvFrontendToken.
	envFrontendToken = "RELAY_FRONTEND_TOKEN"

	// envServiceToken is the full-access service token used to authenticate
	// bridge calls (ResolvePtyEnv, RegisterManifest). Mirrors
	// relay/bridge.EnvServiceToken. It is NOT a project token and must never
	// be injected into a spawned child shell. envServiceTokenLegacy is the
	// pre-rename name, accepted as a transition fallback; drop once relay
	// stops setting it.
	envServiceToken       = "RELAY_SERVICE_TOKEN"
	envServiceTokenLegacy = "RELAY_MCP_TOKEN"

	// envProjectToken is the project-scoped token relayLLM injects into spawned
	// children (an LLM CLI, the `relay mcp` subprocess, a project-scoped
	// terminal). Mirrors relay/bridge.EnvProjectToken. envProjectTokenLegacy is
	// the pre-rename name; we dual-write it into children during the transition
	// so existing user skills/scripts that reference RELAY_TOKEN keep working,
	// and strip it from the inherited base env so a stale one can't leak.
	envProjectToken       = "RELAY_PROJECT_TOKEN"
	envProjectTokenLegacy = "RELAY_TOKEN"

	// Bridge request/response type values. Must stay in sync with
	// relay/bridge/types.go.
	reqResolvePtyEnv    = "ResolvePtyEnv"
	reqRegisterManifest = "RegisterManifest"
	respError           = "Error"
	respPtyEnv          = "PtyEnv"
	respOK              = "OK"
)

// RelayPtyEnvRequest mirrors relay/bridge.PtyEnvRequest. Kept inline to
// avoid a cross-repo Go module dependency. ProjectID is the authoritative
// resolution key; relay validates Directory is within the project's path.
type RelayPtyEnvRequest struct {
	ProjectID   string `json:"project_id,omitempty"`
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
// Prefers the RELAY_BRIDGE_SOCKET env var (set by relay at spawn) and
// falls back to the conventional location so direct invocations (tests,
// debug runs) still work without env setup.
func relayBridgeSocketPath() string {
	if p := os.Getenv(envBridgeSocket); p != "" {
		return p
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir, _ = os.UserHomeDir()
	}
	return filepath.Join(configDir, "relay", relayBridgeSocketName)
}

// serviceToken returns the full-access service token relay injected at spawn,
// preferring the current env name and falling back to the legacy name during
// the cross-repo rename window. Empty when this process was not spawned by
// relay (standalone/dev runs).
func serviceToken() string {
	if t := os.Getenv(envServiceToken); t != "" {
		return t
	}
	return os.Getenv(envServiceTokenLegacy)
}

// sendBridgeRequest dials relay's bridge socket, writes one request, reads
// one response, returns the parsed envelope. Authentication is read from
// the RELAY_SERVICE_TOKEN env (the service token relay issued at spawn),
// falling back to the legacy name during the cross-repo rename window.
//
// Shared by every bridge-consuming call site (PTY env resolution, manifest
// registration) so the dial / write / scan / parse machinery lives in
// exactly one place.
func sendBridgeRequest(reqType string, args json.RawMessage) (relayBridgeResponse, error) {
	token := serviceToken()
	if token == "" {
		return relayBridgeResponse{}, fmt.Errorf("%s not set in environment (relay-managed callers require a service token)", envServiceToken)
	}

	payload, err := json.Marshal(relayBridgeRequest{
		Type:      reqType,
		Token:     token,
		Arguments: args,
	})
	if err != nil {
		return relayBridgeResponse{}, fmt.Errorf("marshal envelope: %w", err)
	}

	sockPath := relayBridgeSocketPath()
	conn, err := net.DialTimeout("unix", sockPath, relayBridgeTimeout)
	if err != nil {
		return relayBridgeResponse{}, fmt.Errorf("dial relay bridge at %s: %w (is Relay tray app running?)", sockPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(relayBridgeTimeout))

	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return relayBridgeResponse{}, fmt.Errorf("write to relay bridge: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return relayBridgeResponse{}, fmt.Errorf("read from relay bridge: %w", err)
		}
		return relayBridgeResponse{}, fmt.Errorf("relay bridge closed connection without responding")
	}

	var resp relayBridgeResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return relayBridgeResponse{}, fmt.Errorf("parse relay response: %w", err)
	}
	if resp.Type == respError {
		return resp, fmt.Errorf("relay bridge error (code %d): %s", resp.Code, resp.Message)
	}
	return resp, nil
}

// resolveRelayPtyEnv calls relay's bridge ResolvePtyEnv. Returns an error
// if relay is not running, the project cannot be resolved, or the auth
// token is missing.
func resolveRelayPtyEnv(req RelayPtyEnvRequest) (RelayPtyEnvResponse, error) {
	args, err := json.Marshal(req)
	if err != nil {
		return RelayPtyEnvResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := sendBridgeRequest(reqResolvePtyEnv, args)
	if err != nil {
		return RelayPtyEnvResponse{}, err
	}
	if resp.Type != respPtyEnv {
		return RelayPtyEnvResponse{}, fmt.Errorf("unexpected relay response type: %s", resp.Type)
	}
	var out RelayPtyEnvResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return RelayPtyEnvResponse{}, fmt.Errorf("parse PtyEnv data: %w", err)
	}
	return out, nil
}
