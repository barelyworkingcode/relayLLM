package main

// HTTP API coverage. Hits every route through TestServer. The
// permission-timeout test exercises the Clock injection done in Phase 2.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ----------------------------------------------------------------------------
// Auth
// ----------------------------------------------------------------------------

func TestAPI_AuthRequired_OnEveryRoute(t *testing.T) {
	srv := NewTestServer(t, nil)

	routes := []struct {
		method, path string
		body         []byte
	}{
		{"GET", "/api/sessions", nil},
		{"POST", "/api/sessions", []byte(`{}`)},
		{"GET", "/api/status", nil},
		{"GET", "/api/models", nil},
		{"GET", "/api/llama/instances", nil},
	}
	for _, r := range routes {
		var body *bytes.Reader
		if r.body != nil {
			body = bytes.NewReader(r.body)
		}
		var req *http.Request
		var err error
		if body != nil {
			req, err = http.NewRequest(r.method, srv.HTTP.URL+r.path, body)
		} else {
			req, err = http.NewRequest(r.method, srv.HTTP.URL+r.path, nil)
		}
		if err != nil {
			t.Fatalf("build req: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without bearer: got %d, want 401", r.method, r.path, resp.StatusCode)
		}
	}
}

// ----------------------------------------------------------------------------
// Sessions
// ----------------------------------------------------------------------------

func TestAPI_PostSession_CreatesAndReturnsIDs(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()

	var resp struct {
		SessionID string `json:"sessionId"`
		Directory string `json:"directory"`
		Model     string `json:"model"`
	}
	httpResp := srv.PostJSON("/api/sessions", map[string]interface{}{
		"providerType": "fake",
		"model":        "fake/m1",
		"directory":    srv.DataDir,
		"name":         "first",
	}, &resp)
	if httpResp.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d", httpResp.StatusCode)
	}
	if resp.SessionID == "" {
		t.Error("empty sessionId")
	}
	if resp.Model != "fake/m1" {
		t.Errorf("model: got %q", resp.Model)
	}
}

func TestAPI_GetSessions_ReturnsCreatedSessions(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(map[string]interface{}{"name": "listed"})

	var list []map[string]interface{}
	srv.GetJSON("/api/sessions", &list)

	for _, s := range list {
		if s["id"] == sessionID || s["sessionId"] == sessionID {
			return
		}
	}
	t.Errorf("session %s not in list: %v", sessionID, list)
}

func TestAPI_PostMessageSync_ReturnsAccumulatedResponse(t *testing.T) {
	srv := NewTestServer(t, nil)
	fp := srv.SetFakeProvider()

	fp.ScriptText("hello world")
	fp.ScriptResult("end_turn", SessionStats{InputTokens: 5, OutputTokens: 2})

	sessionID := srv.CreateSession(nil)

	var resp struct {
		Response string       `json:"response"`
		Stats    SessionStats `json:"stats"`
	}
	httpResp := srv.PostJSON("/api/sessions/"+sessionID+"/message",
		map[string]interface{}{"text": "ping"}, &resp)
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d", httpResp.StatusCode)
	}
	if !strings.Contains(resp.Response, "hello world") {
		t.Errorf("response: got %q, want to contain 'hello world'", resp.Response)
	}
	if resp.Stats.InputTokens != 5 {
		t.Errorf("InputTokens: got %d, want 5", resp.Stats.InputTokens)
	}
}

func TestAPI_PostMessage_EmptyText_Returns400(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	httpResp := srv.PostJSON("/api/sessions/"+sessionID+"/message",
		map[string]interface{}{"text": ""}, nil)
	if httpResp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty text: got %d, want 400", httpResp.StatusCode)
	}
}

func TestAPI_PostStop_CallsProviderStop(t *testing.T) {
	srv := NewTestServer(t, nil)
	fp := srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	httpResp := srv.PostJSON("/api/sessions/"+sessionID+"/stop", map[string]string{}, nil)
	if httpResp.StatusCode != http.StatusOK {
		t.Errorf("stop status: got %d", httpResp.StatusCode)
	}
	waitFor(t, 1*time.Second, func() bool { return fp.Stopped() })
}

func TestAPI_DeleteSession_EndsSession(t *testing.T) {
	srv := NewTestServer(t, nil)
	fp := srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	httpResp := srv.DeleteJSON("/api/sessions/"+sessionID, nil)
	if httpResp.StatusCode != http.StatusOK {
		t.Errorf("delete: got %d", httpResp.StatusCode)
	}
	waitFor(t, 1*time.Second, func() bool { return fp.Killed() })
}

func TestAPI_PutModel_RejectsNonPiSession(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil) // providerType="fake"

	httpResp := srv.PutJSON("/api/sessions/"+sessionID+"/model",
		map[string]string{"provider": "anthropic", "modelId": "claude-sonnet-4"}, nil)
	if httpResp.StatusCode != http.StatusBadRequest {
		t.Errorf("set model on non-pi session: got %d, want 400", httpResp.StatusCode)
	}
}

