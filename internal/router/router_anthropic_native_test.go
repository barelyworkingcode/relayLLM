package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"relayllm/internal/config"
	regpkg "relayllm/internal/registry"
	"relayllm/internal/servermanager"
)

// nativeUpstream is a fake Anthropic-speaking backend. It records every
// request and answers with a canned status, headers and body, written in
// small flushed chunks so the router's streaming path is exercised.
type nativeUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   []nativeHit
	status int
	header map[string]string
	body   string
}

type nativeHit struct {
	path   string
	header http.Header
	body   []byte
}

func newNativeUpstream(t *testing.T, status int, header map[string]string, body string) *nativeUpstream {
	t.Helper()
	u := &nativeUpstream{status: status, header: header, body: body}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hits = append(u.hits, nativeHit{path: r.URL.Path, header: r.Header.Clone(), body: b})
		u.mu.Unlock()
		for k, v := range u.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(u.status)
		for rest := u.body; rest != ""; {
			n := min(5, len(rest))
			io.WriteString(w, rest[:n])
			w.(http.Flusher).Flush()
			rest = rest[n:]
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *nativeUpstream) recorded() []nativeHit {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]nativeHit(nil), u.hits...)
}

// nativeRegistry marks every endpoint online, advertising "backend-model".
func nativeRegistry(eps ...config.OpenAIEndpoint) *regpkg.ProxyRegistry {
	reg := regpkg.NewProxyRegistry(&config.OpenAIConfig{Endpoints: eps})
	for _, ep := range eps {
		reg.SetStatusForTest(ep, true, regpkg.UpstreamModel{ID: "backend-model"})
	}
	return reg
}

func sendAnthropic(t *testing.T, url, body string, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, b
}

var anthropicAPI = []string{config.APIAnthropic}

// A Claude Code-shaped request: cache_control on system, tools and messages,
// signed thinking and redacted_thinking history, a nested "model" key, unknown
// top-level and nested fields, odd whitespace, \u escapes and <>&.
const nativeClaudeCodeReq = `{
  "model" :  "claude-haiku-4-5",
	"max_tokens":32000,"stream":true,
  "system":[{"type":"text","text":"You are Claude Code, Acme's CLI. Use <tags> & \"quotes\".","cache_control":{"type":"ephemeral","ttl":"1h"}}],
  "thinking":{"type":"enabled","budget_tokens":31999},
  "metadata":{"user_id":"{\"session_id\":\"s-1\"}","model":"claude-haiku-4-5"},
  "tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}},"cache_control":{"type":"ephemeral"}}],
  "messages":[
    {"role":"user","content":[{"type":"text","text":"Open notes é  <b>","cache_control":{"type":"ephemeral"}}]},
    {"role":"assistant","content":[
      {"type":"thinking","thinking":"Need the file.","signature":"EqQBCkYIBxgCKkB+/sig=="},
      {"type":"redacted_thinking","data":"EmwKAhgBEgy3opaque=="},
      {"type":"tool_use","id":"toolu_01","name":"Read","input":{"path":"notes.txt"},"x_nested_unknown":{"k":[1,2.50,null]}}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"line one","cache_control":{"type":"ephemeral"}}]}
  ],
  "context_management":{"edits":[{"type":"clear_thinking_20251015"}]},
  "x_future_field": 1.0e3
}`

const nativeModelNeedle = `"model" :  "claude-haiku-4-5"`

// jsonDiffPaths returns every path at which a and b differ.
func jsonDiffPaths(path string, a, b any) []string {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		var out []string
		for k := range am {
			out = append(out, jsonDiffPaths(strings.TrimPrefix(path+"."+k, "."), am[k], bm[k])...)
		}
		for k := range bm {
			if _, ok := am[k]; !ok {
				out = append(out, strings.TrimPrefix(path+"."+k, "."))
			}
		}
		sort.Strings(out)
		return out
	}
	aa, aok := a.([]any)
	ba, bok := b.([]any)
	if aok && bok && len(aa) == len(ba) {
		var out []string
		for i := range aa {
			out = append(out, jsonDiffPaths(fmt.Sprintf("%s[%d]", path, i), aa[i], ba[i])...)
		}
		return out
	}
	if !reflect.DeepEqual(a, b) {
		return []string{path}
	}
	return nil
}

