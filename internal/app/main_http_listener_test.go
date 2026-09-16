package app

// Coverage for the optional TCP front on the main mux (--http-port /
// --http-bind / --http-tls-cert / --http-tls-key) added so the /status
// dashboard is reachable from a browser without hand-rolling a proxy.
//
// The invariant these tests exist to protect is "same routes, different
// auth": the TCP front serves the identical route table (mux) the Unix
// socket serves, but carries NO bearer-token requirement of its own —
// reachability is gated purely by which --http-bind addresses actually
// bound, matching --router-port's existing unauthenticated-by-bind-address
// posture. The socket keeps requiring the token (it carries relay's
// manifest bridging). See auth.go's bearerAuth doc comment and main.go's
// handler-chain comment for the full reasoning.
//
// Certificates come from tls_test_helpers_test.go — hermetic, loopback only.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"relayllm/internal/api"
	"relayllm/internal/testutil"
	"strings"
	"testing"
	"time"
)

const httpFrontToken = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// echoMux answers with the requested path, so a response body doubles as
// proof of which route ran.
func echoMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "served "+r.URL.Path)
	})
	return mux
}

func TestValidateHTTPListener(t *testing.T) {
	cases := []struct {
		name    string
		port    string
		cert    string
		key     string
		wantErr string // substring; "" means the config must be accepted
	}{
		{name: "disabled by default", port: ""},
		// A disabled listener must not be able to fail startup for any
		// reason, even on flags that would be invalid were it enabled.
		{name: "disabled ignores a broken tls pair", port: "", cert: "/tmp/cert.pem"},
		{name: "no tls configured", port: "8181"},
		{name: "cert without key", port: "8181", cert: "/tmp/cert.pem",
			wantErr: "--http-tls-key"},
		{name: "key without cert", port: "8181", key: "/tmp/key.pem",
			wantErr: "--http-tls-cert"},
		{name: "matched cert and key", port: "8181", cert: "/tmp/cert.pem", key: "/tmp/key.pem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHTTPListener(tc.port, tc.cert, tc.key)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateHTTPListener(%q,%q,%q) = %v; want nil",
						tc.port, tc.cert, tc.key, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateHTTPListener(%q,%q,%q) = nil; want error containing %q",
					tc.port, tc.cert, tc.key, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// An empty addr must bind nothing and start nothing — the default
// deployment (no --http-port) has to be byte-identical to the code before
// this feature existed.
func TestMainTCPListener_DisabledIsNoop(t *testing.T) {
	srv, err := startMainTCPListener(nil, "", "", echoMux())
	if err != nil {
		t.Fatalf("disabled listener returned an error: %v", err)
	}
	if srv != nil {
		t.Fatalf("disabled listener returned a server (%v); want nil", srv.Addr)
	}
}

// startTestFront brings up the TCP front on an ephemeral port and returns
// its base URL. Registers its own shutdown.
func startTestFront(t *testing.T, certFile, keyFile string, handler http.Handler) (*http.Server, string) {
	t.Helper()
	srv, err := startMainTCPListener([]string{"127.0.0.1:0"}, certFile, keyFile, handler)
	if err != nil {
		t.Fatalf("startMainTCPListener: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	scheme := "http"
	if certFile != "" {
		scheme = "https"
	}
	return srv, scheme + "://" + srv.Addr
}

// The TCP front must serve every request with no credentials at all — no
// Authorization header, no cookie, nothing. Reachability is the only gate,
// via --http-bind.
func TestMainTCPListener_ServesWithoutAnyCredentials(t *testing.T) {
	_, base := startTestFront(t, "", "", api.RecoverMiddleware(echoMux()))

	cases := []struct {
		name   string
		header string
	}{
		{"no credentials", ""},
		{"a bearer header present but never checked", "Bearer " + httpFrontToken},
		{"garbage credentials still work", "Basic garbage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, base+"/status", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET /status: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d; want 200 regardless of credentials", resp.StatusCode)
			}
		})
	}
}

// The load-bearing claim of this feature's new shape: the TCP front serves
// the identical route table (mux) the Unix socket serves, but the socket
// keeps requiring the bearer token while the TCP front never does.
func TestMainTCPListener_SameRoutesAsSocket_DifferentAuth(t *testing.T) {
	recovered := api.RecoverMiddleware(echoMux())

	// /tmp rather than t.TempDir(): a sockaddr_un path is capped near 104
	// bytes and the per-test temp path overflows it (same reason
	// manifest_test.go's testutil.FakeBridge does this).
	dir, err := os.MkdirTemp("/tmp", "rl")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "r.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	sockServer := &http.Server{Handler: api.BearerAuth(httpFrontToken, recovered)}
	go func() { _ = sockServer.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = sockServer.Shutdown(ctx)
	})

	sockClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}}
	_, base := startTestFront(t, "", "", recovered)

	get := func(client *http.Client, rawURL, token string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", rawURL, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(body))
	}

	// Socket without a token: still rejected.
	if code, _ := get(sockClient, "http://unix/status", ""); code != http.StatusUnauthorized {
		t.Fatalf("socket without a token: status %d, want 401", code)
	}

	// Socket with the token, and the TCP front with no token at all, must
	// answer the same route with the same body.
	sockCode, sockBody := get(sockClient, "http://unix/status", httpFrontToken)
	tcpCode, tcpBody := get(http.DefaultClient, base+"/status", "")
	if sockCode != http.StatusOK || tcpCode != http.StatusOK {
		t.Fatalf("socket (with token) = %d, tcp (no token) = %d; want both 200", sockCode, tcpCode)
	}
	if sockBody != tcpBody {
		t.Fatalf("socket body %q != tcp body %q; expected the same route table", sockBody, tcpBody)
	}
}

