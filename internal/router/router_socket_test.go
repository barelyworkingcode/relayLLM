package router

// Coverage for router.sock (C9, plan-broker-and-sessions.md §2): admission
// against a captured relay identity, the mux differences from the TCP
// router, the "target" field on anthropic-map catalog rows, and
// X-Relay-Model-Target on a proxied response.
//
// The admission tests use real Unix sockets and, where a genuinely
// different OS process is needed (a peer that is NOT the identity captured
// at Hello), a subprocess of this same test binary — a Unix socket peer's
// kernel audit token is scoped to a real process, so nothing inside one
// process can manufacture a second identity to dial from.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"relayllm/internal/config"
	regpkg "relayllm/internal/registry"
	"relayllm/internal/relay"
	"relayllm/internal/testutil"
)

// ---------------------------------------------------------------------------
// Admission (real Unix sockets)
// ---------------------------------------------------------------------------

// shortSocketPath returns a path under /tmp, not t.TempDir() (which buries
// paths under TestName/NNN/ and routinely overflows macOS's 104-char
// AF_UNIX path limit — the same reason testutil.FakeBridge uses /tmp
// directly).
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rsock")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "r.sock")
}

func dialRouterSocket(t *testing.T, sockPath, path string) (status int, body string) {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("http://unix" + path)
	if err != nil {
		t.Fatalf("dial router socket: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// TestHelperDialUnixSocket is not a real test: it is invoked as a subprocess
// (exec.Command(os.Args[0], ...)) by tests that need a connection from a
// genuinely different OS process. It skips immediately under a normal
// `go test` run, where the trigger env var is unset.
func TestHelperDialUnixSocket(t *testing.T) {
	sock := os.Getenv("RH_ROUTER_SOCK_TEST_SOCKET")
	outFile := os.Getenv("RH_ROUTER_SOCK_TEST_OUT")
	if sock == "" || outFile == "" {
		t.Skip("helper test; runs only as a subprocess with env vars set")
	}
	status, body := dialRouterSocket(t, sock, "/health")
	_ = os.WriteFile(outFile, []byte(strconv.Itoa(status)+"\n"+body), 0o600)
}

// dialFromSeparateProcess re-execs this test binary as a helper subprocess
// (see TestHelperDialUnixSocket) and reports the status/body it observed
// dialing sockPath. A brand-new process has both a different pid AND a
// different pidversion from anything captured earlier in this test binary —
// a stronger mismatch than a mere pidversion change (an actual pid-recycle
// event, per spike SP1's note, isn't reproducible in a short-lived hermetic
// test), but it exercises the identical mechanism: admission is an exact
// (pid, pidversion) tuple match, and this peer satisfies neither half.
func dialFromSeparateProcess(t *testing.T, sockPath string) (status int, body string) {
	t.Helper()
	outFile := filepath.Join(t.TempDir(), "out")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDialUnixSocket$")
	cmd.Env = append(os.Environ(),
		"RH_ROUTER_SOCK_TEST_SOCKET="+sockPath,
		"RH_ROUTER_SOCK_TEST_OUT="+outFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper subprocess failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read helper output (subprocess output: %s): %v", out, err)
	}
	parts := strings.SplitN(string(data), "\n", 2)
	status, convErr := strconv.Atoi(parts[0])
	if convErr != nil {
		t.Fatalf("helper output not a status code: %q", data)
	}
	if len(parts) > 1 {
		body = parts[1]
	}
	return status, body
}

// helloSecret is a valid 64-lowercase-hex launch secret, shared by every test
// in this file that needs one.
const helloSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// selfHello runs a real Hello handshake against an in-process FakeBridge —
// plan-broker-and-sessions.md's L-M1 row's own recipe: "bind the test
// process's own identity as relay through the fake bridge + a real socket
// pair". Because FakeBridge's listener runs in this same test process,
// spike SP1's finding (the connecting end's LOCAL_PEERTOKEN names the
// ACCEPTING side) means Hello captures exactly this process's own
// (pid, pidversion) into relay.RelayIdentity.
func selfHello(t *testing.T) {
	t.Helper()
	t.Cleanup(relay.ResetRelayIdentityForTesting)
	fb := testutil.NewFakeBridge(t)
	if _, err := relay.Hello(fb.SocketPath(), "relayllm", helloSecret); err != nil {
		t.Fatalf("Hello: %v", err)
	}
}

func TestRouterSocket_AdmitsTheIdentityCapturedAtHello(t *testing.T) {
	selfHello(t)

	r := NewRelayRouter(":0", nil, nil, nil)
	sockPath := shortSocketPath(t)
	if err := r.ListenSocket(sockPath, relay.RelayIdentity); err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	defer r.Close()

	// Dialing from THIS process: per spike SP1, both ends of a connected
	// Unix stream socket see the OTHER side's kernel identity, so
	// router.sock's accept-side check here sees this process's own
	// identity — exactly what Hello (above) captured.
	status, body := dialRouterSocket(t, sockPath, "/health")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
}

func TestRouterSocket_NonRelayPeerRefused(t *testing.T) {
	selfHello(t)

	r := NewRelayRouter(":0", nil, nil, nil)
	sockPath := shortSocketPath(t)
	if err := r.ListenSocket(sockPath, relay.RelayIdentity); err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	defer r.Close()

	// A different OS process is never the identity Hello captured (this
	// test process's own pid/pidversion) — the ordinary "who is this"
	// refusal, and also stands in for a restarted relay (see
	// dialFromSeparateProcess's doc comment).
	status, body := dialFromSeparateProcess(t, sockPath)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	var errBody struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &errBody); err != nil {
		t.Fatalf("body not the expected JSON shape: %s: %v", body, err)
	}
	if errBody.Error.Message != "unauthorized" || errBody.Error.Type != "authentication_error" {
		t.Errorf("error body = %+v, want {unauthorized authentication_error}", errBody)
	}
}

// TestRouterSocket_RestartedRelayRefused documents the "different pidversion"
// case C9/L-M1 calls out explicitly: dialFromSeparateProcess's peer never
// shares this process's pidversion (or pid), the same mismatch a restarted
// relay (same pid, new pidversion, on a pid that hasn't actually been
// recycled) would produce against the admitted (pid, pidversion) tuple.
func TestRouterSocket_RestartedRelayRefused(t *testing.T) {
	selfHello(t)
	capturedBefore, _ := relay.RelayIdentity()

	r := NewRelayRouter(":0", nil, nil, nil)
	sockPath := shortSocketPath(t)
	if err := r.ListenSocket(sockPath, relay.RelayIdentity); err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	defer r.Close()

	status, _ := dialFromSeparateProcess(t, sockPath)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a peer with a different (pid, pidversion) than %+v", status, capturedBefore)
	}
}

// TestRouterSocket_TCPStillServedAlongside pins P1's rule (C9: "--router-port
// while launched: allowed in P1"): router.sock and the TCP listener run on
// the same *RelayRouter side by side, and ListenSocket must not disturb the
// TCP path in any way — same mux-building code, same Listen/Serve as an
// entirely standalone (non-launched) router.
func TestRouterSocket_TCPStillServedAlongside(t *testing.T) {
	selfHello(t)

	r := NewRelayRouter("127.0.0.1:0", nil, nil, nil)
	if err := r.Listen([]string{"127.0.0.1:0"}); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	r.Serve()
	defer r.Close()

	sockPath := shortSocketPath(t)
	if err := r.ListenSocket(sockPath, relay.RelayIdentity); err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}

	// TCP: unauthenticated model dispatch works exactly as before — nothing
	// about opening router.sock changes what the TCP listener serves.
	tcpResp, err := http.Get("http://" + r.Addr() + "/health")
	if err != nil {
		t.Fatalf("TCP GET /health: %v", err)
	}
	defer tcpResp.Body.Close()
	if tcpResp.StatusCode != http.StatusOK {
		t.Errorf("TCP /health status = %d, want 200", tcpResp.StatusCode)
	}

	// Socket: admitted (this process is the identity captured at Hello).
	status, body := dialRouterSocket(t, sockPath, "/health")
	if status != http.StatusOK {
		t.Fatalf("socket /health status = %d, want 200; body=%s", status, body)
	}
}

