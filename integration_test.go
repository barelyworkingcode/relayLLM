package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testServer wires up the full relayLLM stack in-process for integration testing.
type testServer struct {
	Server       *httptest.Server
	ProjectStore *ProjectStore
	SessionStore *SessionStore
	Sessions     *SessionManager
	Perms        *PermissionManager
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	dataDir := t.TempDir()

	store := NewProjectStore(dataDir + "/projects.json")
	if err := store.Load(); err != nil {
		t.Fatalf("load project store: %v", err)
	}

	sessionStore := NewSessionStore(dataDir + "/sessions")
	perms := NewPermissionManager()
	sessions := NewSessionManager(store, sessionStore, perms)
	sessions.SetOpenAIConfig(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{
			{Name: "omlx", BaseURL: integOMLXURL, APIKey: integOMLXKey},
		},
	})

	templateStore := NewTemplateStore(dataDir + "/terminals/templates.json")
	terminalMgr := NewTerminalManager(templateStore)
	wsHub := NewWSHub(sessions, perms, terminalMgr)
	sessions.SetEventSink(wsHub)
	perms.sink = wsHub

	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store, nil)
	RegisterSessionRoutes(mux, sessions)
	RegisterPermissionRoutes(mux, perms)
	mux.HandleFunc("/ws", wsHub.HandleUpgrade)

	server := httptest.NewServer(mux)

	// Point the hook URL at our test server so providers can reach the permission endpoint.
	sessions.SetHookURL(server.URL)

	t.Cleanup(func() {
		sessions.StopAll()
		server.Close()
	})

	return &testServer{
		Server:       server,
		ProjectStore: store,
		SessionStore: sessionStore,
		Sessions:     sessions,
		Perms:        perms,
	}
}

// doJSON performs an HTTP request with JSON body and decodes the JSON response.
func doJSON(t *testing.T, method, url string, body interface{}, resp interface{}) *http.Response {
	t.Helper()

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, url, err)
	}
	defer httpResp.Body.Close()

	if resp != nil {
		respBody, err := io.ReadAll(httpResp.Body)
		if err != nil {
			t.Fatalf("read response body: %v", err)
		}
		if err := json.Unmarshal(respBody, resp); err != nil {
			t.Fatalf("unmarshal response (status %d, body: %s): %v", httpResp.StatusCode, string(respBody), err)
		}
	}

	return httpResp
}

// createTestProject creates a project via the API and returns its ID.
func createTestProject(t *testing.T, ts *testServer) string {
	t.Helper()
	projectDir := t.TempDir()

	var project struct {
		ID string `json:"id"`
	}
	resp := doJSON(t, "POST", ts.Server.URL+"/api/projects", map[string]interface{}{
		"name": "integration-test",
		"path": projectDir,
	}, &project)
	if resp.StatusCode != 201 {
		t.Fatalf("create project: expected 201, got %d", resp.StatusCode)
	}
	if project.ID == "" {
		t.Fatal("create project: empty ID")
	}
	return project.ID
}

// createTestSession creates a session via the API and returns its ID.
func createTestSession(t *testing.T, ts *testServer, projectID string) string {
	t.Helper()

	var session struct {
		SessionID string `json:"sessionId"`
	}
	resp := doJSON(t, "POST", ts.Server.URL+"/api/sessions", map[string]interface{}{
		"projectId": projectID,
		"model":     "omlx/gemma-4-31b-it-mxfp8",
	}, &session)
	if resp.StatusCode != 201 {
		t.Fatalf("create session: expected 201, got %d", resp.StatusCode)
	}
	if session.SessionID == "" {
		t.Fatal("create session: empty sessionId")
	}
	return session.SessionID
}

// sendMessage sends a message via the sync HTTP endpoint and returns response + stats.
func sendMessage(t *testing.T, ts *testServer, sessionID, text string) (string, SessionStats) {
	t.Helper()

	var result struct {
		Response string       `json:"response"`
		Stats    SessionStats `json:"stats"`
	}
	resp := doJSON(t, "POST", ts.Server.URL+"/api/sessions/"+sessionID+"/message", map[string]string{
		"text": text,
	}, &result)
	if resp.StatusCode != 200 {
		t.Fatalf("send message: expected 200, got %d", resp.StatusCode)
	}
	return result.Response, result.Stats
}

// endSession ends a session via the API.
func endSession(t *testing.T, ts *testServer, sessionID string) {
	t.Helper()
	resp := doJSON(t, "DELETE", ts.Server.URL+"/api/sessions/"+sessionID, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("end session: expected 200, got %d", resp.StatusCode)
	}
}

// --- Tests ---

