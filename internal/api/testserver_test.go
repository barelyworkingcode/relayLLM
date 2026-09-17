package api

// TestServer composes relayLLM's surviving HTTP stack (status/llama/mlx)
// against an httptest listener. Tests get a one-call factory that returns a
// ready-to-drive server.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	clk "relayllm/internal/clock"
)

const supportBearerToken = "test-bearer-token"

// TestServer wraps an in-process relayLLM stack and exposes helpers tests
// commonly need (POST/GET/DELETE JSON). Cleanup is automatic via t.Cleanup,
// so callers never have to remember to Close.
type TestServer struct {
	t     *testing.T
	HTTP  *httptest.Server
	Token string
	// Recovered is the handler value below bearerAuth — the same one
	// app.go wraps in TCPDiagnosticsOnly for the --http-port front. Exposed
	// so tests (tcp_diagnostics_test.go) can build both fronts from one mux
	// without duplicating the full route registration.
	Recovered http.Handler
}

// TestServerOptions controls how the server is wired.
type TestServerOptions struct {
	// Clock for any future Clock-aware code. Default: DefaultClock.
	Clock clk.Clock
}

// NewTestServer spins up relayLLM's surviving HTTP stack: status, llama, and
// mlx instance routes plus the detailed status dashboard, matching exactly
// what app.go's Main registers. No manager is wired (nil llama/mlx), so
// every test through this factory exercises the "no manager configured"
// paths — the only ones this package's tests need.
func NewTestServer(t *testing.T, opts *TestServerOptions) *TestServer {
	t.Helper()
	if opts == nil {
		opts = &TestServerOptions{}
	}

	mux := http.NewServeMux()
	RegisterStatusRoutes(mux, nil, nil, time.Now())
	RegisterDetailedStatusRoutes(mux, DetailedStatusDeps{
		StartTime: time.Now(),
		Clock:     opts.Clock,
	})

	recovered := RecoverMiddleware(mux)
	handler := BearerAuth(supportBearerToken, recovered)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &TestServer{
		t:         t,
		HTTP:      srv,
		Token:     supportBearerToken,
		Recovered: recovered,
	}
}

// PostJSON sends a JSON POST and decodes the JSON response if respBody is non-nil.
func (s *TestServer) PostJSON(path string, reqBody, respBody interface{}) *http.Response {
	return s.doJSON("POST", path, reqBody, respBody)
}

// DeleteJSON sends a DELETE (no body), decoding JSON response if asked.
func (s *TestServer) DeleteJSON(path string, respBody interface{}) *http.Response {
	return s.doJSON("DELETE", path, nil, respBody)
}

// GetJSON sends a GET and decodes the JSON response.
func (s *TestServer) GetJSON(path string, respBody interface{}) *http.Response {
	return s.doJSON("GET", path, nil, respBody)
}

func (s *TestServer) doJSON(method, path string, reqBody, respBody interface{}) *http.Response {
	s.t.Helper()
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			s.t.Fatalf("marshal request body: %v", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, s.HTTP.URL+path, body)
	if err != nil {
		s.t.Fatalf("build request: %v", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	if respBody != nil {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err := json.Unmarshal(data, respBody); err != nil {
			s.t.Fatalf("decode response (status %d, body %q): %v", resp.StatusCode, string(data), err)
		}
		// Replace the body with a fresh reader so callers can still read it if they want.
		resp.Body = io.NopCloser(bytes.NewReader(data))
	}
	return resp
}

// RawRequest returns a *http.Request prepared with bearer auth — useful for
// tests that want to manipulate headers or stream the body manually.
func (s *TestServer) RawRequest(method, path string, body io.Reader) *http.Request {
	s.t.Helper()
	req, err := http.NewRequest(method, s.HTTP.URL+path, body)
	if err != nil {
		s.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	return req
}