func TestAnthropicNative_RequestForwardedWithOnlyModelAndCredentialsChanged(t *testing.T) {
	up := newNativeUpstream(t, 200, map[string]string{"Content-Type": "application/json"}, `{"type":"message","content":[]}`)
	keyed := config.OpenAIEndpoint{Name: "acme", BaseURL: up.srv.URL + "/acme/v1", APIKey: "ep-key", API: anthropicAPI}
	keyless := config.OpenAIEndpoint{Name: "open", BaseURL: up.srv.URL + "/open/v1", API: anthropicAPI}
	mgr := servermanager.NewServerManager(servermanager.LlamaProfile, &config.ServerConfig{
		API:    anthropicAPI,
		Models: []config.ServerModelConfig{{Alias: "local-model"}},
	}, "")
	injectHealthyManagedInstance(t, mgr, "local-model", up.srv)

	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid", map[string]string{
		"claude-haiku-4-5": "acme/backend-model",
		"claude-keyless":   "open/backend-model",
		"claude-local":     "local-model",
	}), []*servermanager.ServerManager{mgr}, nativeRegistry(keyed, keyless), nil)
	tcp := httptest.NewServer(r.server.Handler)
	defer tcp.Close()
	sock := httptest.NewServer(r.SocketHandler())
	defer sock.Close()

	if strings.Count(nativeClaudeCodeReq, nativeModelNeedle) != 1 {
		t.Fatal("fixture must carry the top-level model needle exactly once")
	}
	clientHeaders := map[string]string{
		"Authorization":      "Bearer client-token",
		"X-Api-Key":          "client-key",
		"X-Relay-Router-Key": "rk-client",
		"X-Relay-Service":    "spoof",
		"anthropic-version":  "2023-06-01",
		"anthropic-beta":     "interleaved-thinking-2025-05-14,prompt-caching-2024-07-31",
	}

	cases := []struct {
		name, server, key, wantPath, wantModel, wantKey string
	}{
		{"endpoint with apiKey over tcp", tcp.URL, "claude-haiku-4-5", "/acme/v1/messages", "backend-model", "ep-key"},
		{"endpoint with apiKey over router.sock handler", sock.URL, "claude-haiku-4-5", "/acme/v1/messages", "backend-model", "ep-key"},
		{"endpoint without apiKey", tcp.URL, "claude-keyless", "/open/v1/messages", "backend-model", ""},
		{"managed alias", tcp.URL, "claude-local", "/v1/messages", "local-model", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(up.recorded())
			orig := strings.Replace(nativeClaudeCodeReq, "claude-haiku-4-5", tc.key, 1)
			resp, body := sendAnthropic(t, tc.server+"/v1/messages", orig, clientHeaders)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			hits := up.recorded()[before:]
			if len(hits) != 1 {
				t.Fatalf("upstream hits = %d, want 1", len(hits))
			}
			hit := hits[0]
			if hit.path != tc.wantPath {
				t.Errorf("upstream path = %q, want %q", hit.path, tc.wantPath)
			}

			// Bytes: the original with only the model value spliced.
			want := strings.Replace(nativeClaudeCodeReq, nativeModelNeedle, `"model" :  "`+tc.wantModel+`"`, 1)
			if string(hit.body) != want {
				t.Errorf("upstream body differs from the spliced original\n--- got ---\n%s\n--- want ---\n%s", hit.body, want)
			}
			// Structure: a generic diff allows exactly the model path.
			var a, b any
			if err := json.Unmarshal([]byte(orig), &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(hit.body, &b); err != nil {
				t.Fatalf("upstream body is not JSON: %v", err)
			}
			if diff := jsonDiffPaths("", a, b); !reflect.DeepEqual(diff, []string{"model"}) {
				t.Errorf("JSON diff paths = %v, want [model]", diff)
			}

			wantAuth := ""
			if tc.wantKey != "" {
				wantAuth = "Bearer " + tc.wantKey
			}
			if got := hit.header.Get("Authorization"); got != wantAuth {
				t.Errorf("Authorization = %q, want %q", got, wantAuth)
			}
			if got := hit.header.Get("X-Api-Key"); got != tc.wantKey {
				t.Errorf("X-Api-Key = %q, want %q", got, tc.wantKey)
			}
			for k := range hit.header {
				if strings.HasPrefix(strings.ToLower(k), "x-relay-") {
					t.Errorf("upstream received %s", k)
				}
			}
			for _, h := range []string{"anthropic-version", "anthropic-beta"} {
				if got := hit.header.Get(h); got != clientHeaders[h] {
					t.Errorf("%s = %q, want %q", h, got, clientHeaders[h])
				}
			}
		})
	}
}

