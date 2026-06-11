package main

// TestServer composes the full relayLLM HTTP+WS stack against an httptest
// listener, using the fakes from support_test.go for the LLM provider and
// MCP. Tests get a one-call factory that returns a ready-to-drive server.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const supportBearerToken = "test-bearer-token"

// TestServer wraps an in-process relayLLM stack and exposes helpers tests
// commonly need (POST/GET JSON, WS dialing). Cleanup is automatic via
// t.Cleanup, so callers never have to remember to Close.
type TestServer struct {
	t            *testing.T
	HTTP         *httptest.Server
	DataDir      string
	Sessions     *SessionManager
	Perms        *PermissionManager
	Terminals    *TerminalManager
	WSHub        *WSHub
	Token        string
	FakeProvider *FakeProvider // set if ProviderFromFake is used
}

// TestServerOptions controls how the server is wired.
type TestServerOptions struct {
	// Clock for permission-timeout (and any future Clock-aware code). Default: DefaultClock.
	Clock Clock
	// ProviderFactory injected into SessionManager. If non-nil, replaces the
	// real switch on session.ProviderType. Default: nil (i.e. real providers,
	// which most tests will not want).
	ProviderFactory func(*Session, EventHandler) (Provider, error)
	// MCP is the MCPClient that scripted providers should reference. Tests
	// that don't need tool calling can leave this nil.
	MCP MCPClient
}

// NewTestServer spins up a minimal relayLLM HTTP+WS stack. Defaults are
// chosen so a test that just wants to drive sessions through a FakeProvider
// gets working endpoints with one line:
//
//	srv := NewTestServer(t, nil)
//	srv.SetFakeProvider() // installs a FakeProvider; returns it
func NewTestServer(t *testing.T, opts *TestServerOptions) *TestServer {
	t.Helper()
	if opts == nil {
		opts = &TestServerOptions{}
	}
	if opts.Clock == nil {
		opts.Clock = DefaultClock
	}

	dataDir := t.TempDir()

	sessionStore := NewSessionStore(dataDir + "/sessions")
	perms := NewPermissionManager()
	perms.SetClock(opts.Clock)
	sessions := NewSessionManager(sessionStore, perms)
	sessions.SetDataDir(dataDir)

	templateStore := NewTemplateStore(dataDir + "/terminals/templates.json")
	if err := templateStore.Load(nil); err != nil {
		t.Fatalf("template store load: %v", err)
	}
	terminals := NewTerminalManager(templateStore, dataDir+"/terminal_logs")
	wsHub := NewWSHub(sessions, perms, terminals)
	sessions.SetEventSink(wsHub)
	perms.SetEventSink(wsHub)
	sessions.SetHookSocket("")

	// Mirror main.go: terminal I/O + exit events bridge to the WS hub.
	terminals.SetOutputHandler(func(id string, data []byte) {
		wsHub.SendToTerminal(id, map[string]interface{}{
			"type":       "terminal_output",
			"terminalId": id,
			"data":       base64.StdEncoding.EncodeToString(data),
		})
	})
	terminals.SetExitHandler(func(id string, exitCode int) {
		wsHub.SendToTerminal(id, map[string]interface{}{
			"type":       "terminal_exit",
			"terminalId": id,
			"exitCode":   exitCode,
		})
	})

	if opts.ProviderFactory != nil {
		sessions.SetProviderFactory(opts.ProviderFactory)
	}

	mux := http.NewServeMux()
	RegisterSessionRoutes(mux, sessions)
	RegisterTerminalRoutes(mux, templateStore, terminals)
	RegisterPermissionRoutes(mux, perms, sessions)
	// Empty configs are fine — /api/models just returns nothing when nothing's wired.
	RegisterModelRoutes(mux, "", nil, nil, nil, nil, sessions.piOverlayInputs)
	RegisterGeneratedImageRoutes(mux, dataDir)
	RegisterStatusRoutes(mux, sessions, terminals, nil, nil, time.Now())
	mux.HandleFunc("/ws", wsHub.HandleUpgrade)

	handler := bearerAuth(supportBearerToken, recoverMiddleware(mux))
	srv := httptest.NewServer(handler)

	t.Cleanup(func() {
		sessions.StopAll()
		srv.Close()
	})

	return &TestServer{
		t:         t,
		HTTP:      srv,
		DataDir:   dataDir,
		Sessions:  sessions,
		Perms:     perms,
		Terminals: terminals,
		WSHub:     wsHub,
		Token:     supportBearerToken,
	}
}

