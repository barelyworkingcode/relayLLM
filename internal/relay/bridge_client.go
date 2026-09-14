package relay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"relayllm/internal/config"
	"relayllm/internal/types"
)

// Minimal client for relay's bridge Unix socket. Used to resolve a
// project's runtime env (token, working dir, host) at spawn time and to
// register the manifest. Requests carry no token: relay authenticates them by
// the peer audit token it bound at Hello (launch.go). The wire format mirrors
// relay/bridge — newline-delimited JSON, one request, one response.

const (
	relayBridgeSocketName = "relay.sock"
	relayBridgeTimeout    = 5 * time.Second

	// Env vars relay injects into every spawned service. None is secret.
	EnvBridgeSocket = "RELAY_BRIDGE_SOCKET"
	EnvServiceID    = "RELAY_SERVICE_ID"

	// Credential names relay no longer sets. relayLLM never reads them; they
	// exist only so they can be scrubbed (see removedCredentialEnvKeys).
	EnvFrontendToken      = "RELAY_FRONTEND_TOKEN"
	EnvServiceToken       = "RELAY_SERVICE_TOKEN"
	EnvServiceTokenLegacy = "RELAY_MCP_TOKEN"

	// EnvProjectToken is the project-scoped token relayLLM injects into spawned
	// children (an LLM CLI, the `relay mcp` subprocess, a project-scoped
	// terminal). Mirrors relay/bridge.EnvProjectToken. EnvProjectTokenLegacy is
	// the pre-rename name; we dual-write it into children during the transition
	// so existing user skills/scripts that reference RELAY_TOKEN keep working,
	// and strip it from the inherited base env so a stale one can't leak.
	EnvProjectToken       = "RELAY_PROJECT_TOKEN"
	EnvProjectTokenLegacy = "RELAY_TOKEN"

	// Bridge request/response type values. Must stay in sync with
	// relay/bridge/types.go.
	ReqResolvePtyEnv          = "ResolvePtyEnv"
	ReqResolveProjectTemplate = "ResolveProjectTemplate"
	ReqRegisterManifest       = "RegisterManifest"
	RespError                 = "Error"
	RespPtyEnv                = "PtyEnv"
	RespProjectTemplate       = "ProjectTemplate"
	RespOK                    = "OK"
)

// RelayPtyEnvRequest mirrors relay/bridge.PtyEnvRequest. Kept inline to
// avoid a cross-repo Go module dependency. ProjectID is the authoritative
// resolution key; relay validates Directory is within the project's path.
// The call resolves a project-scoped token + working dir; skill generation is
// owned by relay and is not driven from here.
type RelayPtyEnvRequest struct {
	ProjectID string `json:"project_id,omitempty"`
	Project   string `json:"project,omitempty"`
	Directory string `json:"directory,omitempty"`
}

// RelayPtyEnvResponse mirrors relay/bridge.PtyEnvResponse.
type RelayPtyEnvResponse struct {
	RelayToken string          `json:"relay_token"`
	WorkingDir string          `json:"working_dir"`
	Host       *types.HostSpec `json:"host,omitempty"`
}

// RelayProjectTemplateRequest mirrors relay/bridge.ShellTemplateRequest. Kept
// inline to avoid a cross-repo Go module dependency. Resolves a project-scoped
// shell (terminal) launch template definition by (ProjectID, TemplateID).
type RelayProjectTemplateRequest struct {
	ProjectID  string `json:"project_id"`
	TemplateID string `json:"template_id"`
}

// RelayProjectTemplateResponse mirrors relay/bridge.ShellTemplateResponse. It
// carries only the template definition — never a token.
type RelayProjectTemplateResponse struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
	Icon        string            `json:"icon,omitempty"`
}

