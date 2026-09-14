package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/gorilla/websocket"

	"relayllm/internal/permission"
	"relayllm/internal/types"
)

// Full-stack coverage of the per-session hook token against the real
// permission route (TestServer wires HookScopedBearerAuth + the real
// permission.PermissionManager, matching app.go). security_hooktoken_test.go
// covers the middleware in isolation; these tests cover the handler's own
// session-match check (api.go's RegisterPermissionRoutes) and the full
// mint -> use -> revoke lifecycle, which the middleware alone can't see.

func hookTokenRequest(t *testing.T, srv *TestServer, token string, body map[string]interface{}) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.HTTP.URL+"/api/permission", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/permission: %v", err)
	}
	return resp
}

func TestAPI_HookToken_WorksOnPermissionRouteForOwnSession(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	// An allow-by-policy rule makes the handler answer synchronously,
	// exactly the shape the hook binary itself waits on — no WS client
	// needed to resolve a pending request for this test to observe a real
	// decision.
	sess, _ := srv.Sessions.GetSession(sessionID)
	sess.Policy = &types.PermissionPolicy{AllowedTools: []string{"Read"}}

	token := srv.Perms.MintHookToken(sessionID)
	resp := hookTokenRequest(t, srv, token, map[string]interface{}{
		"sessionId": sessionID, "toolName": "Read", "toolInput": `{"path":"/tmp/x"}`, "toolUseId": "t1",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var decision permission.PermissionDecision
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		t.Fatalf("decode decision: %v", err)
	}
	if decision.Decision != "allow" {
		t.Errorf("decision = %+v; want allow", decision)
	}
}

func TestAPI_HookToken_RefusedForADifferentSessionId(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	token := srv.Perms.MintHookToken(sessionID)
	resp := hookTokenRequest(t, srv, token, map[string]interface{}{
		"sessionId": "a-completely-different-session", "toolName": "Read", "toolInput": "{}", "toolUseId": "t1",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d; want 403 (token valid, but not for this session)", resp.StatusCode)
	}
}

func TestAPI_HookToken_RefusedAfterSessionEnds(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	token := srv.Perms.MintHookToken(sessionID)
	// What a real ClaudeProvider.Kill() does on session end/delete.
	srv.Perms.RevokeHookToken(sessionID)

	resp := hookTokenRequest(t, srv, token, map[string]interface{}{
		"sessionId": sessionID, "toolName": "Read", "toolInput": "{}", "toolUseId": "t1",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 (token revoked with the session)", resp.StatusCode)
	}
}

func TestAPI_HookToken_RefusedOnOrdinaryRoutesEndToEnd(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)
	token := srv.Perms.MintHookToken(sessionID)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/sessions"},
		{"GET", "/api/status"},
		{"GET", "/api/terminals"},
	} {
		req, err := http.NewRequest(tc.method, srv.HTTP.URL+tc.path, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with a hook token: status %d; want 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestAPI_HookToken_RefusedOnWebSocketUpgrade(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)
	token := srv.Perms.MintHookToken(sessionID)

	u, err := url.Parse(srv.HTTP.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	u.Scheme = "ws"
	u.Path = "/ws"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)

	conn, resp, err := websocket.DefaultDialer.Dial(u.String(), hdr)
	if err == nil {
		conn.Close()
		t.Fatal("expected the WS upgrade to be refused for a hook token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		status := -1
		if resp != nil {
			status = resp.StatusCode
		}
		t.Errorf("WS upgrade status = %d; want 401", status)
	}
}
