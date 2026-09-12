package main

// Coverage for FetchOpenAIModels (provider_openai.go) against a real TLS
// server, exercising the pinned/CA-anchored transports built by
// config.PrepareEndpointTransports. Certificates are generated in-process
// (tls_test_helpers_test.go) — no openssl, no network beyond loopback
// httptest servers.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"relayllm/internal/config"
)

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

	ep := config.OpenAIEndpoint{Name: "ep", BaseURL: srv.URL, CAFile: ca.writeCAFile(t)}
	if err := config.PrepareEndpointTransports(&ep, false); err != nil {
		t.Fatalf("PrepareEndpointTransports: %v", err)
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

	ep := config.OpenAIEndpoint{Name: "ep", BaseURL: srv.URL}
	if err := config.PrepareEndpointTransports(&ep, false); err != nil {
		t.Fatalf("PrepareEndpointTransports: %v", err)
	}

	if _, err := FetchOpenAIModels(context.Background(), ep); err == nil {
		t.Fatal("expected a certificate verification failure with no caFile configured")
	}
}

func TestFetchOpenAIModels_PinMatchesServedLeaf_Succeeds(t *testing.T) {
	ca := newTestCA(t)
	leaf1 := ca.issueLeaf(t, 4)
	srv := newModelsTLSServer(t, leaf1)

	ep := config.OpenAIEndpoint{
		Name:      "ep",
		BaseURL:   srv.URL,
		CAFile:    ca.writeCAFile(t),
		PinSHA256: []string{fingerprintSHA256(leaf1.cert)},
	}
	if err := config.PrepareEndpointTransports(&ep, false); err != nil {
		t.Fatalf("PrepareEndpointTransports: %v", err)
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

	ep := config.OpenAIEndpoint{
		Name:      "ep",
		BaseURL:   srv.URL,
		CAFile:    ca.writeCAFile(t),
		PinSHA256: []string{fingerprintSHA256(leaf1.cert)}, // ...but we pinned leaf1.
	}
	if err := config.PrepareEndpointTransports(&ep, false); err != nil {
		t.Fatalf("PrepareEndpointTransports: %v", err)
	}

	_, err := FetchOpenAIModels(context.Background(), ep)
	if err == nil {
		t.Fatal("expected pin mismatch to fail the request")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("error = %v, want it to mention \"fingerprint\"", err)
	}
}
