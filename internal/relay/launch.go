package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// Launch identity: relay hands this process a one-time secret on an inherited
// pipe (RELAY_LAUNCH_FD), relayLLM spends it in a single Hello, and relay binds
// the connection's peer audit token as this service's identity. Every later
// bridge request carries an empty token and is authenticated by that identity.
// Contract: ../spec-launch-identity.md.

const (
	EnvLaunchFD = "RELAY_LAUNCH_FD"

	ReqHello = "Hello"

	launchSecretLen = 64
)

// removedCredentialEnvKeys are relay credentials that no longer exist. relayLLM
// never reads them; they are scrubbed from its own environment and from every
// child's so a stale value from an older relay or a user's shell cannot leak.
var removedCredentialEnvKeys = []string{
	EnvServiceToken,
	EnvServiceTokenLegacy,
	EnvFrontendToken,
}

var launched atomic.Bool

// Launched reports whether relay launched this process and the Hello
// handshake succeeded. It is the only signal that relay bridge features are
// available; false means standalone.
func Launched() bool { return launched.Load() }

// ResetLaunchForTesting clears the launched state. It can only ever turn
// bridge features off; turning them on requires a successful Hello.
func ResetLaunchForTesting() { launched.Store(false) }

// HelloResult is the data of relay's OK response to Hello.
type HelloResult struct {
	ServiceID string `json:"service_id"`
	RelayPID  int    `json:"relay_pid"`
}

// BootstrapLaunch performs the launch handshake when RELAY_LAUNCH_FD is set.
// Unset means standalone: (false, nil) and the bridge is never contacted. Set
// with any failure returns an error the caller must treat as fatal — relay
// believes it launched this process, so degrading to standalone would leave
// the two disagreeing. No returned error contains the secret.
func BootstrapLaunch() (bool, HelloResult, error) {
	for _, k := range removedCredentialEnvKeys {
		_ = os.Unsetenv(k)
	}
	raw, ok := os.LookupEnv(EnvLaunchFD)
	if !ok {
		return false, HelloResult{}, nil
	}
	// Deliberate: unset before anything else so no child spawned through an
	// inherited environment (exec.Cmd with a nil Env) believes it has a pipe.
	_ = os.Unsetenv(EnvLaunchFD)

	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return false, HelloResult{}, fmt.Errorf("%s=%q is not a file descriptor >= 3", EnvLaunchFD, raw)
	}
	res, err := CompleteLaunch(os.NewFile(uintptr(fd), "relay-launch"))
	if err != nil {
		return false, HelloResult{}, err
	}
	return true, res, nil
}

// CompleteLaunch reads the launch secret from f (closing it), then sends Hello
// to RELAY_BRIDGE_SOCKET as RELAY_SERVICE_ID. On success Launched() is true.
func CompleteLaunch(f *os.File) (HelloResult, error) {
	secret, err := ReadLaunchSecret(f)
	if err != nil {
		return HelloResult{}, err
	}
	sock := os.Getenv(EnvBridgeSocket)
	serviceID := os.Getenv(EnvServiceID)
	if sock == "" || serviceID == "" {
		return HelloResult{}, fmt.Errorf("%s set but %s or %s is empty", EnvLaunchFD, EnvBridgeSocket, EnvServiceID)
	}
	res, err := Hello(sock, serviceID, secret)
	if err != nil {
		return HelloResult{}, err
	}
	launched.Store(true)
	return res, nil
}

// ReadLaunchSecret reads f to EOF, closes it, and returns its content if it is
// exactly 64 lowercase hex characters. f is closed on every path.
func ReadLaunchSecret(f *os.File) (string, error) {
	if f == nil {
		return "", errors.New("launch fd: invalid descriptor")
	}
	// Deliberate: the limit is one past the valid length so an oversized
	// payload is rejected without waiting on a writer that never closes.
	buf, readErr := io.ReadAll(io.LimitReader(f, launchSecretLen+1))
	closeErr := f.Close()
	if readErr != nil {
		return "", fmt.Errorf("launch fd: read: %w", readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("launch fd: close: %w", closeErr)
	}
	if len(buf) != launchSecretLen {
		return "", fmt.Errorf("launch secret: got %d bytes, want %d lowercase hex characters", len(buf), launchSecretLen)
	}
	for _, c := range buf {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", errors.New("launch secret: not lowercase hex")
		}
	}
	return string(buf), nil
}

// Hello presents the launch secret to relay's bridge on one connection and
// returns relay's acknowledgement. The response must name the same service.
func Hello(socketPath, serviceID, secret string) (HelloResult, error) {
	resp, err := roundTrip(socketPath, BridgeRequest{Type: ReqHello, Name: serviceID, Token: secret})
	if err != nil {
		// Deliberate: relay promises never to echo the secret, but the error
		// ends up in logs, so do not rely on that.
		return HelloResult{}, errors.New(strings.ReplaceAll("relay hello: "+err.Error(), secret, "[redacted]"))
	}
	if resp.Type != RespOK {
		return HelloResult{}, fmt.Errorf("relay hello: unexpected response type %q", resp.Type)
	}
	var res HelloResult
	if err := json.Unmarshal(resp.Data, &res); err != nil {
		return HelloResult{}, fmt.Errorf("relay hello: parse OK data: %w", err)
	}
	if res.ServiceID != serviceID {
		return HelloResult{}, fmt.Errorf("relay hello: bound service %q, want %q", res.ServiceID, serviceID)
	}
	if res.RelayPID <= 0 {
		return HelloResult{}, fmt.Errorf("relay hello: invalid relay_pid %d", res.RelayPID)
	}
	return res, nil
}
