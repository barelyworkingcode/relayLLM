package main

// Coverage for the two router-side TLS surfaces added alongside
// endpoint_tls.go:
//
//   - the endpoint-backed proxy path (routeOpenAI) actually dials upstream
//     through the endpoint's pinned transport, not a bare http.DefaultTransport
//   - the router's own listener can serve TLS and refuses plaintext once it does
//
// Certificates come from tls_test_helpers_test.go — no openssl, no network
// beyond loopback.

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRouter_Proxy_EndpointModel_PinnedTLSUpstream covers item 3 of the
// endpoint-TLS test plan: a matching pin forwards successfully, and a
// mismatched pin on the same endpoint entry surfaces as a 502.
//
// The registry's /v1/models catalog probe and the actual chat-completions
// forward share the same endpoint transport (both go through ep.Transport()),
// so the first request below both proves the pinned proxy path works AND
// warms ProxyRegistry's 15s freshness cache to Online. The second request
// then re-pins the SAME cfg.Endpoints[0] entry to a fingerprint the server
// will never present: ProxyRegistry.LookupModel re-reads the live cfg on
// every call but skips re-probing while the cached status is still fresh
// (see its isFresh check), so it reports the endpoint Online from the first
// probe while handing back the newly mismatched endpoint value. That routes
// the request all the way to the real upstream dial — isolating the
// assertion to the proxy path's transport, not the catalog probe.
func TestRouter_Proxy_EndpointModel_PinnedTLSUpstream(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, 10)

	var seenAuth string
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "Qwen"}},
			})
		case "/v1/chat/completions":
			seenAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"resp","choices":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{leaf.tlsCert}}
	upstream.StartTLS()
	defer upstream.Close()

	cfg := &OpenAIConfig{
		Endpoints: []OpenAIEndpoint{
			{
				Name:      "fakeep",
				BaseURL:   upstream.URL + "/v1",
				APIKey:    "upstream-key",
				CAFile:    ca.writeCAFile(t),
				PinSHA256: []string{fingerprintSHA256(leaf.cert)},
			},
		},
	}
	if err := prepareEndpointTransports(&cfg.Endpoints[0], false); err != nil {
		t.Fatalf("prepareEndpointTransports: %v", err)
	}
	registry := NewProxyRegistry(cfg)

	r := NewRelayRouter(":0", nil, registry, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"fakeep/Qwen","stream":false}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("matching pin: expected 200, got %d body=%s", resp.StatusCode, body)
	}
	if seenAuth != "Bearer upstream-key" {
		t.Errorf("upstream Authorization: got %q, want %q", seenAuth, "Bearer upstream-key")
	}

	cfg.Endpoints[0].PinSHA256 = []string{strings.Repeat("00", 32)}
	if err := prepareEndpointTransports(&cfg.Endpoints[0], false); err != nil {
		t.Fatalf("prepareEndpointTransports (mismatched pin): %v", err)
	}

	resp2 := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"fakeep/Qwen","stream":false}`))
	if resp2.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("mismatched pin: expected 502, got %d body=%s", resp2.StatusCode, body)
	}
}

// freeTCPAddr binds then immediately releases a loopback port, following the
// same bind-and-release idiom TestPreflightPortFree_SucceedsForFreePort uses
// (server_manager_test.go) to hand a real, currently-free address to a
// component (here, StartRelayRouter) that wants to do its own net.Listen.
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

// waitForTLSHandshake polls until addr accepts a TLS handshake under trust,
// or fails the test after a short timeout — StartRelayRouter's ListenAndServe
// runs in a background goroutine, so the listener isn't guaranteed bound the
// instant the function returns.
func waitForTLSHandshake(t *testing.T, addr string, trust *x509.CertPool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: trust})
		if err == nil {
			conn.Close()
			return
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("router TLS listener never came up at %s: %v", addr, lastErr)
}

// TestRelayRouter_TLSListener_ServesHTTPSAndRejectsPlainHTTP covers item 4:
// the --router-tls-cert/--router-tls-key pair (wired via StartRelayRouter's
// trailing tlsCert/tlsKey parameters, set with setTLS before the serving
// goroutine starts — see StartRelayRouter's doc comment) makes the listener
// speak TLS, and a plain http request to that same port no longer works.
func TestRelayRouter_TLSListener_ServesHTTPSAndRejectsPlainHTTP(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, 30)
	certPath, keyPath := leaf.writeFiles(t)

	addr := freeTCPAddr(t)
	mgr := NewServerManager(llamaProfile, &ServerConfig{
		Models: []ServerModelConfig{{Alias: "a"}},
	}, "")
	router := StartRelayRouter(addr, []*ServerManager{mgr}, nil, nil, nil, certPath, keyPath)
	if router == nil {
		t.Fatal("expected a non-nil router")
	}
	defer router.Close()

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	waitForTLSHandshake(t, addr, pool)

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	resp, err := client.Get("https://" + addr + "/v1/models")
	if err != nil {
		t.Fatalf("https request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("https /v1/models: got status %d, want 200", resp.StatusCode)
	}

	// net/http's server detects a plaintext request arriving on a TLS
	// listener and writes back a plaintext 400 rather than dropping the
	// connection — so this must assert on the status, not on a transport
	// error, or it would pass against any status including a stray 200.
	plainResp, err := http.Get("http://" + addr + "/v1/models")
	if err != nil {
		return
	}
	defer plainResp.Body.Close()
	if plainResp.StatusCode == http.StatusOK {
		t.Fatalf("expected plain http request to a TLS-only listener to fail, got %d", plainResp.StatusCode)
	}
}
