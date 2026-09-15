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
	"bytes"
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
	"sync/atomic"
	"testing"
	"time"

	"relayllm/internal/config"
	"relayllm/internal/peertoken"
	regpkg "relayllm/internal/registry"
	"relayllm/internal/relay"
	"relayllm/internal/servermanager"
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

// TestRouterSocket_SamePidDifferentPidversionRefused pins the half of C9's
// admission check dialFromSeparateProcess's tests cannot: every test above
// exercises a peer with BOTH a different pid and a different pidversion (an
// actual different process), which a mutant comparing pid alone would also
// correctly refuse — so none of them would catch that mutation. This test
// dials from THIS process (real pid, real pidversion) but gives ListenSocket
// a `want` that reports the same pid with pidversion+1, isolating the
// pidversion half of the (pid, pidversion) tuple compare
// (peertoken.Process equality in router_socket.go's ConnContext).
func TestRouterSocket_SamePidDifferentPidversionRefused(t *testing.T) {
	selfHello(t)
	real, ok := relay.RelayIdentity()
	if !ok {
		t.Fatal("setup: selfHello must have captured an identity")
	}

	wantIdentity := peertoken.Process{PID: real.PID, PIDVersion: real.PIDVersion + 1}
	r := NewRelayRouter(":0", nil, nil, nil)
	sockPath := shortSocketPath(t)
	if err := r.ListenSocket(sockPath, func() (peertoken.Process, bool) { return wantIdentity, true }); err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	defer r.Close()

	// Dialing from THIS process presents the REAL (pid, pidversion) —
	// same pid as wantIdentity, different pidversion. A pid-only compare
	// would wrongly admit this.
	status, body := dialRouterSocket(t, sockPath, "/health")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a same-pid/different-pidversion peer (real=%+v, admitted=%+v); body=%s", status, real, wantIdentity, body)
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

// TestRouterSocket_ListenSocketRefusesRegularFileAtPath pins
// removeStaleSocket's refusal: a regular file at --router-socket's path is a
// configuration mistake (or worse, something else's file), never a stale
// socket a prior crashed relayLLM could have left — ListenSocket must error
// out and leave it untouched, not silently unlink and recreate it.
func TestRouterSocket_ListenSocketRefusesRegularFileAtPath(t *testing.T) {
	sockPath := shortSocketPath(t)
	const content = "not a socket"
	if err := os.WriteFile(sockPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	r := NewRelayRouter(":0", nil, nil, nil)
	if err := r.ListenSocket(sockPath, relay.RelayIdentity); err == nil {
		t.Fatal("ListenSocket must refuse a path where a regular file already exists")
	}

	got, err := os.ReadFile(sockPath)
	if err != nil {
		t.Fatalf("the regular file was removed: %v", err)
	}
	if string(got) != content {
		t.Fatalf("the regular file's content changed: got %q, want %q", got, content)
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

// newFakeAnthropicUpstream returns an httptest server shaped like
// api.anthropic.com, plus a hit counter. Tests point router.anthropic.Upstream
// at it (instead of leaving the real "https://api.anthropic.com" default) so
// a bug that fell through to the real passthrough would be caught locally —
// asserting on the counter — rather than either silently passing (network
// unreachable in a sandboxed run) or, worse, actually reaching the internet
// from a hermetic test.
func newFakeAnthropicUpstream(t *testing.T) (srv *httptest.Server, hits *int32) {
	t.Helper()
	hits = new(int32)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestRouterSocket_MessagesServesOnlyModelMapKeys(t *testing.T) {
	// newFakeOpenAIUpstream only serves /v1/models; the mapped-model case
	// below needs a real /v1/chat/completions response too.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "local-model"}}})
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp","model":"local-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	cfg := &config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{{Name: "ep", BaseURL: upstream.URL + "/v1", APIKey: "k"}}}
	registry := regpkg.NewProxyRegistry(cfg)
	anthropicUpstream, hits := newFakeAnthropicUpstream(t)

	r := NewRelayRouter(":0", nil, registry, nil)
	r.setAnthropic(&config.AnthropicRouterConfig{
		Upstream: anthropicUpstream.URL,
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
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Errorf("fake Anthropic upstream hit %d time(s) for a non-mapped model; router.sock must never reach the real passthrough", got)
	}

	// Mapped: succeeds via the redirect path — and still never touches the
	// Anthropic upstream, only the modelMap's OpenAI-endpoint target.
	resp2 := postBytes(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-mapped","stream":false,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("mapped model: status = %d, want 200; body=%s", resp2.StatusCode, body2)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Errorf("fake Anthropic upstream hit %d time(s) for a MAPPED model; it must be served by the mapped target only", got)
	}
}

// TestRouterSocket_CountTokensNeverGenerates pins the should-fix: an earlier
// version of router_socket.go wired /v1/messages/count_tokens to the SAME
// handler as /v1/messages, so a mapped model ran a full upstream
// /v1/chat/completions call (and returned an Anthropic `message` body) where
// only a byte-based estimate was ever wanted.
func TestRouterSocket_CountTokensNeverGenerates(t *testing.T) {
	var chatCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "local-model"}}})
		case "/v1/chat/completions":
			atomic.AddInt32(&chatCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp","choices":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()

	cfg := &config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{{Name: "ep", BaseURL: upstream.URL + "/v1", APIKey: "k"}}}
	registry := regpkg.NewProxyRegistry(cfg)
	r := NewRelayRouter(":0", nil, registry, nil)
	r.setAnthropic(&config.AnthropicRouterConfig{
		ModelMap: map[string]string{"claude-mapped": "ep/local-model"},
	})
	srv := httptest.NewServer(r.SocketHandler())
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages/count_tokens", []byte(`{"model":"claude-mapped","messages":[{"role":"user","content":"hello there, how many tokens is this"}]}`))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body not the expected {input_tokens} shape: %s: %v", body, err)
	}
	if out.InputTokens <= 0 {
		t.Errorf("input_tokens = %d, want > 0", out.InputTokens)
	}
	if got := atomic.LoadInt32(&chatCalls); got != 0 {
		t.Errorf("count_tokens made %d upstream /v1/chat/completions call(s); it must only estimate, never generate", got)
	}

	// Unmapped model: 404, same shape as /v1/messages, still zero upstream
	// calls.
	resp2 := postBytes(t, srv.URL+"/v1/messages/count_tokens", []byte(`{"model":"not-mapped","messages":[]}`))
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unmapped count_tokens status = %d, want 404; body=%s", resp2.StatusCode, body2)
	}
	if got := atomic.LoadInt32(&chatCalls); got != 0 {
		t.Errorf("count_tokens made %d upstream call(s) for an unmapped model", got)
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

// TestRouterSocket_XRelayModelTargetHeader_ManagedAlias pins the header's
// managed-alias shape (a bare alias) over SocketHandler() specifically, not
// just the TCP mux — router.sock is relay's only path to this signal.
// InjectInstanceForTest fabricates a healthy instance bound to port 9000
// (the fixed port endpointForPort always reports for a fabricated instance);
// a real httptest server is bound there so Acquire's returned endpoint
// actually answers.
func TestRouterSocket_XRelayModelTargetHeader_ManagedAlias(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:9000")
	if err != nil {
		t.Skipf("cannot bind 127.0.0.1:9000 for this test: %v", err)
	}
	upstream := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","choices":[]}`))
	})}}
	upstream.Start()
	defer upstream.Close()

	mgr := servermanager.NewServerManager(servermanager.LlamaProfile, &config.ServerConfig{
		Models: []config.ServerModelConfig{{Alias: "test-alias"}},
	}, "")
	mgr.InjectInstanceForTest("test-alias", 0, time.Now())

	r := NewRelayRouter(":0", []*servermanager.ServerManager{mgr}, nil, nil)
	srv := httptest.NewServer(r.SocketHandler())
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"test-alias"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("X-Relay-Model-Target"), "test-alias"; got != want {
		t.Errorf("X-Relay-Model-Target = %q, want %q", got, want)
	}
}