func TestAPI_PutThinkingLevel_RejectsNonPiSession(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	httpResp := srv.PutJSON("/api/sessions/"+sessionID+"/thinking-level",
		map[string]string{"level": "high"}, nil)
	if httpResp.StatusCode != http.StatusBadRequest {
		t.Errorf("set thinking on non-pi session: got %d, want 400", httpResp.StatusCode)
	}
}

// ----------------------------------------------------------------------------
// Status / models / llama
// ----------------------------------------------------------------------------

func TestAPI_GetStatus_ReturnsShape(t *testing.T) {
	srv := NewTestServer(t, nil)
	var status map[string]interface{}
	srv.GetJSON("/api/status", &status)
	for _, k := range []string{"uptimeSeconds", "sessions", "terminals", "llamaInstances"} {
		if _, ok := status[k]; !ok {
			t.Errorf("status missing %q: %v", k, status)
		}
	}
}

func TestAPI_GetModels_ReturnsClaudeDefaults(t *testing.T) {
	srv := NewTestServer(t, nil)
	var resp struct {
		Models []map[string]interface{} `json:"models"`
	}
	srv.GetJSON("/api/models", &resp)
	if len(resp.Models) == 0 {
		t.Fatal("expected at least the built-in Claude models")
	}
	foundClaude := false
	for _, m := range resp.Models {
		if m["provider"] == "claude" {
			foundClaude = true
			break
		}
	}
	if !foundClaude {
		t.Errorf("no claude entries in /api/models: %v", resp.Models)
	}
}

func TestAPI_GetLlamaInstances_EmptyWhenNoManager(t *testing.T) {
	srv := NewTestServer(t, nil)
	var arr []interface{}
	srv.GetJSON("/api/llama/instances", &arr)
	if len(arr) != 0 {
		t.Errorf("expected no instances without llamaManager, got %v", arr)
	}
}

// ----------------------------------------------------------------------------
// Permission — exercises Clock injection
// ----------------------------------------------------------------------------

func TestAPI_PostPermission_TimesOut_DeterministicallyWithFakeClock(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	srv := NewTestServer(t, &TestServerOptions{Clock: clock})
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	// Issue the permission POST in a goroutine. It will block in the
	// select on clock.After(60s). We then Advance to fire the timeout.
	type result struct {
		status int
		body   []byte
	}
	done := make(chan result, 1)
	go func() {
		req := srv.RawRequest("POST", "/api/permission", bytes.NewReader([]byte(
			`{"sessionId":"`+sessionID+`","toolName":"Read","toolInput":"{\"path\":\"/tmp/x\"}","toolUseId":"t1"}`,
		)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("permission request: %v", err)
			done <- result{}
			return
		}
		defer resp.Body.Close()
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		done <- result{status: resp.StatusCode, body: body[:n]}
	}()

	// Wait until the handler has registered its select on the fake clock.
	waitFor(t, 1*time.Second, func() bool { return clock.Waiters() > 0 })

	clock.Advance(61 * time.Second)

	select {
	case r := <-done:
		if r.status != http.StatusOK {
			t.Errorf("status: got %d, want 200", r.status)
		}
		var decision PermissionDecision
		if err := json.Unmarshal(r.body, &decision); err != nil {
			t.Fatalf("decode body %q: %v", string(r.body), err)
		}
		if decision.Decision != "deny" || decision.Reason != "timeout" {
			t.Errorf("decision: %+v", decision)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("permission handler never returned after clock advanced")
	}
}

func TestAPI_PostPermission_AutoAllowsByPolicy(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	// Inject a policy that auto-allows Read.
	sess, _ := srv.Sessions.GetSession(sessionID)
	sess.Policy = &PermissionPolicy{AllowedTools: []string{"Read"}}

	var decision PermissionDecision
	srv.PostJSON("/api/permission", map[string]interface{}{
		"sessionId": sessionID,
		"toolName":  "Read",
		"toolInput": `{"path":"/tmp/x"}`,
		"toolUseId": "t1",
	}, &decision)
	if decision.Decision != "allow" {
		t.Errorf("expected allow, got %+v", decision)
	}
}

func TestAPI_PostPermission_AutoDeniesByPolicy(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	sess, _ := srv.Sessions.GetSession(sessionID)
	sess.Policy = &PermissionPolicy{DeniedTools: []string{"fs_bash"}}

	var decision PermissionDecision
	srv.PostJSON("/api/permission", map[string]interface{}{
		"sessionId": sessionID,
		"toolName":  "fs_bash",
		"toolInput": `{"command":"rm -rf /"}`,
		"toolUseId": "t1",
	}, &decision)
	if decision.Decision != "deny" {
		t.Errorf("expected deny, got %+v", decision)
	}
}