// BridgeRequest is the on-wire request envelope.
type BridgeRequest struct {
	Type      string          `json:"type"`
	Name      string          `json:"name,omitempty"`
	Token     string          `json:"token,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// BridgeResponse is the on-wire response envelope.
type BridgeResponse struct {
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
	if p := os.Getenv(EnvBridgeSocket); p != "" {
		return p
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir, _ = os.UserHomeDir()
	}
	return filepath.Join(configDir, "relay", relayBridgeSocketName)
}

// SendBridgeRequest sends one tokenless request to relay's bridge and returns
// the parsed envelope. Refuses unless Launched(): a standalone process has no
// bound identity, so relay would treat the call as unauthenticated.
func SendBridgeRequest(reqType string, args json.RawMessage) (BridgeResponse, error) {
	if !Launched() {
		return BridgeResponse{}, fmt.Errorf("relay bridge unavailable: not launched by relay")
	}
	return roundTrip(relayBridgeSocketPath(), BridgeRequest{Type: reqType, Arguments: args})
}

// roundTrip dials sockPath, writes req, and reads exactly one response. An
// Error frame is returned as an error alongside the envelope.
func roundTrip(sockPath string, req BridgeRequest) (BridgeResponse, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return BridgeResponse{}, fmt.Errorf("marshal envelope: %w", err)
	}

	conn, err := net.DialTimeout("unix", sockPath, relayBridgeTimeout)
	if err != nil {
		return BridgeResponse{}, fmt.Errorf("dial relay bridge at %s: %w (is Relay tray app running?)", sockPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(relayBridgeTimeout))

	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return BridgeResponse{}, fmt.Errorf("write to relay bridge: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return BridgeResponse{}, fmt.Errorf("read from relay bridge: %w", err)
		}
		return BridgeResponse{}, fmt.Errorf("relay bridge closed connection without responding")
	}

	var resp BridgeResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return BridgeResponse{}, fmt.Errorf("parse relay response: %w", err)
	}
	if resp.Type == RespError {
		return resp, fmt.Errorf("relay bridge error (code %d): %s", resp.Code, resp.Message)
	}
	return resp, nil
}

// ResolvePtyEnv calls relay's bridge ResolvePtyEnv. Returns an error
// if this process was not launched by relay, relay is not reachable, or the
// project cannot be resolved.
func ResolvePtyEnv(req RelayPtyEnvRequest) (RelayPtyEnvResponse, error) {
	args, err := json.Marshal(req)
	if err != nil {
		return RelayPtyEnvResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := SendBridgeRequest(ReqResolvePtyEnv, args)
	if err != nil {
		return RelayPtyEnvResponse{}, err
	}
	if resp.Type != RespPtyEnv {
		return RelayPtyEnvResponse{}, fmt.Errorf("unexpected relay response type: %s", resp.Type)
	}
	var out RelayPtyEnvResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return RelayPtyEnvResponse{}, fmt.Errorf("parse PtyEnv data: %w", err)
	}
	return out, nil
}

// ResolveProjectTemplate calls relay's bridge ResolveProjectTemplate to
// fetch a project-scoped shell template definition by (projectID, templateID),
// mapping the response into a config.TerminalTemplate so the existing launch path is
// unchanged. Returns an error if this process was not launched by relay, relay
// is not reachable, or the project/template cannot be resolved — the caller fails closed
// and never spawns a guessed command. The response carries no token; the
// project token is injected separately by the existing ResolvePtyEnv path.
func ResolveProjectTemplate(projectID, templateID string) (config.TerminalTemplate, error) {
	args, err := json.Marshal(RelayProjectTemplateRequest{ProjectID: projectID, TemplateID: templateID})
	if err != nil {
		return config.TerminalTemplate{}, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := SendBridgeRequest(ReqResolveProjectTemplate, args)
	if err != nil {
		return config.TerminalTemplate{}, err
	}
	if resp.Type != RespProjectTemplate {
		return config.TerminalTemplate{}, fmt.Errorf("unexpected relay response type: %s", resp.Type)
	}
	var out RelayProjectTemplateResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return config.TerminalTemplate{}, fmt.Errorf("parse ProjectTemplate data: %w", err)
	}
	return config.TerminalTemplate{
		ID:          out.ID,
		Name:        out.Name,
		Command:     out.Command,
		Args:        out.Args,
		Env:         out.Env,
		Description: out.Description,
		Icon:        out.Icon,
	}, nil
}