// TestRouterSocket_XRelayModelTargetHeader_VirtualFailover pins that the
// header reflects the LAST attempted (successful) candidate, not the first —
// routeVirtual calls setModelTargetHeader before every attempt, so a
// regression that hoisted the call above the retry loop (setting it once,
// for the first candidate only) would leave this pointing at a target that
// never actually served the request.
func TestRouterSocket_XRelayModelTargetHeader_VirtualFailover(t *testing.T) {
	// Both endpoints must answer /v1/models (so the registry's reachability
	// probe puts BOTH in the "fresh"/declared-order set — CandidatesForVirtual
	// would otherwise already reorder a probe-time-unreachable endpoint to
	// the back on its own, which would make candidates[0] equal the
	// surviving target from the start and defeat the point of this test).
	// "primary" only fails at actual DISPATCH time (hijack+close, a
	// pre-response failure indistinguishable from a dial error), which is
	// what forces the retry this test is pinning the header through.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "primary-model"}}})
		case "/v1/chat/completions":
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
		}
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "secondary-model"}}})
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer secondary.Close()

	registry := regpkg.NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{
		{Name: "primary", BaseURL: primary.URL + "/v1"},
		{Name: "secondary", BaseURL: secondary.URL + "/v1"},
	}})
	r := NewRelayRouter(":0", nil, registry, &config.VirtualLLMConfig{Models: []config.VirtualLLM{{
		Name: "vRetry", Targets: []config.VirtualLLMTarget{
			{Endpoint: "primary", Model: "primary-model"},
			{Endpoint: "secondary", Model: "secondary-model"},
		},
	}}})
	srv := httptest.NewServer(r.SocketHandler())
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"vRetry"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("X-Relay-Model-Target"), "secondary/secondary-model"; got != want {
		t.Errorf("X-Relay-Model-Target = %q, want %q (the second, actually-successful candidate — declared FIRST is \"primary\", which failed at dispatch time)", got, want)
	}
}

