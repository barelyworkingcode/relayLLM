//go:build llm

package main

// Live tests against a real local llama-server process. Validates exactly the
// surface fake-provider tests can't reach:
//
//   - SSE stream chunking and event ordering
//   - LlamaServerManager lifecycle (launch, port allocation, graceful stop)
//   - Real tool-call JSON quality (model emits valid function args)
//   - Mid-stream stop / abort
//
// Run with: go test -tags=llm ./...
//
// Requires `Qwen3.6 MoE 35` registered in
// `~/Library/Application Support/relayLLM/settings.json` under
// `llama-server.models` with the model file present at the configured
// modelDir path. Skips gracefully if absent.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const liveTestAlias = "Qwen3.6 MoE 35"

// Shared across all subtests so the model loads exactly once. nil if the
// model isn't installed — every subtest checks and t.Skip's in that case.
var liveLlama *LlamaServerManager

func TestMain(m *testing.M) {
	cfg := loadLiveLlamaConfig()
	if cfg != nil {
		liveLlama = NewLlamaServerManager(cfg, "")
	}
	code := m.Run()
	if liveLlama != nil {
		liveLlama.StopAll()
	}
	os.Exit(code)
}

// loadLiveLlamaConfig reads the user's relayLLM settings.json and returns the
// LlamaConfig if the required alias is present, else nil.
func loadLiveLlamaConfig() *LlamaConfig {
	settingsPath := liveSettingsPath()
	if settingsPath == "" {
		return nil
	}
	dataDir := filepath.Dir(settingsPath)
	_, llamaCfg, _, _, err := LoadConfig(dataDir, "")
	if err != nil || llamaCfg == nil {
		return nil
	}
	if llamaCfg.FindByAlias(liveTestAlias) == nil {
		return nil
	}
	return llamaCfg
}

