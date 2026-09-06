package main

// Coverage for endpoint_tls.go: the fail-closed validation and pinned
// transport construction for the relayLLM-to-upstream OpenAI-endpoint hop.
// Certificates are generated in-process (tls_test_helpers_test.go) — no
// openssl, no network beyond loopback httptest servers.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// captureWarnings temporarily swaps slog's default logger for one writing to
// a buffer, restoring the original on test cleanup, so a test can assert a
// specific warning fired without depending on log format details beyond
// substring containment.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// ---------------------------------------------------------------------------
// validateEndpointTransport
// ---------------------------------------------------------------------------

func TestValidateEndpointTransport_HTTPNonLoopback_Rejected(t *testing.T) {
	ep := OpenAIEndpoint{Name: "remote", BaseURL: "http://example.com/v1"}
	if err := validateEndpointTransport(ep, false); err == nil {
		t.Fatal("expected error for plaintext http to a non-loopback host")
	}
}

func TestValidateEndpointTransport_HTTPNonLoopback_AllowedWithFlag_WarnsOnce(t *testing.T) {
	buf := captureWarnings(t)
	ep := OpenAIEndpoint{Name: "remote", BaseURL: "http://example.com/v1"}
	if err := validateEndpointTransport(ep, true); err != nil {
		t.Fatalf("expected allowPlaintextEndpoints to permit this endpoint, got: %v", err)
	}
	if !strings.Contains(buf.String(), "remote") || !strings.Contains(strings.ToLower(buf.String()), "plaintext") {
		t.Errorf("expected a plaintext warning naming the endpoint, got log: %s", buf.String())
	}
}

func TestValidateEndpointTransport_HTTPLoopback_OK(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]"} {
		ep := OpenAIEndpoint{Name: "local", BaseURL: "http://" + host + ":8080/v1"}
		if err := validateEndpointTransport(ep, false); err != nil {
			t.Errorf("host %q: expected loopback http to be allowed without the flag, got: %v", host, err)
		}
	}
}

func TestValidateEndpointTransport_BadScheme_Rejected(t *testing.T) {
	ep := OpenAIEndpoint{Name: "ftp", BaseURL: "ftp://example.com/v1"}
	if err := validateEndpointTransport(ep, true); err == nil {
		t.Fatal("expected error for non-http(s) scheme")
	}
}

func TestValidateEndpointTransport_HTTPS_MissingCAFile_Rejected(t *testing.T) {
	ep := OpenAIEndpoint{Name: "https-ep", BaseURL: "https://example.com/v1", CAFile: "/nonexistent/ca.pem"}
	if err := validateEndpointTransport(ep, false); err == nil {
		t.Fatal("expected error for an unreadable caFile")
	}
}

func TestValidateEndpointTransport_CAFile_NoCerts_Rejected(t *testing.T) {
	path := writeTempFile(t, "not a certificate")
	ep := OpenAIEndpoint{Name: "https-ep", BaseURL: "https://example.com/v1", CAFile: path}
	if err := validateEndpointTransport(ep, false); err == nil {
		t.Fatal("expected error for a caFile with no certificates")
	}
}

func TestValidateEndpointTransport_MalformedPin_Rejected(t *testing.T) {
	cases := [][]string{
		{"not-hex-and-wrong-length"},
		{"deadbeef"},                     // too short
		{"zz" + strings.Repeat("a", 62)}, // invalid hex
	}
	for _, pins := range cases {
		ep := OpenAIEndpoint{Name: "https-ep", BaseURL: "https://example.com/v1", PinSHA256: pins}
		if err := validateEndpointTransport(ep, false); err == nil {
			t.Errorf("pins %v: expected malformed-pin error", pins)
		}
	}
}

func TestValidateEndpointTransport_PinsWithHTTP_Rejected(t *testing.T) {
	validPin := strings.Repeat("ab", 32)
	ep := OpenAIEndpoint{Name: "local", BaseURL: "http://127.0.0.1:8080/v1", PinSHA256: []string{validPin}}
	if err := validateEndpointTransport(ep, false); err == nil {
		t.Fatal("expected error: pinSHA256 has no effect on an http baseURL")
	}
}

