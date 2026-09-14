package testutil

// FakeBridge — minimal Unix-socket implementation of relay's bridge wire
// protocol, for tests of anything that dials relay's bridge socket
// (launch Hello, PTY env resolution, manifest registration, host-project
// template resolution).

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"relayllm/internal/relay"
	"relayllm/internal/types"
)

// FakeBridge accepts newline-delimited JSON requests on a Unix socket,
// records them, and replies with scripted responses. One connection per
// request matches relayLLM's relay.SendBridgeRequest behavior.
//
// Hello requests are recorded separately (Hellos) and answered with an OK
// naming the requested service unless SetHelloResponse overrides it. Any
// other request carrying a non-empty token is answered with an unauthorized
// Error, as relay does for a token that is not a project token.
type FakeBridge struct {
	socketPath string
	listener   net.Listener

	mu            sync.Mutex
	requests      []relay.BridgeRequest
	hellos        []relay.BridgeRequest
	respondWith   relay.BridgeResponse
	helloResponse *relay.BridgeResponse
}

const unauthorizedCode = -32001

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

// SetResponse replaces the scripted reply to non-Hello requests.
func (b *FakeBridge) SetResponse(resp relay.BridgeResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.respondWith = resp
}

// SetHelloResponse replaces the reply to Hello requests.
func (b *FakeBridge) SetHelloResponse(resp relay.BridgeResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.helloResponse = &resp
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

// Requests returns a snapshot of every non-Hello request the bridge received.
func (b *FakeBridge) Requests() []relay.BridgeRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]relay.BridgeRequest, len(b.requests))
	copy(out, b.requests)
	return out
}

// Hellos returns a snapshot of every Hello request the bridge received.
func (b *FakeBridge) Hellos() []relay.BridgeRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]relay.BridgeRequest, len(b.hellos))
	copy(out, b.hellos)
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
	var resp relay.BridgeResponse
	switch {
	case req.Type == relay.ReqHello:
		b.hellos = append(b.hellos, req)
		if b.helloResponse != nil {
			resp = *b.helloResponse
		} else {
			data, _ := json.Marshal(relay.HelloResult{ServiceID: req.Name, RelayPID: os.Getpid()})
			resp = relay.BridgeResponse{Type: relay.RespOK, Data: data}
		}
	case req.Token != "":
		b.requests = append(b.requests, req)
		resp = relay.BridgeResponse{Type: relay.RespError, Code: unauthorizedCode, Message: "unauthorized"}
	default:
		b.requests = append(b.requests, req)
		resp = b.respondWith
	}
	b.mu.Unlock()

	out, _ := json.Marshal(resp)
	_, _ = conn.Write(append(out, '\n'))
}

// WithBridgeEnv sets the non-secret env vars relay injects (bridge socket and
// service id) without performing a launch, so relay.Launched() stays false.
// Restores them on cleanup.
func WithBridgeEnv(t *testing.T, sockPath, serviceID string) {
	t.Helper()
	t.Setenv(relay.EnvBridgeSocket, sockPath)
	t.Setenv(relay.EnvServiceID, serviceID)
	relay.ResetLaunchForTesting()
	t.Cleanup(relay.ResetLaunchForTesting)
}

// LaunchViaBridge simulates relay launching this process: it sets the bridge
// env, writes a fresh launch secret into a real pipe, and completes the Hello
// handshake against b. Afterwards relay.Launched() is true until cleanup.
func LaunchViaBridge(t *testing.T, b *FakeBridge, serviceID string) {
	t.Helper()
	WithBridgeEnv(t, b.SocketPath(), serviceID)
	r := LaunchPipe(t, strings.Repeat("0123456789abcdef", 4))
	if _, err := relay.CompleteLaunch(r); err != nil {
		t.Fatalf("launch handshake: %v", err)
	}
}

// LaunchPipe returns the read end of a pipe whose write end has received
// content and been closed — the shape relay hands a service on fd 3.
func LaunchPipe(t *testing.T, content string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatalf("write launch pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close launch pipe writer: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}