func TestIntegration_ProjectCRUD(t *testing.T) {
	ts := newTestServer(t)

	// Create.
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Path string `json:"path"`
	}
	resp := doJSON(t, "POST", ts.Server.URL+"/api/projects", map[string]interface{}{
		"name": "test-project",
		"path": t.TempDir(),
	}, &created)
	if resp.StatusCode != 201 {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}
	if created.ID == "" {
		t.Fatal("create: empty ID")
	}

	// List.
	var projects []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	resp = doJSON(t, "GET", ts.Server.URL+"/api/projects", nil, &projects)
	if resp.StatusCode != 200 {
		t.Fatalf("list: expected 200, got %d", resp.StatusCode)
	}
	if len(projects) != 1 {
		t.Fatalf("list: expected 1 project, got %d", len(projects))
	}
	if projects[0].ID != created.ID {
		t.Fatalf("list: expected ID %s, got %s", created.ID, projects[0].ID)
	}

	// Get by ID.
	var got struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	resp = doJSON(t, "GET", ts.Server.URL+"/api/projects/"+created.ID, nil, &got)
	if resp.StatusCode != 200 {
		t.Fatalf("get: expected 200, got %d", resp.StatusCode)
	}
	if got.Name != "test-project" {
		t.Fatalf("get: expected name 'test-project', got %q", got.Name)
	}

	// Update.
	var updated struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	resp = doJSON(t, "PUT", ts.Server.URL+"/api/projects/"+created.ID, map[string]string{
		"name": "renamed-project",
	}, &updated)
	if resp.StatusCode != 200 {
		t.Fatalf("update: expected 200, got %d", resp.StatusCode)
	}
	if updated.Name != "renamed-project" {
		t.Fatalf("update: expected name 'renamed-project', got %q", updated.Name)
	}

	// Verify update.
	resp = doJSON(t, "GET", ts.Server.URL+"/api/projects/"+created.ID, nil, &got)
	if resp.StatusCode != 200 {
		t.Fatalf("get after update: expected 200, got %d", resp.StatusCode)
	}
	if got.Name != "renamed-project" {
		t.Fatalf("get after update: expected 'renamed-project', got %q", got.Name)
	}

	// Delete.
	resp = doJSON(t, "DELETE", ts.Server.URL+"/api/projects/"+created.ID, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: expected 200, got %d", resp.StatusCode)
	}

	// Verify deleted.
	resp = doJSON(t, "GET", ts.Server.URL+"/api/projects", nil, &projects)
	if resp.StatusCode != 200 {
		t.Fatalf("list after delete: expected 200, got %d", resp.StatusCode)
	}
	if len(projects) != 0 {
		t.Fatalf("list after delete: expected 0 projects, got %d", len(projects))
	}
}

func TestIntegration_HelloWorld(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test (requires local LLM)")
	}
	skipIfOMLXUnavailable(t)

	ts := newTestServer(t)
	projectID := createTestProject(t, ts)
	sessionID := createTestSession(t, ts, projectID)

	response, stats := sendMessage(t, ts, sessionID, "Respond with exactly: hello world")

	if response == "" {
		t.Fatal("empty response")
	}
	if !strings.Contains(strings.ToLower(response), "hello") {
		t.Fatalf("expected response to contain 'hello', got: %q", response)
	}

	if stats.InputTokens == 0 {
		t.Error("expected non-zero InputTokens")
	}
	if stats.OutputTokens == 0 {
		t.Error("expected non-zero OutputTokens")
	}
	endSession(t, ts, sessionID)
}

func TestIntegration_SessionResume(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test (requires local LLM)")
	}
	skipIfOMLXUnavailable(t)

	ts := newTestServer(t)
	projectID := createTestProject(t, ts)
	sessionID := createTestSession(t, ts, projectID)

	// Send first message with a code word.
	response1, _ := sendMessage(t, ts, sessionID,
		"Remember this code word: pineapple42. Just confirm you've noted it.")
	if response1 == "" {
		t.Fatal("empty response to first message")
	}
	t.Logf("first response: %s", response1)

	// End session (persists ProviderState and message history).
	endSession(t, ts, sessionID)

	// Send follow-up message — session will be lazy-loaded from disk.
	response2, _ := sendMessage(t, ts, sessionID,
		"What was the code word I told you?")
	if response2 == "" {
		t.Fatal("empty response to resumed message")
	}
	t.Logf("resumed response: %s", response2)

	if !strings.Contains(strings.ToLower(response2), "pineapple42") {
		t.Fatalf("expected resumed response to contain 'pineapple42', got: %q", response2)
	}

	endSession(t, ts, sessionID)
}

func TestIntegration_MultipleMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test (requires local LLM)")
	}
	skipIfOMLXUnavailable(t)

	ts := newTestServer(t)
	projectID := createTestProject(t, ts)
	sessionID := createTestSession(t, ts, projectID)

	// First message.
	response1, stats1 := sendMessage(t, ts, sessionID,
		"What is 2+2? Reply with just the number.")
	if !strings.Contains(response1, "4") {
		t.Fatalf("expected response to contain '4', got: %q", response1)
	}
	t.Logf("first response: %s (tokens: in=%d out=%d cost=$%.6f)",
		response1, stats1.InputTokens, stats1.OutputTokens, stats1.CostUsd)

	// Second message — tests conversation context.
	response2, stats2 := sendMessage(t, ts, sessionID,
		"What was the math question I just asked? Repeat it.")
	lower := strings.ToLower(response2)
	if !strings.Contains(lower, "2+2") && !strings.Contains(lower, "2 + 2") &&
		!strings.Contains(lower, "two plus two") && !strings.Contains(lower, "2 plus 2") {
		t.Fatalf("expected response to reference '2+2', got: %q", response2)
	}
	t.Logf("second response: %s (tokens: in=%d out=%d cost=$%.6f)",
		response2, stats2.InputTokens, stats2.OutputTokens, stats2.CostUsd)

	// Stats should reflect cumulative totals from the second turn.
	if stats2.InputTokens == 0 || stats2.OutputTokens == 0 {
		t.Error("expected non-zero token counts on second message")
	}

	fmt.Printf("cumulative stats: in=%d out=%d cost=$%.6f\n",
		stats2.InputTokens, stats2.OutputTokens, stats2.CostUsd)

	endSession(t, ts, sessionID)
}
