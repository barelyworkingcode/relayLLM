//go:build llm

package main

// Live tests against a real `claude` CLI binary. Uses Haiku to keep cost
// down. Validates the persistent-process lifecycle and session resume that
// unit tests can't exercise.
//
// Run: go test -tags=llm -run TestClaudeLive ./...
//
// Requires:
//   - `claude` on PATH (Claude Code CLI ≥ 2.x)
//   - Anthropic auth (ANTHROPIC_API_KEY env or `claude auth login` cache)

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// requireClaude skips if the `claude` binary isn't reachable.
func requireClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("requires `claude` CLI on PATH (Claude Code)")
	}
}

func newClaudeLiveServer(t *testing.T) *TestServer {
	t.Helper()
	requireClaude(t)
	srv := NewTestServer(t, nil)
	return srv
}

// ---------------------------------------------------------------------------
// Text round-trip via Haiku
// ---------------------------------------------------------------------------

func TestClaudeLive_HTTPMessage_RoundTripsThroughCLI(t *testing.T) {
	srv := newClaudeLiveServer(t)

	// Headless skips permission prompts so the CLI doesn't block on hook
	// roundtrips for this test. The hook integration path has its own
	// dedicated test (TODO — see ROADMAP).
	body := map[string]interface{}{
		"providerType":   "claude",
		"model":          "haiku",
		"directory":      srv.DataDir,
		"systemPrompt":   "Reply with exactly one word and nothing else.",
		"appendClaudeMd": false,
		"settings":       json.RawMessage(`{"headless": true}`),
	}
	// `headless` is not in BaseChatSettings; set it directly on the session
	// after creation since CreateSession doesn't accept it.
	sessionID := srv.CreateSession(body)
	sess, _ := srv.Sessions.GetSession(sessionID)
	sess.mu.Lock()
	sess.Headless = true
	sess.mu.Unlock()

	var resp struct {
		Response string       `json:"response"`
		Stats    SessionStats `json:"stats"`
	}
	httpResp := srv.PostJSON("/api/sessions/"+sessionID+"/message",
		map[string]interface{}{"text": "Reply with exactly: pong"},
		&resp)
	if httpResp.StatusCode != 200 {
		t.Fatalf("status: got %d body=%s", httpResp.StatusCode, resp.Response)
	}
	if strings.TrimSpace(resp.Response) == "" {
		t.Fatal("empty response from claude")
	}
	if !strings.Contains(strings.ToLower(resp.Response), "pong") {
		t.Errorf("expected 'pong' somewhere in response; got %q", resp.Response)
	}
	if resp.Stats.OutputTokens == 0 {
		t.Errorf("expected non-zero OutputTokens; got %+v", resp.Stats)
	}
}

// ---------------------------------------------------------------------------
// Session resume — claudeSessionID survives EndSession → reload → SendMessage
// ---------------------------------------------------------------------------

func TestClaudeLive_SessionResume_RemembersContext(t *testing.T) {
	srv := newClaudeLiveServer(t)

	sessionID := srv.CreateSession(map[string]interface{}{
		"providerType":   "claude",
		"model":          "haiku",
		"directory":      srv.DataDir,
		"appendClaudeMd": false,
	})
	sess, _ := srv.Sessions.GetSession(sessionID)
	sess.mu.Lock()
	sess.Headless = true
	sess.mu.Unlock()

	// Turn 1: plant a code word.
	var first struct {
		Response string `json:"response"`
	}
	srv.PostJSON("/api/sessions/"+sessionID+"/message",
		map[string]interface{}{"text": "Remember this code word for later: pineapple42. Reply with only 'ok'."},
		&first)
	if first.Response == "" {
		t.Fatal("empty response to first turn")
	}
	t.Logf("first response: %s", first.Response)

	// End the session — process dies, ProviderState persists to disk
	// (includes claudeSessionID so --resume works on reload).
	srv.DeleteJSON("/api/sessions/"+sessionID, nil)

	// Turn 2: GetSession lazy-loads from disk; SendMessageSync's path will
	// re-init the provider, which sees a non-empty claudeSessionID and
	// passes --resume to the new claude subprocess.
	var second struct {
		Response string `json:"response"`
	}
	httpResp := srv.PostJSON("/api/sessions/"+sessionID+"/message",
		map[string]interface{}{"text": "What was the code word I gave you? Reply with just the word."},
		&second)
	if httpResp.StatusCode != 200 {
		t.Fatalf("resumed status: got %d body=%s", httpResp.StatusCode, second.Response)
	}
	if !strings.Contains(strings.ToLower(second.Response), "pineapple42") {
		t.Errorf("resumed session did not remember code word; got %q", second.Response)
	}
}
