package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An OpenAI backend can refuse a request by sending an error frame inside a
// 200 SSE stream (`data: {"error": {...}}`). The translated path must answer
// with an Anthropic error, never an empty end_turn.
func sseErrorUpstream(t *testing.T, msg string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"error\": {\"message\": \""+msg+"\"}}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

const overflowMsg = "Job requires 513 pages (only 512 available) and cannot be enqueued. Total cache allocated is 512 * 256 = 131072 tokens"

func TestAnthropic_Redirect_SSEErrorFrame_Streaming_IsPromptTooLong(t *testing.T) {
	upstream := sseErrorUpstream(t, overflowMsg)
	r := anthropicRedirectFixture(t, "local-model", upstream, map[string]string{"claude-haiku-4-5": "local-model"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages",
		[]byte(`{"model":"claude-haiku-4-5","stream":true,"max_tokens":128000,"messages":[{"role":"user","content":"hi"}]}`))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.StatusCode, body)
	}
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body not JSON: %v: %s", err, body)
	}
	msg, _ := env["error"].(map[string]any)["message"].(string)
	if !strings.HasPrefix(msg, "prompt is too long:") {
		t.Errorf("message = %q, want prefix %q", msg, "prompt is too long:")
	}
}

func TestAnthropic_Redirect_SSEErrorFrame_NonStreaming_IsPromptTooLong(t *testing.T) {
	upstream := sseErrorUpstream(t, overflowMsg)
	r := anthropicRedirectFixture(t, "local-model", upstream, map[string]string{"claude-haiku-4-5": "local-model"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages",
		[]byte(`{"model":"claude-haiku-4-5","max_tokens":128000,"messages":[{"role":"user","content":"hi"}]}`))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "prompt is too long:") {
		t.Errorf("body = %s, want prompt is too long", body)
	}
}

func TestAnthropic_Redirect_SSEErrorFrame_NonOverflow_IsAPIError(t *testing.T) {
	upstream := sseErrorUpstream(t, "model crashed")
	r := anthropicRedirectFixture(t, "local-model", upstream, map[string]string{"claude-haiku-4-5": "local-model"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages",
		[]byte(`{"model":"claude-haiku-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want an error; body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "model crashed") {
		t.Errorf("body = %s, want upstream message", body)
	}
}