func TestValidateEndpointTransport_CAFileWithHTTP_Rejected(t *testing.T) {
	ca := newTestCA(t)
	ep := OpenAIEndpoint{Name: "local", BaseURL: "http://127.0.0.1:8080/v1", CAFile: ca.writeCAFile(t)}
	if err := validateEndpointTransport(ep, false); err == nil {
		t.Fatal("expected error: caFile has no effect on an http baseURL")
	}
}

func TestNormalizePin_LowercasesAndStripsColons(t *testing.T) {
	raw := "AA:BB:CC:DD:" + strings.ToUpper(strings.Repeat("ef", 28))
	got, err := normalizePin(raw)
	if err != nil {
		t.Fatalf("normalizePin: %v", err)
	}
	want := strings.ToLower(strings.ReplaceAll(raw, ":", ""))
	if got != want {
		t.Errorf("normalizePin(%q) = %q, want %q", raw, got, want)
	}
	if len(got) != 64 {
		t.Errorf("normalized pin length = %d, want 64", len(got))
	}
}

func writeTempFile(t *testing.T, contents string) string {
	t.Helper()
	path := t.TempDir() + "/f"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// FetchOpenAIModels against a real TLS server
// ---------------------------------------------------------------------------

func newModelsTLSServer(t *testing.T, leaf testLeaf) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "some-model"}},
		})
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf.tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchOpenAIModels_CAFile_TrustsServer(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, 2)
	srv := newModelsTLSServer(t, leaf)

	ep := OpenAIEndpoint{Name: "ep", BaseURL: srv.URL, CAFile: ca.writeCAFile(t)}
	if err := prepareEndpointTransports(&ep, false); err != nil {
		t.Fatalf("prepareEndpointTransports: %v", err)
	}

	models, err := FetchOpenAIModels(context.Background(), ep)
	if err != nil {
		t.Fatalf("FetchOpenAIModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "some-model" {
		t.Errorf("models = %+v", models)
	}
}

func TestFetchOpenAIModels_NoCAFile_FailsVerification(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, 3)
	srv := newModelsTLSServer(t, leaf)

	ep := OpenAIEndpoint{Name: "ep", BaseURL: srv.URL}
	if err := prepareEndpointTransports(&ep, false); err != nil {
		t.Fatalf("prepareEndpointTransports: %v", err)
	}

	if _, err := FetchOpenAIModels(context.Background(), ep); err == nil {
		t.Fatal("expected a certificate verification failure with no caFile configured")
	}
}

func TestFetchOpenAIModels_PinMatchesServedLeaf_Succeeds(t *testing.T) {
	ca := newTestCA(t)
	leaf1 := ca.issueLeaf(t, 4)
	srv := newModelsTLSServer(t, leaf1)

	ep := OpenAIEndpoint{
		Name:      "ep",
		BaseURL:   srv.URL,
		CAFile:    ca.writeCAFile(t),
		PinSHA256: []string{fingerprintSHA256(leaf1.cert)},
	}
	if err := prepareEndpointTransports(&ep, false); err != nil {
		t.Fatalf("prepareEndpointTransports: %v", err)
	}

	if _, err := FetchOpenAIModels(context.Background(), ep); err != nil {
		t.Fatalf("FetchOpenAIModels: %v", err)
	}
}

// The MITM-with-a-valid-cert case: leaf2 chains to the SAME trusted CA as
// leaf1 (so plain chain verification alone would accept it), but the pin
// names leaf1's fingerprint specifically. The connection must still fail.
func TestFetchOpenAIModels_PinMismatch_ValidCertDifferentLeaf_Fails(t *testing.T) {
	ca := newTestCA(t)
	leaf1 := ca.issueLeaf(t, 5)
	leaf2 := ca.issueLeaf(t, 6)
	srv := newModelsTLSServer(t, leaf2) // server presents leaf2...

	ep := OpenAIEndpoint{
		Name:      "ep",
		BaseURL:   srv.URL,
		CAFile:    ca.writeCAFile(t),
		PinSHA256: []string{fingerprintSHA256(leaf1.cert)}, // ...but we pinned leaf1.
	}
	if err := prepareEndpointTransports(&ep, false); err != nil {
		t.Fatalf("prepareEndpointTransports: %v", err)
	}

	_, err := FetchOpenAIModels(context.Background(), ep)
	if err == nil {
		t.Fatal("expected pin mismatch to fail the request")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("error = %v, want it to mention \"fingerprint\"", err)
	}
}
