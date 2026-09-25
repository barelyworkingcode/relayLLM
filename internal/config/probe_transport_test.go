package config_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"relayllm/internal/config"
	"relayllm/internal/testutil"
)

func TestProbeTransport_EnforcesPin(t *testing.T) {
	ca := testutil.NewTestCA(t)
	pinned := ca.IssueLeaf(t, 1)
	other := ca.IssueLeaf(t, 2)

	cases := []struct {
		name    string
		served  testutil.TestLeaf
		wantErr bool
	}{
		{"server presents the pinned leaf", pinned, false},
		{"server presents a valid but unpinned leaf", other, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			srv.TLS = &tls.Config{Certificates: []tls.Certificate{tc.served.TLSCert}}
			srv.StartTLS()
			t.Cleanup(srv.Close)

			ep := config.OpenAIEndpoint{
				Name:      "p1",
				BaseURL:   srv.URL,
				CAFile:    ca.WriteCAFile(t),
				PinSHA256: []string{testutil.FingerprintSHA256(pinned.Cert)},
			}
			if err := config.PrepareEndpointTransports(&ep, false); err != nil {
				t.Fatalf("PrepareEndpointTransports: %v", err)
			}

			resp, err := (&http.Client{Transport: ep.ProbeTransport()}).Get(srv.URL + "/models")
			if err == nil {
				resp.Body.Close()
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("GET through ProbeTransport: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected the pin mismatch to fail the request")
			}
			if !strings.Contains(err.Error(), "fingerprint") {
				t.Errorf("error = %v, want a pin (fingerprint) failure", err)
			}
		})
	}
}