func TestRouterSocket_NoCapturedIdentityRefusesEveryone(t *testing.T) {
	t.Cleanup(relay.ResetRelayIdentityForTesting)
	relay.ResetRelayIdentityForTesting() // belt and suspenders: no Hello ran in this test

	r := NewRelayRouter(":0", nil, nil, nil)
	sockPath := shortSocketPath(t)
	if err := r.ListenSocket(sockPath, relay.RelayIdentity); err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	defer r.Close()

	status, _ := dialRouterSocket(t, sockPath, "/health")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when no identity has ever been captured", status)
	}
}

// ---------------------------------------------------------------------------
// Mux differences from the TCP router (httptest against SocketHandler
// directly — these assert on routing decisions, not admission, so a real
// socket adds nothing)
// ---------------------------------------------------------------------------

func TestRouterSocket_APIPassthrough404s(t *testing.T) {
	r := NewRelayRouter(":0", nil, nil, nil)
	r.setAnthropic(&config.AnthropicRouterConfig{})
	srv := httptest.NewServer(r.SocketHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/claude_cli/bootstrap")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRouterSocket_ConfiguredPassthrough404s(t *testing.T) {
	r := NewRelayRouter(":0", nil, nil, nil)
	r.setPassthrough(map[string]config.PassthroughConfig{
		"chatgpt": {Upstream: "https://chatgpt.com/backend-api"},
	})

	// Sanity: the TCP mux DOES serve it (setPassthrough registered it there),
	// so the socket's 404 below is really the socket-specific refusal, not an
	// artifact of a broken test upstream.
	tcpSrv := httptest.NewServer(r.mux)
	defer tcpSrv.Close()
	tcpResp := postBytes(t, tcpSrv.URL+"/chatgpt/codex/responses", []byte(`{}`))
	if tcpResp.StatusCode == http.StatusNotFound {
		t.Fatalf("setup: TCP mux should have a live passthrough route to compare against")
	}

	sockSrv := httptest.NewServer(r.SocketHandler())
	defer sockSrv.Close()
	resp, err := http.Get(sockSrv.URL + "/chatgpt/codex/responses")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 on router.sock", resp.StatusCode)
	}
}

