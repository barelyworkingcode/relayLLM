package api

// Exercises TCPDiagnosticsOnly against a real, full route table (the same
// mux NewTestServer wires for every other test in this package) over real
// HTTP via httptest.Server — never a direct ServeHTTP call — so path
// parsing, method dispatch, and escaping all go through the same net/http
// machinery a real client would.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newDiagnosticsFronts returns the two fronts app.go actually builds from
// one mux: tcpSrv (TCPDiagnosticsOnly-wrapped, anonymous — mirrors
// --http-port) and the TestServer's own socketSrv (bearerAuth-wrapped —
// mirrors --socket). Both are backed by the identical handler value
// (srv.Recovered) app.go itself wraps two different ways, so a pass here
// pins the same composition production uses rather than a hand-rolled
// stand-in.
func newDiagnosticsFronts(t *testing.T) (tcpSrv *httptest.Server, socket *TestServer) {
	t.Helper()
	srv := NewTestServer(t, nil)
	tcp := httptest.NewServer(TCPDiagnosticsOnly(srv.Recovered))
	t.Cleanup(tcp.Close)
	return tcp, srv
}

func TestTCPDiagnostics_AllowlistedRoutesServe200(t *testing.T) {
	tcp, _ := newDiagnosticsFronts(t)

	for _, path := range []string{
		"/status",
		"/status/status.css",
		"/status/status.js",
		"/api/status",
		"/api/status/detailed",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(tcp.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("GET %s on TCP front: got %d, want 200; body=%q", path, resp.StatusCode, body)
			}
		})
	}
}

// notFoundBody is the exact, literal body http.NotFound writes — both
// TCPDiagnosticsOnly's own block and an unmatched mux pattern produce this
// byte-for-byte, by design (a probe must not be able to tell "blocked" from
// "never existed"). Tests use it the other way: to confirm a *socket-side*
// response came from a real handler rather than coincidentally matching the
// generic 404 shape.
const notFoundBody = "404 page not found\n"

func TestTCPDiagnostics_NonAllowlistedRoutesServe404OnTCP(t *testing.T) {
	tcp, _ := newDiagnosticsFronts(t)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"create terminal", http.MethodPost, "/api/terminals"},
		{"create session", http.MethodPost, "/api/sessions"},
		{"list sessions", http.MethodGet, "/api/sessions"},
		{"delete session", http.MethodDelete, "/api/sessions/abc"},
		{"stop session", http.MethodPost, "/api/sessions/abc/stop"},
		{"list terminals", http.MethodGet, "/api/terminals"},
		{"delete terminal", http.MethodDelete, "/api/terminals/abc"},
		{"terminal log", http.MethodGet, "/api/terminals/abc/log"},
		{"list models", http.MethodGet, "/api/models"},
		{"list llama instances", http.MethodGet, "/api/llama/instances"},
		{"delete llama instance", http.MethodDelete, "/api/llama/instances/abc"},
		{"list mlx instances", http.MethodGet, "/api/mlx/instances"},
		{"delete mlx instance", http.MethodDelete, "/api/mlx/instances/abc"},
		{"post permission", http.MethodPost, "/api/permission"},
		{"generated image", http.MethodGet, "/api/generated/x.png"},
		{"terminal templates", http.MethodGet, "/api/terminal/templates"},
		{"ws upgrade attempt", http.MethodGet, "/ws"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, tcp.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound || string(body) != notFoundBody {
				t.Fatalf("%s %s on TCP front: got %d %q, want 404 %q", tc.method, tc.path, resp.StatusCode, body, notFoundBody)
			}
		})
	}
}

