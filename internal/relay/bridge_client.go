package relay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"relayllm/internal/peertoken"
)

// Minimal client for relay's bridge Unix socket. Used to register the
// manifest and to register router.sock as relay's model-broker upstream
// (C9). Requests carry no token: relay authenticates them by the peer audit
// token it bound at Hello (launch.go). The wire format mirrors relay/bridge
// — newline-delimited JSON, one request, one response.

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
	ReqRegisterManifest = "RegisterManifest"

	// ReqRegisterModelHost registers this process's router.sock as the model
	// endpoint's upstream (C9, ../relay/docs/model-endpoint.md). Tokenless,
	// like RegisterManifest: relay authenticates the caller by the launch
	// identity Hello bound and requires the model_host capability.
	ReqRegisterModelHost = "RegisterModelHost"

	RespError = "Error"
	RespOK    = "OK"
)

// RegisterModelHostRequest mirrors relay/bridge.RegisterModelHostRequest.
// Unlike every other mirrored request type in this file, the field names are
// snake_case, not relayLLM's own camelCase convention — relay decodes this
// payload straight into its own RegisterModelHostRequest struct, whose json
// tags are service_id/router_socket (review amendment: "Bridge request JSON
// uses snake_case field names ... matching RegisterManifestRequest").
type RegisterModelHostRequest struct {
	ServiceID    string `json:"service_id"`
	RouterSocket string `json:"router_socket"`
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

// dialBridge dials sockPath and returns the connection alongside the peer
// audit token relay's kernel reports for it, read once immediately after
// connect — a Unix stream socket's peer identity cannot change for the life
// of one connection, so there is no reason to read it more than once. Every
// bridge dial goes through this, even though only the Hello path
// (launch.go's Hello, via helloRoundTrip below) currently uses the returned
// token: that keeps exactly one dial implementation instead of two that
// could drift.
func dialBridge(sockPath string) (net.Conn, peertoken.Token, error) {
	conn, err := net.DialTimeout("unix", sockPath, relayBridgeTimeout)
	if err != nil {
		return nil, peertoken.Token{}, fmt.Errorf("dial relay bridge at %s: %w (is Relay tray app running?)", sockPath, err)
	}
	tok, tokErr := peertoken.FromConn(conn)
	if tokErr != nil {
		_ = conn.Close()
		return nil, peertoken.Token{}, fmt.Errorf("read relay's peer audit token: %w", tokErr)
	}
	return conn, tok, nil
}

// sendAndReceive writes req and reads exactly one newline-delimited response
// off conn. An Error frame is returned as an error alongside the envelope.
func sendAndReceive(conn net.Conn, req BridgeRequest) (BridgeResponse, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return BridgeResponse{}, fmt.Errorf("marshal envelope: %w", err)
	}
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

// roundTrip dials sockPath, writes req, and reads exactly one response.
func roundTrip(sockPath string, req BridgeRequest) (BridgeResponse, error) {
	conn, _, err := dialBridge(sockPath)
	if err != nil {
		return BridgeResponse{}, err
	}
	defer conn.Close()
	return sendAndReceive(conn, req)
}

// helloRoundTrip is roundTrip plus the one thing only Hello needs: relay's
// own peer audit token off the SAME connection Hello succeeds over (spike
// SP1, plan-broker-and-sessions.md §2 C9) — every later bridge dial reaches
// the same relay process, so this is the one dial in relayLLM's lifetime
// where capturing the token matters.
func helloRoundTrip(sockPath string, req BridgeRequest) (BridgeResponse, peertoken.Token, error) {
	conn, tok, err := dialBridge(sockPath)
	if err != nil {
		return BridgeResponse{}, peertoken.Token{}, err
	}
	defer conn.Close()
	resp, err := sendAndReceive(conn, req)
	return resp, tok, err
}

// RegisterModelHost registers routerSocketPath as the model endpoint's
// upstream (C9, ../relay/docs/model-endpoint.md): relay will dial it, verify
// the server peer's kernel audit token matches this launch, and proxy
// brokered model calls to it. routerSocketPath must be absolute — relay
// dials it straight from its own process, so a relative path would resolve
// against relay's working directory, never this one's.
func RegisterModelHost(routerSocketPath string) error {
	if !filepath.IsAbs(routerSocketPath) {
		return fmt.Errorf("register model host: router socket path %q is not absolute", routerSocketPath)
	}
	args, err := json.Marshal(RegisterModelHostRequest{
		ServiceID:    os.Getenv(EnvServiceID),
		RouterSocket: routerSocketPath,
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	_, err = SendBridgeRequest(ReqRegisterModelHost, args)
	return err
}

// RegisterModelHostOrExit calls RegisterModelHost and, on a refusal, calls
// exitFn(78) — C9's fail-closed rule: relayLLM's router.sock only means
// anything once relay has actually agreed to dial it, so a refused
// registration must never be treated as a soft degradation the way a failed
// RegisterManifest is (MaybeRegisterManifest logs and continues; this does
// not). exitFn is injected — production (app.go) passes os.Exit — so a test
// can observe the refusal path without ending the test process.
func RegisterModelHostOrExit(routerSocketPath string, exitFn func(int)) {
	if err := RegisterModelHost(routerSocketPath); err != nil {
		slog.Error("model host registration refused by relay; exiting", "error", err)
		exitFn(78)
	}
}