// TestRouterSocket_XRelayModelTargetHeader_AnthropicRedirect pins the header
// through the /v1/messages modelMap redirect path specifically —
// translatingResponseWriter buffers headers set via its own Header() call
// separately from the real ResponseWriter (see its doc comment), so this is
// the one dispatch path where a plain w.Header().Set would silently vanish;
// only the modelTargetSetter/SetModelTarget interposition makes it survive.
func TestRouterSocket_XRelayModelTargetHeader_AnthropicRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "local-model"}}})
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp","model":"local-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()

	cfg := &config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{{Name: "ep", BaseURL: upstream.URL + "/v1", APIKey: "k"}}}
	registry := regpkg.NewProxyRegistry(cfg)
	r := NewRelayRouter(":0", nil, registry, nil)
	r.setAnthropic(&config.AnthropicRouterConfig{
		ModelMap: map[string]string{"claude-mapped": "ep/local-model"},
	})
	srv := httptest.NewServer(r.SocketHandler())
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-mapped","stream":false,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("X-Relay-Model-Target"), "ep/local-model"; got != want {
		t.Errorf("X-Relay-Model-Target = %q, want %q (body=%s)", got, want, body)
	}
}

// TestRouterSocket_ForwardsCleanRequest_StripsCredentialAndRelayHeaders is
// the "check what you forward" test the plan's review-driven amendment
// calls for, one hop further down than relay's own broker: relayLLM's own
// dispatch must not forward a caller's Authorization/X-Api-Key/X-Relay-*
// headers to the real backend regardless of who sent them (defense in
// depth — relay's broker already strips these before a call ever reaches
// router.sock, but relayLLM must not depend on that), and must not let an
// upstream's own spoofed X-Relay-Model-Target survive into the response
// relayLLM sends back.
func TestRouterSocket_ForwardsCleanRequest_StripsCredentialAndRelayHeaders(t *testing.T) {
	var seenAuth, seenAPIKey, seenModel string
	var seenXRelay []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "local-model"}}})
		case "/v1/chat/completions":
			seenAuth = r.Header.Get("Authorization")
			seenAPIKey = r.Header.Get("X-Api-Key")
			for k := range r.Header {
				if strings.HasPrefix(strings.ToLower(k), "x-relay-") {
					seenXRelay = append(seenXRelay, k)
				}
			}
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			seenModel = body.Model
			w.Header().Set("Content-Type", "application/json")
			// A malicious or merely-echoing upstream trying to smuggle its
			// own X-Relay-Model-Target into the response — must never
			// survive to the caller.
			w.Header().Set("X-Relay-Model-Target", "spoofed")
			_, _ = w.Write([]byte(`{"id":"resp","choices":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()

	cfg := &config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "upstream-key"}}}
	registry := regpkg.NewProxyRegistry(cfg)
	r := NewRelayRouter(":0", nil, registry, nil)
	srv := httptest.NewServer(r.SocketHandler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"fakeep/local-model"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer client-side-leaked-token")
	req.Header.Set("X-Api-Key", "client-side-leaked-key")
	req.Header.Set("X-Relay-Session", "should-never-reach-upstream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}

	if seenModel != "local-model" {
		t.Errorf("upstream saw model %q, want %q", seenModel, "local-model")
	}
	if seenAuth != "Bearer upstream-key" {
		t.Errorf("upstream saw Authorization %q, want the endpoint's OWN api key, not the caller's", seenAuth)
	}
	if seenAPIKey != "" {
		t.Errorf("upstream saw X-Api-Key %q; the caller's header must never be forwarded", seenAPIKey)
	}
	if len(seenXRelay) != 0 {
		t.Errorf("upstream saw x-relay-* headers %v; must never be forwarded", seenXRelay)
	}

	got := resp.Header.Values("X-Relay-Model-Target")
	if len(got) != 1 || got[0] != "fakeep/local-model" {
		t.Errorf("X-Relay-Model-Target values = %v, want exactly [%q] (the upstream's spoofed value must be stripped)", got, "fakeep/local-model")
	}
}
