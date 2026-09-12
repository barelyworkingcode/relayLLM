package testutil

// FakeBridge — minimal Unix-socket implementation of relay's bridge wire
// protocol, for tests of anything that dials relay's bridge socket
// (PTY env resolution, manifest registration, host-project template
// resolution).

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"relayllm/internal/relay"
	"relayllm/internal/types"
)

// FakeBridge accepts newline-delimited JSON requests on a Unix socket,
// records them, and replies with scripted responses. One connection per
// request matches relayLLM's relay.SendBridgeRequest behavior.
type FakeBridge struct {
	socketPath string
	listener   net.Listener

	mu          sync.Mutex
	requests    []relay.BridgeRequest
	respondWith relay.BridgeResponse
}

// NewFakeBridge listens on a Unix socket and returns the running instance.
// macOS has a 104-char limit on socket paths, so t.TempDir() (which buries
// the dir under TestName/NNN/) often overflows. We use /tmp directly and
// clean up the socket file ourselves.
//
// Default scripted response is {Type: "OK"} — override via SetResponse for
// negative-path tests. Cleanup via t.Cleanup.
func NewFakeBridge(t *testing.T) *FakeBridge {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fb")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	sockPath := filepath.Join(dir, "r.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	b := &FakeBridge{
		socketPath:  sockPath,
		listener:    ln,
		respondWith: relay.BridgeResponse{Type: relay.RespOK},
	}
	go b.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	})
	return b
}

// SocketPath is the path callers should set RELAY_BRIDGE_SOCKET to.
func (b *FakeBridge) SocketPath() string { return b.socketPath }

// SetResponse replaces the scripted reply. Useful for forcing an Error.
func (b *FakeBridge) SetResponse(resp relay.BridgeResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.respondWith = resp
}

// SetHostPtyEnv scripts a ResolvePtyEnv response carrying a host, letting a
// test simulate a project that lives on an SSH host instead of the console.
func (b *FakeBridge) SetHostPtyEnv(workingDir string, host *types.HostSpec) {
	data, err := json.Marshal(relay.RelayPtyEnvResponse{WorkingDir: workingDir, Host: host})
	if err != nil {
		panic(err) // test-only helper; a marshal failure here is a test bug
	}
	b.SetResponse(relay.BridgeResponse{Type: relay.RespPtyEnv, Data: data})
}

// Requests returns a snapshot of every request the bridge has received.
func (b *FakeBridge) Requests() []relay.BridgeRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]relay.BridgeRequest, len(b.requests))
	copy(out, b.requests)
	return out
}

func (b *FakeBridge) serve() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go b.handleConn(conn)
	}
}

func (b *FakeBridge) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	if !scanner.Scan() {
		return
	}
	var req relay.BridgeRequest
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		return
	}
	b.mu.Lock()
	b.requests = append(b.requests, req)
	resp := b.respondWith
	b.mu.Unlock()

	out, _ := json.Marshal(resp)
	_, _ = conn.Write(append(out, '\n'))
}

// WithBridgeEnv sets the three env vars relayLLM looks for and restores them
// on cleanup. Test isolation: never leak env changes to other tests.
func WithBridgeEnv(t *testing.T, sockPath, serviceID, token string) {
	t.Helper()
	keys := []string{relay.EnvBridgeSocket, relay.EnvServiceID, relay.EnvServiceToken, relay.EnvServiceTokenLegacy}
	prev := map[string]string{}
	for _, k := range keys {
		prev[k] = os.Getenv(k)
	}
	_ = os.Setenv(relay.EnvBridgeSocket, sockPath)
	_ = os.Setenv(relay.EnvServiceID, serviceID)
	_ = os.Setenv(relay.EnvServiceToken, token)
	_ = os.Setenv(relay.EnvServiceTokenLegacy, "") // deterministic: token rides the new name only
	t.Cleanup(func() {
		for k, v := range prev {
			if v == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, v)
			}
		}
	})
}