func TestAnthropicNative_ResponsePassesThroughUnmodified(t *testing.T) {
	cases := []struct {
		name, contentType, body string
	}{
		{"sse", "text/event-stream; charset=utf-8", tapSSEStream},
		{"json", "application/json", tapJSONBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newNativeUpstream(t, 200, map[string]string{
				"Content-Type":         tc.contentType,
				"Request-Id":           "req_acme1",
				"X-Relay-Model-Target": "spoofed",
				"X-Relay-Leak":         "1",
			}, tc.body)
			ep := config.OpenAIEndpoint{Name: "acme", BaseURL: up.srv.URL + "/v1", API: anthropicAPI}
			r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid",
				map[string]string{"claude-haiku-4-5": "acme/backend-model"}), nil, nativeRegistry(ep), nil)
			srv := httptest.NewServer(r.server.Handler)
			defer srv.Close()

			resp, body := sendAnthropic(t, srv.URL+"/v1/messages",
				`{"model":"claude-haiku-4-5","max_tokens":64,"stream":`+fmt.Sprint(tc.name == "sse")+`,"messages":[{"role":"user","content":"hi"}]}`, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			if string(body) != tc.body {
				t.Errorf("client body differs from upstream\n--- got ---\n%s\n--- want ---\n%s", body, tc.body)
			}
			if got := resp.Header.Get("Content-Type"); got != tc.contentType {
				t.Errorf("Content-Type = %q, want %q", got, tc.contentType)
			}
			if got := resp.Header.Get("Request-Id"); got != "req_acme1" {
				t.Errorf("Request-Id = %q, want upstream's", got)
			}
			if got := resp.Header.Get("X-Relay-Model-Target"); got != "acme/backend-model" {
				t.Errorf("X-Relay-Model-Target = %q, want acme/backend-model", got)
			}
			if got := resp.Header.Get("X-Relay-Leak"); got != "" {
				t.Errorf("upstream X-Relay-Leak reached the client: %q", got)
			}
		})
	}
}

// Criterion 6: a declared ["openai"] upstream translates exactly as the
// criterion 1 goldens (generated on main) record.
func TestAnthropicNative_OpenAIOnlyDeclarationStillTranslates(t *testing.T) {
	up := newRegressionUpstream(t)
	openaiOnly := []string{config.APIOpenAI}
	ep := config.OpenAIEndpoint{Name: "ep", BaseURL: up.srv.URL + "/v1", API: openaiOnly}
	reg := regpkg.NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{ep}})
	reg.SetStatusForTest(ep, true, regpkg.UpstreamModel{ID: "upstream-model"})
	mgr := servermanager.NewServerManager(servermanager.LlamaProfile, &config.ServerConfig{
		API:    openaiOnly,
		Models: []config.ServerModelConfig{{Alias: "local-model", Args: map[string]any{"model": "/fake"}}},
	}, "")
	injectHealthyManagedInstance(t, mgr, "local-model", up.srv)

	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid", map[string]string{
		"claude-haiku-4-5":  "ep/upstream-model",
		"claude-sonnet-4-5": "local-model",
	}), []*servermanager.ServerManager{mgr}, reg, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	cases := []struct {
		golden, req string
		reply       []string
	}{
		{"endpoint_stream_text", fmt.Sprintf(anthropicRegressionTextReq, "claude-haiku-4-5", true), anthropicRegressionTextSSE},
		{"endpoint_stream_tool_call", fmt.Sprintf(anthropicRegressionToolReq, "claude-haiku-4-5", true), anthropicRegressionToolSSE},
		{"managed_stream_text", fmt.Sprintf(anthropicRegressionTextReq, "claude-sonnet-4-5", true), anthropicRegressionTextSSE},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			up.arm(tc.reply)
			resp, body := sendAnthropic(t, srv.URL+"/v1/messages", tc.req, map[string]string{"anthropic-version": "2023-06-01"})
			client := normalizeAnthropicIDs([]byte(fmt.Sprintf("HTTP %d\n\n%s", resp.StatusCode, body)))
			checkAnthropicGolden(t, tc.golden+".upstream.golden", normalizeAnthropicIDs(up.recorded()))
			checkAnthropicGolden(t, tc.golden+".client.golden", client)
		})
	}
}