func TestMainTCPListener_TLS(t *testing.T) {
	ca := testutil.NewTestCA(t)
	leaf := ca.IssueLeaf(t, 20)
	certFile, keyFile := leaf.WriteFiles(t)

	_, base := startTestFront(t, certFile, keyFile, api.RecoverMiddleware(echoMux()))

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.PEM) {
		t.Fatal("append CA pem")
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}

	resp, err := client.Get(base + "/status")
	if err != nil {
		t.Fatalf("https GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("https status %d; want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Fatal("response was not served over TLS")
	}

	// Same address, plaintext: the TLS listener must not answer it as http.
	plainResp, err := http.Get("http://" + strings.TrimPrefix(base, "https://") + "/status")
	if err == nil {
		plainResp.Body.Close()
		if plainResp.StatusCode < 400 {
			t.Fatalf("plaintext request to the TLS listener succeeded with %d", plainResp.StatusCode)
		}
	}
}

// freeTCPAddr binds then immediately releases a loopback port, following the
// same bind-and-release idiom TestPreflightPortFree_SucceedsForFreePort uses
// (server_manager_test.go) to hand a real, currently-free address to a
// component that wants to do its own net.Listen. Duplicated from
// internal/router/router_tls_test.go — a test helper can't cross a package
// boundary.
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// TestMainTCPListener_MultipleBinds is the end-to-end proof that this
// feature works: two independent addresses, one shared *http.Server, both
// serving the same handler, and a single Shutdown tearing down both at
// once.
func TestMainTCPListener_MultipleBinds(t *testing.T) {
	// freeTCPAddr binds then releases a loopback port, giving two addresses
	// this test can dial by name before startMainTCPListener rebinds them for
	// real — srv.Addr alone only ever reports the first, so this is the only
	// way to address the second bind directly.
	addr1 := freeTCPAddr(t)
	addr2 := freeTCPAddr(t)

	srv, err := startMainTCPListener([]string{addr1, addr2}, "", "", api.RecoverMiddleware(echoMux()))
	if err != nil {
		t.Fatalf("startMainTCPListener: %v", err)
	}

	get := func(addr string) (int, error) {
		resp, err := http.Get("http://" + addr + "/status")
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}

	for _, addr := range []string{addr1, addr2} {
		status, err := get(addr)
		if err != nil {
			t.Fatalf("GET %s: %v", addr, err)
		}
		if status != http.StatusOK {
			t.Fatalf("GET %s: status %d; want 200", addr, status)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	for _, addr := range []string{addr1, addr2} {
		if _, err := get(addr); err == nil {
			t.Fatalf("expected %s to be closed after Shutdown", addr)
		}
	}
}
