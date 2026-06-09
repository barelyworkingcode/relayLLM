package main

// Coverage for the relay-facing bridge handshake. Validates:
//   - buildManifest() returns the expected route table (golden).
//   - maybeRegisterManifest is a no-op when RELAY_BRIDGE_SOCKET is unset.
//   - With env wired and a FakeBridge running, it sends a well-formed
//     RegisterManifest request carrying the right service id + socket + token.

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// FakeBridge — minimal Unix-socket implementation of relay's wire protocol
// ---------------------------------------------------------------------------

// FakeBridge accepts newline-delimited JSON requests on a Unix socket,
// records them, and replies with scripted responses. One connection per
// request matches relayLLM's sendBridgeRequest behavior.
type FakeBridge struct {
	socketPath string
	listener   net.Listener

	mu        sync.Mutex
	requests  []relayBridgeRequest
	respondWith relayBridgeResponse
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
		respondWith: relayBridgeResponse{Type: respOK},
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
func (b *FakeBridge) SetResponse(resp relayBridgeResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.respondWith = resp
}

// Requests returns a snapshot of every request the bridge has received.
func (b *FakeBridge) Requests() []relayBridgeRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]relayBridgeRequest, len(b.requests))
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
	var req relayBridgeRequest
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

// withBridgeEnv sets the three env vars relayLLM looks for and restores
// them on cleanup. Test isolation: never leak env changes to other tests.
func withBridgeEnv(t *testing.T, sockPath, serviceID, token string) {
	t.Helper()
	keys := []string{envBridgeSocket, envServiceID, envServiceToken, envServiceTokenLegacy}
	prev := map[string]string{}
	for _, k := range keys {
		prev[k] = os.Getenv(k)
	}
	_ = os.Setenv(envBridgeSocket, sockPath)
	_ = os.Setenv(envServiceID, serviceID)
	_ = os.Setenv(envServiceToken, token)
	_ = os.Setenv(envServiceTokenLegacy, "") // deterministic: token rides the new name only
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

// ---------------------------------------------------------------------------
// buildManifest golden
// ---------------------------------------------------------------------------

func TestManifest_BuildManifest_HasExpectedRoutes(t *testing.T) {
	dataDir := t.TempDir()
	m := buildManifest(dataDir)

	// Routes the relay dispatcher must know about. Adding a route is a
	// protocol change — break this test deliberately when adding one.
	wantRoutes := []string{
		"/api/sessions",
		"/api/sessions/",
		"/api/models",
		"/api/terminals",
		"/api/terminals/",
		"/api/terminal/",
		"/api/permission",
		"/api/generated/",
		"/api/status",
		"/api/llama/",
		"/ws",
	}
	if len(m.Routes) != len(wantRoutes) {
		t.Fatalf("route count: got %d, want %d (%v)", len(m.Routes), len(wantRoutes), m.Routes)
	}
	for _, want := range wantRoutes {
		found := false
		for _, got := range m.Routes {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing route %q in manifest", want)
		}
	}
	if m.Status == nil || m.Status.Path != "/api/status" {
		t.Errorf("status: got %+v, want path=/api/status", m.Status)
	}

	// Config rides on registration so relay can render the settings.json editor.
	if m.Config == nil {
		t.Fatalf("manifest has no Config — relay can't surface the settings editor")
	}
	if want := filepath.Join(dataDir, "settings.json"); m.Config.Path != want {
		t.Errorf("config.path: got %q, want %q", m.Config.Path, want)
	}
	if m.Config.ApplyMode != "restart" {
		t.Errorf("config.applyMode: got %q, want restart", m.Config.ApplyMode)
	}
	if len(m.Config.Schema) == 0 {
		t.Errorf("config.schema is empty — the editor would render nothing")
	}

	// Actions are part of the wire contract: the relay UI builds buttons
	// from this declaration. Adding/removing one is a coordinated change.
	wantActions := map[string]ActionDecl{
		"stop-llama": {
			ID: "stop-llama", Label: "Stop", Method: "DELETE",
			PathTemplate: "/api/llama/instances/{alias}", ForEach: "instances",
		},
		"stop-terminal": {
			ID: "stop-terminal", Label: "Kill", Method: "DELETE",
			PathTemplate: "/api/terminals/{id}", ForEach: "terminals",
		},
	}
	if len(m.Actions) != len(wantActions) {
		t.Fatalf("Actions count: got %d (%+v), want %d", len(m.Actions), m.Actions, len(wantActions))
	}
	for _, got := range m.Actions {
		want, ok := wantActions[got.ID]
		if !ok {
			t.Errorf("unexpected action %q", got.ID)
			continue
		}
		if got != want {
			t.Errorf("action %q: got %+v, want %+v", got.ID, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// maybeRegisterManifest
// ---------------------------------------------------------------------------

func TestManifest_MaybeRegister_StandaloneMode_IsNoOp(t *testing.T) {
	bridge := NewFakeBridge(t)
	// Deliberately do NOT set envBridgeSocket. Service ID/token presence
	// shouldn't matter — standalone mode short-circuits before reading them.
	_ = os.Unsetenv(envBridgeSocket)

	maybeRegisterManifest(t.TempDir(), "/tmp/some.sock", "tok123")

	if len(bridge.Requests()) != 0 {
		t.Errorf("standalone mode sent %d requests; want 0", len(bridge.Requests()))
	}
}

func TestManifest_MaybeRegister_SendsCorrectPayload(t *testing.T) {
	bridge := NewFakeBridge(t)
	withBridgeEnv(t, bridge.SocketPath(), "relayllm-test", "service-token-xyz")

	maybeRegisterManifest(t.TempDir(), "/tmp/internal.sock", "internal-token-abc")

	// Bridge accepts and replies are async via goroutine; wait briefly.
	waitFor(t, 1*time.Second, func() bool { return len(bridge.Requests()) >= 1 })

	reqs := bridge.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	req := reqs[0]
	if req.Type != reqRegisterManifest {
		t.Errorf("Type: got %q, want %q", req.Type, reqRegisterManifest)
	}
	if req.Token != "service-token-xyz" {
		t.Errorf("Token: got %q, want %q", req.Token, "service-token-xyz")
	}
	var args registerManifestRequest
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		t.Fatalf("decode Arguments: %v", err)
	}
	if args.ServiceID != "relayllm-test" {
		t.Errorf("ServiceID: got %q, want %q", args.ServiceID, "relayllm-test")
	}
	if args.InternalSocket != "/tmp/internal.sock" {
		t.Errorf("InternalSocket: got %q", args.InternalSocket)
	}
	if args.InternalToken != "internal-token-abc" {
		t.Errorf("InternalToken: got %q", args.InternalToken)
	}
	if len(args.Manifest.Routes) == 0 {
		t.Errorf("Manifest.Routes empty: %+v", args.Manifest)
	}
	// Actions ride along on registration — relay needs them to render
	// buttons and to validate dispatch requests against the whitelist.
	if len(args.Manifest.Actions) == 0 {
		t.Errorf("Manifest.Actions empty — relay can't render action buttons: %+v", args.Manifest)
	}
}

func TestManifest_MaybeRegister_MissingServiceID_IsNoOp(t *testing.T) {
	bridge := NewFakeBridge(t)
	// Set the socket but deliberately omit the service id.
	withBridgeEnv(t, bridge.SocketPath(), "", "tok")

	maybeRegisterManifest(t.TempDir(), "/tmp/x.sock", "internal")

	// maybeRegisterManifest is synchronous; if it dialed at all the request
	// would already be on the bridge by the time we return here.
	if len(bridge.Requests()) != 0 {
		t.Errorf("missing serviceID should skip registration; got %d requests", len(bridge.Requests()))
	}
}

func TestManifest_MaybeRegister_BridgeErrorIsSwallowed(t *testing.T) {
	bridge := NewFakeBridge(t)
	bridge.SetResponse(relayBridgeResponse{
		Type:    respError,
		Code:    500,
		Message: "internal error",
	})
	withBridgeEnv(t, bridge.SocketPath(), "relayllm-test", "tok")

	// The contract: registration failure logs + continues. The test asserts
	// "doesn't panic" — if maybeRegisterManifest ever started propagating
	// errors the call site (a goroutine in main) would silently exit and
	// future debugging would be confused.
	maybeRegisterManifest(t.TempDir(), "/tmp/x.sock", "internal")

	waitFor(t, 1*time.Second, func() bool { return len(bridge.Requests()) >= 1 })
	// One request was sent and bridge replied Error — relayLLM should not
	// have retried.
	if len(bridge.Requests()) != 1 {
		t.Errorf("expected exactly 1 request on bridge error; got %d", len(bridge.Requests()))
	}
}