func TestAnthropicNative_AllNativeVirtualFailsOverNatively(t *testing.T) {
	up := newNativeUpstream(t, 200, map[string]string{"Content-Type": "text/event-stream"}, tapSSEStream)
	dead := config.OpenAIEndpoint{Name: "dead", BaseURL: "http://127.0.0.1:1/v1", API: anthropicAPI}
	live := config.OpenAIEndpoint{Name: "live", BaseURL: up.srv.URL + "/live/v1", API: anthropicAPI}
	virtual := &config.VirtualLLMConfig{Models: []config.VirtualLLM{{Name: "vNative", Targets: []config.VirtualLLMTarget{
		{Endpoint: "dead", Model: "backend-model"}, {Endpoint: "live", Model: "backend-model"},
	}}}}
	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid",
		map[string]string{"claude-haiku-4-5": "vNative"}), nil, nativeRegistry(dead, live), virtual)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp, body := sendAnthropic(t, srv.URL+"/v1/messages", nativeClaudeCodeReq, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if string(body) != tapSSEStream {
		t.Errorf("client body differs from the live candidate's stream:\n%s", body)
	}
	if got := resp.Header.Get("X-Relay-Model-Target"); got != "live/backend-model" {
		t.Errorf("X-Relay-Model-Target = %q, want live/backend-model", got)
	}
	hits := up.recorded()
	if len(hits) != 1 || hits[0].path != "/live/v1/messages" {
		t.Fatalf("upstream hits = %+v, want one on /live/v1/messages", hits)
	}
	if want := strings.Replace(nativeClaudeCodeReq, nativeModelNeedle, `"model" :  "backend-model"`, 1); string(hits[0].body) != want {
		t.Errorf("failover candidate got a non-spliced body:\n%s", hits[0].body)
	}
}

func TestAnthropicNative_MixedVirtualTranslates(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range anthropicRegressionTextSSE {
			io.WriteString(w, chunk+"\n\n")
		}
	}))
	defer up.Close()
	nat := config.OpenAIEndpoint{Name: "nat", BaseURL: up.URL + "/nat/v1", API: anthropicAPI}
	oai := config.OpenAIEndpoint{Name: "oai", BaseURL: up.URL + "/oai/v1"}
	virtual := &config.VirtualLLMConfig{Models: []config.VirtualLLM{{Name: "vMixed", Targets: []config.VirtualLLMTarget{
		{Endpoint: "nat", Model: "backend-model"}, {Endpoint: "oai", Model: "backend-model"},
	}}}}
	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid",
		map[string]string{"claude-haiku-4-5": "vMixed"}), nil, nativeRegistry(nat, oai), virtual)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp, body := sendAnthropic(t, srv.URL+"/v1/messages", fmt.Sprintf(anthropicRegressionTextReq, "claude-haiku-4-5", true), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(paths, []string{"/nat/v1/chat/completions"}) {
		t.Errorf("upstream paths = %v, want the first candidate reached in OpenAI shape only", paths)
	}
}