// A real WebSocket handshake (not just a bare GET) must also 404 on TCP —
// TCPDiagnosticsOnly runs before the mux ever sees the Upgrade header, so
// it never reaches wsHub.HandleUpgrade regardless of how well-formed the
// handshake is.
func TestTCPDiagnostics_WebSocketUpgradeBlockedOnTCP(t *testing.T) {
	tcp, _ := newDiagnosticsFronts(t)

	req, err := http.NewRequest(http.MethodGet, tcp.URL+"/ws", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || string(body) != notFoundBody {
		t.Fatalf("ws handshake on TCP front: got %d %q, want 404 %q", resp.StatusCode, body, notFoundBody)
	}
}

// The socket front (bearerAuth, no TCPDiagnosticsOnly) must still reach the
// real handler for every route TCP now blocks — hardening the anonymous
// front must not regress the authenticated one. "Reaches the handler" is
// checked by requiring the response body differ from the literal 404 page
// TCPDiagnosticsOnly (and an unmatched mux pattern) would produce; several
// of these legitimately 404 at the application layer (unknown session id,
// unknown terminal id), which is a different, real answer from a real
// handler, not the generic block.
func TestTCPDiagnostics_SocketFrontStillServesFullTable(t *testing.T) {
	_, socket := newDiagnosticsFronts(t)

	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int // 0 means "don't care, just must not be the generic 404 body"
	}{
		{name: "create session", method: http.MethodPost, path: "/api/sessions", body: `{"providerType":"fake","model":"fake/fake","directory":"/tmp"}`, wantStatus: 201},
		{name: "list sessions", method: http.MethodGet, path: "/api/sessions", wantStatus: 200},
		{name: "delete session", method: http.MethodDelete, path: "/api/sessions/nonexistent", wantStatus: 200},
		{name: "list terminals", method: http.MethodGet, path: "/api/terminals", wantStatus: 200},
		{name: "create terminal bad template", method: http.MethodPost, path: "/api/terminals", body: `{"templateId":"does-not-exist"}`, wantStatus: 400},
		{name: "terminal log unknown id", method: http.MethodGet, path: "/api/terminals/nonexistent/log"},
		{name: "list llama instances", method: http.MethodGet, path: "/api/llama/instances", wantStatus: 200},
		{name: "list models", method: http.MethodGet, path: "/api/models", wantStatus: 200},
		{name: "terminal templates", method: http.MethodGet, path: "/api/terminal/templates", wantStatus: 200},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reqBody io.Reader
			if tc.body != "" {
				reqBody = strings.NewReader(tc.body)
			}
			req := socket.RawRequest(tc.method, tc.path, reqBody)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, err)
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)
			if string(respBody) == notFoundBody {
				t.Fatalf("%s %s on socket front: got the generic block/unmatched 404 body, want a real handler response", tc.method, tc.path)
			}
			if tc.wantStatus != 0 && resp.StatusCode != tc.wantStatus {
				t.Fatalf("%s %s on socket front: got %d %q, want %d", tc.method, tc.path, resp.StatusCode, respBody, tc.wantStatus)
			}
		})
	}
}

// Path-trick attempts must not slip through to an allowlisted route by
// being normalized first — isCleanDiagnosticsPath rejects anything not
// already in the exact canonical form the allowlist compares against.
func TestTCPDiagnostics_PathTricksRejected(t *testing.T) {
	tcp, _ := newDiagnosticsFronts(t)

	cases := []struct {
		name string
		path string // appended raw to tcp.URL; not further escaped
	}{
		{"double slash", "/api//status"},
		{"double slash in status", "//status"},
		{"dot-dot traversal via disallowed segment", "/api/terminals/../status"},
		{"dot-dot resolving to allowed detailed", "/api/status/../status/detailed"},
		{"encoded slash hides allowed path", "/api%2Fstatus"},
		{"encoded slash mid traversal", "/status%2F..%2Fapi%2Fterminals"},
		{"trailing slash on exact route", "/api/status/"},
		{"trailing slash on status root", "/status/"},
		{"dot segment", "/api/./status"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tcp.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("build request for %q: %v", tc.path, err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound || string(body) != notFoundBody {
				t.Fatalf("GET %s on TCP front: got %d %q, want 404 %q", tc.path, resp.StatusCode, body, notFoundBody)
			}
		})
	}
}
