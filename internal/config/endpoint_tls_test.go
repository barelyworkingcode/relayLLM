package config

// Coverage for endpoint_tls.go: the fail-closed validation for the
// relayLLM-to-upstream OpenAI-endpoint hop. Certificates are generated
// in-process (see the minimal testCA helper below) — no openssl, no network.

import (
	"bytes"
	"log/slog"
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
	ca := newConfigTestCA(t)
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
