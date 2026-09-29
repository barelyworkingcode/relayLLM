package router

// Regression guard for the Anthropic modelMap redirect (translation) path:
// with no "api" capability declared on the target, the OpenAI request the
// backend receives and the Anthropic response the client receives must stay
// byte-identical to the goldens under testdata/anthropic_native/regression/,
// which were generated on main before the native pass-through existed.
//
// Regenerate (only when a translation change is intended):
//
//	go test ./internal/router -run TestAnthropicRegression -update-anthropic-regression

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"relayllm/internal/config"
	regpkg "relayllm/internal/registry"
	"relayllm/internal/servermanager"
	"sync"
	"testing"
)

var updateAnthropicGolden = flag.Bool("update-anthropic-regression", false, "rewrite the Anthropic translation-path regression goldens")

// Only the ids the router mints itself with crypto/rand are normalized;
// every other byte is compared as-is.
var anthropicGeneratedID = regexp.MustCompile(`\b(msg_[0-9a-f]{24}|toolu_[0-9a-f]{12})\b`)

func normalizeAnthropicIDs(b []byte) []byte {
	return anthropicGeneratedID.ReplaceAllFunc(b, func(m []byte) []byte {
		if bytes.HasPrefix(m, []byte("msg_")) {
			return []byte("msg_GENERATED")
		}
		return []byte("toolu_GENERATED")
	})
}

// A Claude Code-shaped request: array system with cache_control, thinking
// disabled, metadata.user_id carrying a session id, an unknown top-level
// field, and HTML-sensitive characters (to catch an escaping change).
const anthropicRegressionTextReq = `{"model":"%s","max_tokens":1024,"temperature":0.2,"stream":%t,` +
	`"system":[{"type":"text","text":"You are a helper. Use <tags> & quotes \"sparingly\".","cache_control":{"type":"ephemeral"}}],` +
	`"thinking":{"type":"disabled"},` +
	`"metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"s-123\"}"},` +
	`"context_management":{"edits":[]},` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"Say hi to <Acme> & friends.","cache_control":{"type":"ephemeral"}}]}]}`

// Tool-use history: an assistant turn with a signed thinking block, text and
// a tool_use, the matching tool_result, then a new user turn.
const anthropicRegressionToolReq = `{"model":"%s","max_tokens":2048,"stream":%t,` +
	`"system":"Project assistant.",` +
	`"tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],` +
	`"tool_choice":{"type":"auto"},` +
	`"stop_sequences":["</done>"],` +
	`"messages":[` +
	`{"role":"user","content":"Open the notes file."},` +
	`{"role":"assistant","content":[{"type":"thinking","thinking":"Need to read it.","signature":"sig-abc"},{"type":"text","text":"Reading."},{"type":"tool_use","id":"toolu_hist01","name":"Read","input":{"path":"notes.txt"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_hist01","content":[{"type":"text","text":"line one\nline two"}]}]},` +
	`{"role":"user","content":"Now read the todo file too."}]}`

var anthropicRegressionTextSSE = []string{
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hi <Acme> "},"finish_reason":null}]}`,
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"& friends é!"},"finish_reason":null}]}`,
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":6,"total_tokens":48,"prompt_tokens_details":{"cached_tokens":30}}}`,
	`data: [DONE]`,
}

var anthropicRegressionToolSSE = []string{
	`data: {"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Checking."},"finish_reason":null}]}`,
	`data: {"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_r1","type":"function","function":{"name":"Read","arguments":""}}]},"finish_reason":null}]}`,
	`data: {"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
	`data: {"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"todo.txt\"}"}}]},"finish_reason":null}]}`,
	`data: {"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	`data: {"id":"c2","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":14,"total_tokens":134}}`,
	`data: [DONE]`,
}

// regressionUpstream is one fake OpenAI backend serving both the endpoint
// and the managed alias. It records the request line and body of every
// non-/models request and answers with the current canned SSE chunks.
type regressionUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reply []string
	seen  bytes.Buffer
}

func newRegressionUpstream(t *testing.T) *regressionUpstream {
	t.Helper()
	u := &regressionUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		fmt.Fprintf(&u.seen, "%s %s\n\n%s\n", r.Method, r.URL.Path, body)
		reply := u.reply
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range reply {
			io.WriteString(w, chunk+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *regressionUpstream) arm(reply []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reply = reply
	u.seen.Reset()
}

func (u *regressionUpstream) recorded() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]byte(nil), u.seen.Bytes()...)
}

func TestAnthropicRegression_TranslatedPathBytesUnchanged(t *testing.T) {
	up := newRegressionUpstream(t)

	// Endpoint target, no "api" field.
	ep := config.OpenAIEndpoint{Name: "ep", BaseURL: up.srv.URL + "/v1"}
	reg := regpkg.NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{ep}})
	reg.SetStatusForTest(ep, true, regpkg.UpstreamModel{ID: "upstream-model"})

	// Managed-alias target, no "api" field, backed by the same fake.
	mgr := servermanager.NewServerManager(servermanager.LlamaProfile, &config.ServerConfig{
		Models: []config.ServerModelConfig{{Alias: "local-model", Args: map[string]any{"model": "/fake"}}},
	}, "")
	mgr.InjectReadyInstanceForTest("local-model", up.srv.Listener.Addr().(*net.TCPAddr).Port, 0)

	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid", map[string]string{
		"claude-haiku-4-5":  "ep/upstream-model",
		"claude-sonnet-4-5": "local-model",
	}), []*servermanager.ServerManager{mgr}, reg, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	cases := []struct {
		name  string
		req   string
		reply []string
	}{
		{"endpoint_stream_text", fmt.Sprintf(anthropicRegressionTextReq, "claude-haiku-4-5", true), anthropicRegressionTextSSE},
		{"endpoint_nonstream_text", fmt.Sprintf(anthropicRegressionTextReq, "claude-haiku-4-5", false), anthropicRegressionTextSSE},
		{"endpoint_stream_tool_call", fmt.Sprintf(anthropicRegressionToolReq, "claude-haiku-4-5", true), anthropicRegressionToolSSE},
		{"managed_stream_text", fmt.Sprintf(anthropicRegressionTextReq, "claude-sonnet-4-5", true), anthropicRegressionTextSSE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up.arm(tc.reply)

			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader([]byte(tc.req)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("anthropic-version", "2023-06-01")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			client := normalizeAnthropicIDs([]byte(fmt.Sprintf("HTTP %d\n\n%s", resp.StatusCode, body)))
			upstream := normalizeAnthropicIDs(up.recorded())

			checkAnthropicGolden(t, tc.name+".upstream.golden", upstream)
			checkAnthropicGolden(t, tc.name+".client.golden", client)
		})
	}
}

func checkAnthropicGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "anthropic_native", "regression", name)
	if *updateAnthropicGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}