func TestAnthropicNative_ErrorResponses(t *testing.T) {
	const overflowMsg = "request (40000 tokens) exceeds the available context size (32768 tokens)"
	cases := []struct {
		name, body string
		status     int
		wantMsg    string // "" = client must receive the upstream bytes untouched
	}{
		{"context overflow reworded", `{"type":"error","error":{"type":"invalid_request_error","message":"` + overflowMsg + `"}}`, 400, "prompt is too long: " + overflowMsg},
		{"other 400 untouched", `{"type":"error", "error":{"type":"invalid_request_error","message":"messages: Field required <x>"}}`, 400, ""},
		{"401 untouched", `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, 401, ""},
		{"429 untouched", "{\"type\":\"error\",\n\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}", 429, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newNativeUpstream(t, tc.status, map[string]string{"Content-Type": "application/json"}, tc.body)
			ep := config.OpenAIEndpoint{Name: "acme", BaseURL: up.srv.URL + "/v1", API: anthropicAPI}
			r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid",
				map[string]string{"claude-haiku-4-5": "acme/backend-model"}), nil, nativeRegistry(ep), nil)
			srv := httptest.NewServer(r.server.Handler)
			defer srv.Close()

			resp, body := sendAnthropic(t, srv.URL+"/v1/messages", `{"model":"claude-haiku-4-5","max_tokens":8,"messages":[]}`, nil)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, tc.status, body)
			}
			if tc.wantMsg == "" {
				if string(body) != tc.body {
					t.Errorf("body = %s, want upstream bytes %s", body, tc.body)
				}
				return
			}
			var env struct {
				Type  string `json:"type"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &env); err != nil || env.Type != "error" || env.Error.Message != tc.wantMsg {
				t.Errorf("body = %s, want Anthropic error with message %q", body, tc.wantMsg)
			}
		})
	}
}

func TestAnthropicNative_CountTokens(t *testing.T) {
	const req = `{"model": "claude-haiku-4-5", "messages":[{"role":"user","content":"count <me> é"}]}`
	cases := []struct {
		name      string
		socket    bool
		virtual   bool
		status    int
		forwarded bool // client gets the upstream's answer, not the estimate
	}{
		{"native endpoint forwarded", false, false, 200, true},
		{"native endpoint forwarded via router.sock handler", true, false, 200, true},
		{"upstream 404 falls back to estimate", false, false, 404, false},
		{"upstream 405 falls back to estimate", false, false, 405, false},
		{"virtual keeps the estimate", false, true, 200, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upBody := `{"input_tokens": 4321}`
			if tc.status != 200 {
				upBody = `{"error":"no such route"}`
			}
			up := newNativeUpstream(t, tc.status, map[string]string{"Content-Type": "application/json"}, upBody)
			ep := config.OpenAIEndpoint{Name: "acme", BaseURL: up.srv.URL + "/v1", APIKey: "ep-key", API: anthropicAPI}
			target := "acme/backend-model"
			var virtual *config.VirtualLLMConfig
			if tc.virtual {
				target = "vNative"
				virtual = &config.VirtualLLMConfig{Models: []config.VirtualLLM{{Name: "vNative",
					Targets: []config.VirtualLLMTarget{{Endpoint: "acme", Model: "backend-model"}}}}}
			}
			r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid",
				map[string]string{"claude-haiku-4-5": target}), nil, nativeRegistry(ep), virtual)
			var h http.Handler = r.server.Handler
			if tc.socket {
				h = r.SocketHandler()
			}
			srv := httptest.NewServer(h)
			defer srv.Close()

			resp, body := sendAnthropic(t, srv.URL+"/v1/messages/count_tokens", req, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			hits := up.recorded()
			if tc.virtual {
				if len(hits) != 0 {
					t.Errorf("virtual count_tokens reached the upstream: %+v", hits)
				}
			} else {
				if len(hits) != 1 || hits[0].path != "/v1/messages/count_tokens" {
					t.Fatalf("upstream hits = %+v, want one on /v1/messages/count_tokens", hits)
				}
				if want := strings.Replace(req, `"claude-haiku-4-5"`, `"backend-model"`, 1); string(hits[0].body) != want {
					t.Errorf("upstream body = %s, want %s", hits[0].body, want)
				}
			}
			if tc.forwarded {
				if string(body) != upBody {
					t.Errorf("client body = %s, want upstream's %s", body, upBody)
				}
				return
			}
			var est struct {
				InputTokens int64 `json:"input_tokens"`
			}
			if err := json.Unmarshal(body, &est); err != nil || est.InputTokens <= 0 || bytes.Contains(body, []byte("4321")) {
				t.Errorf("body = %s, want the local estimate {input_tokens>0}", body)
			}
		})
	}
}