// liveSettingsPath returns the path to the user's real relayLLM settings
// file, or "" if it's missing.
func liveSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	candidates := []string{
		filepath.Join(home, "Library/Application Support/relayLLM/settings.json"),
		filepath.Join(home, ".config/relayLLM/settings.json"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// requireLive skips the test if the live llama manager couldn't be loaded.
func requireLive(t *testing.T) {
	t.Helper()
	if liveLlama == nil {
		t.Skipf("requires %q in ~/Library/Application Support/relayLLM/settings.json", liveTestAlias)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle — the most under-tested code in the repo
// ---------------------------------------------------------------------------

func TestLlamaLive_GetOrLaunch_BindsHealthyPort(t *testing.T) {
	requireLive(t)

	endpoint, err := liveLlama.GetOrLaunch(liveTestAlias)
	if err != nil {
		t.Fatalf("GetOrLaunch: %v", err)
	}
	if endpoint == nil || endpoint.BaseURL == "" {
		t.Fatalf("endpoint missing baseURL: %+v", endpoint)
	}

	instances := liveLlama.ListInstances()
	found := false
	for _, inst := range instances {
		if inst.Alias == liveTestAlias {
			found = true
			if !inst.Healthy {
				t.Errorf("instance not healthy: %+v", inst)
			}
		}
	}
	if !found {
		t.Errorf("alias %q not in ListInstances: %+v", liveTestAlias, instances)
	}
}

func TestLlamaLive_GetOrLaunch_Idempotent(t *testing.T) {
	requireLive(t)

	e1, err := liveLlama.GetOrLaunch(liveTestAlias)
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	e2, err := liveLlama.GetOrLaunch(liveTestAlias)
	if err != nil {
		t.Fatalf("second launch: %v", err)
	}
	if e1.BaseURL != e2.BaseURL {
		t.Errorf("re-launch changed BaseURL: %q -> %q", e1.BaseURL, e2.BaseURL)
	}
}

// ---------------------------------------------------------------------------
// End-to-end through the full stack (TestServer + real llama session)
// ---------------------------------------------------------------------------

func TestLlamaLive_HTTPMessage_RoundTripsRealLLM(t *testing.T) {
	requireLive(t)

	// TestServer wires sessions + perms + HTTP routes against the real
	// LlamaServerManager. The only fake here is the bearer-auth glue.
	srv := newLiveTestServer(t)

	sessionID := srv.CreateSession(map[string]interface{}{
		"providerType": "llama",
		"model":        "llama/" + liveTestAlias,
		"directory":    srv.DataDir,
		"settings": json.RawMessage(`{"temperature": 0.0, "top_k": 1}`),
	})

	var resp struct {
		Response string       `json:"response"`
		Stats    SessionStats `json:"stats"`
	}
	httpResp := srv.PostJSON("/api/sessions/"+sessionID+"/message",
		map[string]interface{}{"text": "Reply with exactly the single word: pong"},
		&resp)
	if httpResp.StatusCode != 200 {
		t.Fatalf("status: got %d, body=%s", httpResp.StatusCode, resp.Response)
	}
	if strings.TrimSpace(resp.Response) == "" {
		t.Fatal("empty response")
	}
	if !strings.Contains(strings.ToLower(resp.Response), "pong") {
		t.Errorf("expected 'pong' in response; got %q", resp.Response)
	}
	if resp.Stats.InputTokens == 0 || resp.Stats.OutputTokens == 0 {
		t.Errorf("expected non-zero token counts; got %+v", resp.Stats)
	}
}

// ---------------------------------------------------------------------------
// Tool call round-trip with FakeMCP
// ---------------------------------------------------------------------------

func TestLlamaLive_ToolCall_RoundTripsWithFakeMCP(t *testing.T) {
	requireLive(t)
	t.Skip("TODO: wiring a FakeMCP into a session through the public API requires either an mcpServers setting that routes to a stub binary, or a SessionManager seam to inject the MCP client directly. The chat_base provider builds MCP from session.Settings, so the cleanest path is to add a test-only `mcpClientFactory` hook on SessionManager. Deferred — non-LLM tool_loop_test covers the logic; this test would only validate that the model emits a syntactically correct tool_call JSON.")
}

// ---------------------------------------------------------------------------
// Mid-stream stop
// ---------------------------------------------------------------------------

func TestLlamaLive_StopMidGeneration_CleanlyAborts(t *testing.T) {
	requireLive(t)

	srv := newLiveTestServer(t)
	sessionID := srv.CreateSession(map[string]interface{}{
		"providerType": "llama",
		"model":        "llama/" + liveTestAlias,
		"directory":    srv.DataDir,
		"settings":     json.RawMessage(`{"temperature": 0.7}`),
	})

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "join_session", "sessionId": sessionID})
	ReadUntilType(t, conn, "session_joined", 2*time.Second)

	WSSend(t, conn, map[string]interface{}{
		"type": "send_message", "sessionId": sessionID,
		"text": "Write a 500-word essay about distributed systems consensus algorithms.",
	})

	// Wait for at least one llm_event so we know the stream is actively emitting.
	ReadUntilType(t, conn, HandlerLLMEvent, 10*time.Second)

	// Issue stop.
	WSSend(t, conn, map[string]interface{}{"type": "stop_generation", "sessionId": sessionID})

	// message_complete arrives within a reasonable window after stop.
	ReadUntilType(t, conn, HandlerMessageComplete, 5*time.Second)

	// Lifecycle sanity: after stop, the same model is still healthy.
	instances := liveLlama.ListInstances()
	for _, inst := range instances {
		if inst.Alias == liveTestAlias && !inst.Healthy {
			t.Errorf("server unhealthy after stop_generation: %+v", inst)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newLiveTestServer wires a TestServer with the real LlamaServerManager so
// "llama/{alias}" sessions route to a real llama-server process.
func newLiveTestServer(t *testing.T) *TestServer {
	t.Helper()
	srv := NewTestServer(t, nil)
	srv.Sessions.SetLlamaManager(liveLlama)
	return srv
}