// SetFakeProvider installs a SessionManager provider factory that constructs
// the same shared FakeProvider for every session — convenient when a test
// only opens one session. Returns the FakeProvider so the test can script it.
func (s *TestServer) SetFakeProvider() *FakeProvider {
	s.t.Helper()
	fp := NewFakeProvider(nil)
	s.Sessions.SetProviderFactory(func(_ *Session, h EventHandler) (Provider, error) {
		fp.SetHandler(h)
		return fp, nil
	})
	s.FakeProvider = fp
	return fp
}

// PostJSON sends a JSON POST and decodes the JSON response if respBody is non-nil.
func (s *TestServer) PostJSON(path string, reqBody, respBody interface{}) *http.Response {
	return s.doJSON("POST", path, reqBody, respBody)
}

// PutJSON sends a JSON PUT.
func (s *TestServer) PutJSON(path string, reqBody, respBody interface{}) *http.Response {
	return s.doJSON("PUT", path, reqBody, respBody)
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

// DialWS opens a WebSocket connection to /ws with bearer auth.
func (s *TestServer) DialWS() *websocket.Conn {
	s.t.Helper()
	u, err := url.Parse(s.HTTP.URL)
	if err != nil {
		s.t.Fatalf("parse server URL: %v", err)
	}
	u.Scheme = "ws"
	u.Path = "/ws"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+s.Token)
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), hdr)
	if err != nil {
		s.t.Fatalf("dial ws: %v", err)
	}
	s.t.Cleanup(func() { conn.Close() })
	return conn
}

// CreateSession creates a session via POST /api/sessions and returns the ID.
// Test default: provider=fake (matches what SetFakeProvider installs).
func (s *TestServer) CreateSession(body map[string]interface{}) string {
	s.t.Helper()
	if body == nil {
		body = map[string]interface{}{}
	}
	if _, ok := body["providerType"]; !ok {
		body["providerType"] = "fake"
	}
	if _, ok := body["model"]; !ok {
		body["model"] = "fake/fake"
	}
	if _, ok := body["directory"]; !ok {
		body["directory"] = s.DataDir
	}
	var resp struct {
		SessionID string `json:"sessionId"`
	}
	httpResp := s.PostJSON("/api/sessions", body, &resp)
	if httpResp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(httpResp.Body)
		s.t.Fatalf("CreateSession: expected 201, got %d: %s", httpResp.StatusCode, string(data))
	}
	if resp.SessionID == "" {
		s.t.Fatal("CreateSession: empty sessionId")
	}
	return resp.SessionID
}

// ReadWSEvent waits up to 2 seconds for a single WS event of any type.
func ReadWSEvent(t *testing.T, conn *websocket.Conn) map[string]interface{} {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read ws: %v", err)
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("decode ws message %q: %v", string(data), err)
	}
	return msg
}

// ReadUntilType reads WS events until one matches eventType, returning it.
// Fails the test if the deadline passes before a match.
func ReadUntilType(t *testing.T, conn *websocket.Conn, eventType string, timeout time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read ws while waiting for %q: %v", eventType, err)
		}
		var msg map[string]interface{}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg["type"] == eventType {
			return msg
		}
	}
	t.Fatalf("ReadUntilType: %q not seen within %v", eventType, timeout)
	return nil
}

// WSSend marshals + writes a JSON message to the WS connection.
func WSSend(t *testing.T, conn *websocket.Conn, msg map[string]interface{}) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal ws message: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write ws: %v", err)
	}
}