func TestRouterSocket_MessagesServesOnlyModelMapKeys(t *testing.T) {
	upstream := newFakeOpenAIUpstream(t, []string{"local-model"})
	cfg := &config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{{Name: "ep", BaseURL: upstream.URL + "/v1", APIKey: "k"}}}
	registry := regpkg.NewProxyRegistry(cfg)

	r := NewRelayRouter(":0", nil, registry, nil)
	r.setAnthropic(&config.AnthropicRouterConfig{
		ModelMap: map[string]string{"claude-mapped": "ep/local-model"},
	})
	srv := httptest.NewServer(r.SocketHandler())
	defer srv.Close()

	// Not in modelMap: 404, never the real Anthropic passthrough.
	resp := postBytes(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-3-5-sonnet-not-mapped","messages":[]}`))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, body)
	}
	var errBody struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errBody); err != nil || errBody.Error.Type != "not_found_error" {
		t.Errorf("error body = %s, want an Anthropic not_found_error", body)
	}
}

// ---------------------------------------------------------------------------
// "target" on anthropic-map /v1/models rows, and X-Relay-Model-Target
// ---------------------------------------------------------------------------

func TestRouterCatalog_AnthropicMapRow_HasTargetField(t *testing.T) {
	r := NewRelayRouter(":0", nil, nil, nil)
	r.setAnthropic(&config.AnthropicRouterConfig{
		ModelMap: map[string]string{"claude-mapped": "ep/local-model"},
	})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	var resp struct {
		Data []map[string]interface{} `json:"data"`
	}
	doRouterJSON(t, srv.URL+"/v1/models", "GET", nil, &resp)

	found := false
	for _, row := range resp.Data {
		if row["id"] == "claude-mapped" {
			found = true
			if row["owned_by"] != "anthropic-map" {
				t.Errorf("owned_by = %v, want anthropic-map", row["owned_by"])
			}
			if row["target"] != "ep/local-model" {
				t.Errorf("target = %v, want ep/local-model", row["target"])
			}
		}
	}
	if !found {
		t.Fatalf("anthropic-map row missing from catalog: %v", resp.Data)
	}
}

// TestRouter_XRelayModelTargetHeader_EndpointDispatch pins C9's response
// header for a direct (non-virtual, non-Anthropic-redirect) OpenAI endpoint
// dispatch — the shape relay's model broker reads on every proxied call.
func TestRouter_XRelayModelTargetHeader_EndpointDispatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "Qwen"}}})
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp","choices":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()

	cfg := &config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "k"}}}
	registry := regpkg.NewProxyRegistry(cfg)
	r := NewRelayRouter(":0", nil, registry, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"fakeep/Qwen","stream":false}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Relay-Model-Target"); got != "fakeep/Qwen" {
		t.Errorf("X-Relay-Model-Target = %q, want %q", got, "fakeep/Qwen")
	}
}
