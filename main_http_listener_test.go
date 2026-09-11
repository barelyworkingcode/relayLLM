package main

// Coverage for the optional TCP front on the main mux (--http-port /
// --http-bind / --http-tls-cert / --http-tls-key) added so the /status
// dashboard is reachable from a browser without hand-rolling a proxy.
//
// The invariant these tests exist to protect is "second front, same door":
// the TCP listener must serve the identical handler value the Unix socket
// serves, with the identical bearer auth. TestMainTCPListener_MatchesSocket
// asserts that by driving one handler through both transports and comparing
// the answers, rather than by asserting a route list that would drift.
//
// Certificates come from tls_test_helpers_test.go — hermetic, loopback only.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
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
		bind    string
		cert    string
		key     string
		wantErr string // substring; "" means the config must be accepted
	}{
		{name: "disabled by default", port: "", bind: "127.0.0.1"},
		// A disabled listener must not be able to fail startup for any
		// reason, even on flags that would be invalid were it enabled.
		{name: "disabled ignores a broken tls pair", port: "", bind: "0.0.0.0", cert: "/tmp/cert.pem"},
		{name: "loopback plaintext", port: "8181", bind: "127.0.0.1"},
		{name: "localhost by name is loopback", port: "8181", bind: "localhost"},
		{name: "ipv6 loopback", port: "8181", bind: "::1"},
		{name: "cert without key", port: "8181", bind: "127.0.0.1", cert: "/tmp/cert.pem",
			wantErr: "--http-tls-key"},
		{name: "key without cert", port: "8181", bind: "127.0.0.1", key: "/tmp/key.pem",
			wantErr: "--http-tls-cert"},
		{name: "wildcard bind without tls", port: "8181", bind: "0.0.0.0",
			wantErr: "plaintext"},
		{name: "lan bind without tls", port: "8181", bind: "192.168.64.1",
			wantErr: "plaintext"},
		{name: "wildcard bind with tls", port: "8181", bind: "0.0.0.0",
			cert: "/tmp/cert.pem", key: "/tmp/key.pem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHTTPListener(tc.port, tc.bind, tc.cert, tc.key)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateHTTPListener(%q,%q,%q,%q) = %v; want nil",
						tc.port, tc.bind, tc.cert, tc.key, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateHTTPListener(%q,%q,%q,%q) = nil; want error containing %q",
					tc.port, tc.bind, tc.cert, tc.key, tc.wantErr)
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
	srv, err := startMainTCPListener("", "", "", echoMux())
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
	srv, err := startMainTCPListener("127.0.0.1:0", certFile, keyFile, handler)
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

func TestMainTCPListener_RequiresTheSameBearerToken(t *testing.T) {
	handler := bearerAuth(httpFrontToken, echoMux())
	_, base := startTestFront(t, "", "", handler)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no credentials", "", http.StatusUnauthorized},
		{"wrong token", "Bearer " + strings.Repeat("0", len(httpFrontToken)), http.StatusUnauthorized},
		{"not a bearer scheme", "Basic " + httpFrontToken, http.StatusUnauthorized},
		{"correct token", "Bearer " + httpFrontToken, http.StatusOK},
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
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d; want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// The load-bearing claim of this feature: the TCP front is the same door as
// the Unix socket, not a parallel one. Same handler value, both transports,
// identical answers — including the 401 for an unauthenticated caller.
func TestMainTCPListener_MatchesSocket(t *testing.T) {
	handler := bearerAuth(httpFrontToken, echoMux())

	// /tmp rather than t.TempDir(): a sockaddr_un path is capped near 104
	// bytes and the per-test temp path overflows it (same reason
	// manifest_test.go's FakeBridge does this).
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
	sockServer := &http.Server{Handler: handler}
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
	_, base := startTestFront(t, "", "", handler)

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

	for _, path := range []string{"/status", "/api/status/detailed"} {
		for _, token := range []string{httpFrontToken, ""} {
			sockCode, sockBody := get(sockClient, "http://unix"+path, token)
			tcpCode, tcpBody := get(http.DefaultClient, base+path, token)
			if sockCode != tcpCode || sockBody != tcpBody {
				t.Fatalf("path %s (token present: %t): socket gave %d %q, tcp gave %d %q",
					path, token != "", sockCode, sockBody, tcpCode, tcpBody)
			}
		}
	}
}

// The browser path end to end over a real listener: a URL carrying ?token=
// hands back a cookie and a redirect to the same path without the secret,
// and the cookie alone authenticates every later request (which is what the
// dashboard's own fetch of /api/status/detailed relies on).
func TestMainTCPListener_TokenQueryParamBootstrapsCookie(t *testing.T) {
	handler := bearerAuth(httpFrontToken, echoMux())
	_, base := startTestFront(t, "", "", handler)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar}

	resp, err := client.Get(base + "/status?token=" + httpFrontToken)
	if err != nil {
		t.Fatalf("GET /status?token=: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// The default client follows the redirect, so a 200 here means the
	// cookie it was handed carried the *second* request.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap status %d; want 200", resp.StatusCode)
	}
	if got := strings.TrimSpace(string(body)); got != "served /status" {
		t.Fatalf("bootstrap body %q; want %q", got, "served /status")
	}
	if q := resp.Request.URL.RawQuery; q != "" {
		t.Fatalf("landed on %q; the token must be stripped from the final URL", resp.Request.URL)
	}

	u, _ := url.Parse(base)
	var found *http.Cookie
	for _, c := range jar.Cookies(u) {
		if c.Name == authCookieName {
			found = c
		}
	}
	if found == nil {
		t.Fatal("no auth cookie was set by the bootstrap redirect")
	}

	// Cookie alone, no Authorization header, no query param.
	resp2, err := client.Get(base + "/api/status/detailed")
	if err != nil {
		t.Fatalf("GET /api/status/detailed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("cookie-only request: status %d; want 200", resp2.StatusCode)
	}
}

func TestMainTCPListener_TLS(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, 20)
	certFile, keyFile := leaf.writeFiles(t)

	handler := bearerAuth(httpFrontToken, echoMux())
	_, base := startTestFront(t, certFile, keyFile, handler)

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.pem) {
		t.Fatal("append CA pem")
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}

	req, _ := http.NewRequest(http.MethodGet, base+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+httpFrontToken)
	resp, err := client.Do(req)
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
