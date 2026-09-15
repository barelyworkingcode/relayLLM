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

	"relayllm/internal/peertoken"
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

// relayIdentityStore and haveRelayIdentity hold the (pid, pidversion) pair
// Hello captured off relay's own connection (see Hello below) — the one peer
// router.sock admits (C9). Two atomics rather than one atomic.Value holding
// a nil-able pointer: BootstrapLaunch runs before any other goroutine
// starts, but router.sock's admission check (a later connection, from
// relay's http.Server goroutines) reads this concurrently, and a bool that
// is only ever set true (never back to false in production — only
// ResetRelayIdentityForTesting does that) is simpler to reason about here
// than a typed nil.
var (
	relayIdentityStore atomic.Value // holds peertoken.Process
	haveRelayIdentity  atomic.Bool
)

func storeRelayIdentity(p peertoken.Process) {
	relayIdentityStore.Store(p)
	haveRelayIdentity.Store(true)
}

// RelayIdentity returns the identity Hello bound relay to, or (zero, false)
// before any successful Hello — which matches no real peer, so a caller that
// races BootstrapLaunch fails closed rather than admitting everyone.
func RelayIdentity() (peertoken.Process, bool) {
	if !haveRelayIdentity.Load() {
		return peertoken.Process{}, false
	}
	return relayIdentityStore.Load().(peertoken.Process), true
}

// ResetRelayIdentityForTesting clears the captured identity. Test-only,
// mirroring ResetLaunchForTesting.
func ResetRelayIdentityForTesting() { haveRelayIdentity.Store(false) }

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
//
// It also captures relay's own identity off that same connection (spike SP1,
// plan-broker-and-sessions.md §2 C9): on the connecting end of a Unix stream
// socket, LOCAL_PEERTOKEN names the ACCEPTING process — symmetric with the
// well-known accepting-side use for LOCAL_PEERCRED, and not obvious without
// the spike. Requiring that token's pid to agree with the OK reply's
// relay_pid, rather than trusting either alone, is what makes router.sock's
// later admission check (C9) mean something: a process that could forge the
// JSON reply still cannot forge the kernel's account of who actually
// answered the connection. Any failure here — the token unreadable at all,
// or a pid mismatch — fails Hello closed exactly like every other launch
// failure (BootstrapLaunch's caller, app.go, treats a Hello error as fatal).
func Hello(socketPath, serviceID, secret string) (HelloResult, error) {
	resp, peerTok, err := helloRoundTrip(socketPath, BridgeRequest{Type: ReqHello, Name: serviceID, Token: secret})
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
	if !peerTok.Valid() || peerTok.PID() != int32(res.RelayPID) {
		return HelloResult{}, fmt.Errorf("relay hello: peer audit token pid %d does not match relay_pid %d", peerTok.PID(), res.RelayPID)
	}
	storeRelayIdentity(peerTok.Process())
	return res, nil
}
