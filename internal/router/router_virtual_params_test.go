package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"relayllm/internal/config"
	regpkg "relayllm/internal/registry"
)

const worked = `{"chat_template_kwargs":{"enable_thinking":false},"reasoning_effort":"medium"}`

type paramsUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func (u *paramsUpstream) last(t *testing.T) []byte {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) != 1 {
		t.Fatalf("upstream got %d chat requests, want 1", len(u.bodies))
	}
	return u.bodies[0]
}

// newParamsUpstream records chat bodies and answers with status/respBody.
func newParamsUpstream(t *testing.T, modelID string, status int, respBody string) *paramsUpstream {
	t.Helper()
	u := &paramsUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": modelID}}})
			return
		}
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, b)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func paramsRouter(t *testing.T, eps []config.OpenAIEndpoint, models ...config.VirtualLLM) string {
	t.Helper()
	reg := regpkg.NewProxyRegistry(&config.OpenAIConfig{Endpoints: eps})
	r := NewRelayRouter(":0", nil, reg, &config.VirtualLLMConfig{Models: models})
	srv := httptest.NewServer(r.server.Handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func vChat(params string) config.VirtualLLM {
	tg := config.VirtualLLMTarget{Endpoint: "gpubox", Model: "example-model"}
	if params != "" {
		tg.Params = json.RawMessage(params)
	}
	return config.VirtualLLM{Name: "vChat", Targets: []config.VirtualLLMTarget{tg}}
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body %q: %v", b, err)
	}
	return m
}

// captureRouterSlog collects slog records until the returned stop func runs.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func captureRouterSlog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

const appliedMsg = "relay router: virtual model params applied"

func appliedRecords(t *testing.T, buf *syncBuf, wantAtLeast int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var out []map[string]any
		for _, line := range strings.Split(buf.String(), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == appliedMsg {
				out = append(out, rec)
			}
		}
		if len(out) >= wantAtLeast || time.Now().After(deadline) {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Criteria 2-5, 13 and the worked example: each client body against the
// declared defaults, checked at the upstream.
func TestVirtualParams_WorkedExampleReachesUpstream(t *testing.T) {
	cases := map[string]struct{ extra, want string }{
		"none":                   {``, `{"chat_template_kwargs":{"enable_thinking":false},"reasoning_effort":"medium"}`},
		"client scalar wins":     {`,"reasoning_effort":"low"`, `{"chat_template_kwargs":{"enable_thinking":false},"reasoning_effort":"low"}`},
		"client object key wins": {`,"chat_template_kwargs":{"enable_thinking":true}`, `{"chat_template_kwargs":{"enable_thinking":true},"reasoning_effort":"medium"}`},
		"objects merge":          {`,"chat_template_kwargs":{"preserve_thinking":true}`, `{"chat_template_kwargs":{"preserve_thinking":true,"enable_thinking":false},"reasoning_effort":"medium"}`},
		"client array replaces":  {`,"reasoning_effort":["x"]`, `{"chat_template_kwargs":{"enable_thinking":false},"reasoning_effort":["x"]}`},
		"client null wins":       {`,"reasoning_effort":null`, `{"chat_template_kwargs":{"enable_thinking":false},"reasoning_effort":null}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			up := newParamsUpstream(t, "example-model", 200, `{"id":"r","choices":[]}`)
			url := paramsRouter(t, []config.OpenAIEndpoint{{Name: "gpubox", BaseURL: up.srv.URL + "/v1"}}, vChat(worked))
			resp := postBytes(t, url+"/v1/chat/completions",
				[]byte(`{"model":"vChat","messages":[{"role":"user","content":"hi"}]`+tc.extra+`}`))
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			got := decode(t, up.last(t))
			want := decode(t, []byte(tc.want))
			want["model"] = "example-model"
			want["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("upstream body = %v, want %v", got, want)
			}
		})
	}
}

// Criterion 6: absent, null and empty params send today's body byte for
// byte, and write no params log line.
func TestVirtualParams_NoDeclaredFieldsLeavesBodyAsToday(t *testing.T) {
	client := []byte(`{"model":"vChat","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`)
	want, err := rewriteProxyBody(client, "example-model")
	if err != nil {
		t.Fatal(err)
	}
	for _, params := range []string{"", "null", "{}"} {
		t.Run("params="+params, func(t *testing.T) {
			buf := captureRouterSlog(t)
			up := newParamsUpstream(t, "example-model", 200, `{}`)
			url := paramsRouter(t, []config.OpenAIEndpoint{{Name: "gpubox", BaseURL: up.srv.URL + "/v1"}}, vChat(params))
			postBytes(t, url+"/v1/chat/completions", client).Body.Close()
			if got := up.last(t); !bytes.Equal(got, want) {
				t.Errorf("upstream body = %s, want %s", got, want)
			}
			if recs := appliedRecords(t, buf, 0); len(recs) != 0 {
				t.Errorf("params log line written without declared fields: %v", recs)
			}
		})
	}
}

// Criterion 7: failover to a target without params adds nothing.
func TestVirtualParams_FailoverToTargetWithoutParamsAddsNothing(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "first-model"}}})
			return
		}
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
			}
		}
	}))
	defer dead.Close()
	up := newParamsUpstream(t, "second-model", 200, `{}`)
	url := paramsRouter(t, []config.OpenAIEndpoint{
		{Name: "first", BaseURL: dead.URL + "/v1"},
		{Name: "second", BaseURL: up.srv.URL + "/v1"},
	}, config.VirtualLLM{Name: "vChat", Targets: []config.VirtualLLMTarget{
		{Endpoint: "first", Model: "first-model", Params: json.RawMessage(worked)},
		{Endpoint: "second", Model: "second-model"},
	}})
	client := []byte(`{"model":"vChat","messages":[]}`)
	resp := postBytes(t, url+"/v1/chat/completions", client)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 from the second target", resp.StatusCode)
	}
	want, _ := rewriteProxyBody(client, "second-model")
	if got := up.last(t); !bytes.Equal(got, want) {
		t.Errorf("failover body = %s, want %s", got, want)
	}
}

// Criterion 10: the params model and a params-free one list as two rows.
func TestVirtualParams_CatalogListsBothModels(t *testing.T) {
	up := newParamsUpstream(t, "example-model", 200, `{}`)
	plain := vChat("")
	plain.Name = "vPlain"
	url := paramsRouter(t, []config.OpenAIEndpoint{{Name: "gpubox", BaseURL: up.srv.URL + "/v1"}}, vChat(worked), plain)
	var catalog struct {
		Data []map[string]any `json:"data"`
	}
	doRouterJSON(t, url+"/v1/models", http.MethodGet, nil, &catalog)
	ids := modelIDs(catalog.Data)
	if !sliceContains(ids, "vChat") || !sliceContains(ids, "vPlain") {
		t.Errorf("catalog ids = %v, want vChat and vPlain", ids)
	}
}

// Criteria 12, 18-22: one log line per request that used params, with names
// only, the upstream status, and the level by status; the upstream answer
// passes through unchanged.
func TestVirtualParams_LogLineAndPassThrough(t *testing.T) {
	const declared = `{"chat_template_kwargs":{"enable_thinking":false},"reasoning_effort":"zz-declared-value"}`
	for _, tc := range []struct {
		status int
		body   string
		level  string
	}{
		{200, `{"id":"ok","choices":[]}`, "INFO"},
		{400, `{"error":{"message":"unknown field reasoning_effort"}}`, "WARN"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			buf := captureRouterSlog(t)
			up := newParamsUpstream(t, "example-model", tc.status, tc.body)
			url := paramsRouter(t, []config.OpenAIEndpoint{{Name: "gpubox", BaseURL: up.srv.URL + "/v1"}}, vChat(declared))
			req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(
				`{"model":"vChat","messages":[{"role":"user","content":"SECRET-PROMPT"}],"chat_template_kwargs":{"enable_thinking":true}}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-secret-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.status || strings.TrimSpace(string(got)) != tc.body {
				t.Errorf("client got %d %s, want %d %s unchanged", resp.StatusCode, got, tc.status, tc.body)
			}

			recs := appliedRecords(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("params log lines = %d, want 1: %s", len(recs), buf.String())
			}
			rec := recs[0]
			if rec["model"] != "vChat" || rec["target"] != "gpubox/example-model" {
				t.Errorf("model/target = %v / %v", rec["model"], rec["target"])
			}
			if rec["status"] != float64(tc.status) {
				t.Errorf("status = %v, want %d", rec["status"], tc.status)
			}
			if rec["level"] != tc.level {
				t.Errorf("level = %v, want %s", rec["level"], tc.level)
			}
			if !reflect.DeepEqual(rec["injected"], []any{"reasoning_effort"}) ||
				!reflect.DeepEqual(rec["kept"], []any{"chat_template_kwargs.enable_thinking"}) {
				t.Errorf("injected/kept = %v / %v", rec["injected"], rec["kept"])
			}
			for _, secret := range []string{"zz-declared-value", "SECRET-PROMPT", "sk-secret-token"} {
				if strings.Contains(buf.String(), secret) {
					t.Errorf("log output leaks %q: %s", secret, buf.String())
				}
			}
		})
	}
}

// Criterion 16: a modelMap redirect reaching a virtual model gets the
// declared chat_template_kwargs.
func TestVirtualParams_AnthropicModelMapRedirectGetsDeclaredFields(t *testing.T) {
	var hdr http.Header
	var seen []byte
	up := openaiSSEUpstream(t, &hdr, &seen)
	ep := config.OpenAIEndpoint{Name: "gpubox", BaseURL: up.URL + "/v1"}
	virtual := &config.VirtualLLMConfig{Models: []config.VirtualLLM{vChat(worked)}}
	virtual.Models[0].Targets[0].Model = "backend-model"
	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid",
		map[string]string{"claude-haiku-4-5": "vChat"}), nil, nativeRegistry(ep), virtual)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp, body := sendAnthropic(t, srv.URL+"/v1/messages",
		`{"model":"claude-haiku-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	got := decode(t, seen)
	if !reflect.DeepEqual(got["chat_template_kwargs"], map[string]any{"enable_thinking": false}) {
		t.Errorf("upstream chat_template_kwargs = %v, want declared default; body %s", got["chat_template_kwargs"], seen)
	}
}

// Criterion 17: a virtual whose targets all use native Anthropic pass-through
// forwards the body with only the model spliced.
func TestVirtualParams_NativeAnthropicPassThroughGetsNoDeclaredFields(t *testing.T) {
	up := newNativeUpstream(t, 200, map[string]string{"Content-Type": "application/json"}, `{"type":"message","content":[]}`)
	ep := config.OpenAIEndpoint{Name: "live", BaseURL: up.srv.URL + "/v1", API: anthropicAPI}
	virtual := &config.VirtualLLMConfig{Models: []config.VirtualLLM{{Name: "vNative", Targets: []config.VirtualLLMTarget{
		{Endpoint: "live", Model: "backend-model", Params: json.RawMessage(worked)},
	}}}}
	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid",
		map[string]string{"claude-haiku-4-5": "vNative"}), nil, nativeRegistry(ep), virtual)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp, body := sendAnthropic(t, srv.URL+"/v1/messages", nativeClaudeCodeReq, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	hits := up.recorded()
	if len(hits) != 1 {
		t.Fatalf("upstream hits = %d, want 1", len(hits))
	}
	if want := strings.Replace(nativeClaudeCodeReq, nativeModelNeedle, `"model" :  "backend-model"`, 1); string(hits[0].body) != want {
		t.Errorf("native body was altered:\n%s", hits[0].body)
	}
}

// Criteria 6, 14 (unit): no-op cases return the same bytes unapplied; a body
// that is not a JSON object is returned unchanged.
func TestApplyVirtualParams_NoOpCases(t *testing.T) {
	obj := []byte(`{"model":"m","messages":[]}`)
	for name, tc := range map[string]struct {
		body   []byte
		params json.RawMessage
	}{
		"nil params":   {obj, nil},
		"null params":  {obj, json.RawMessage(`null`)},
		"empty params": {obj, json.RawMessage(`{}`)},
		"not json":     {[]byte(`not json`), json.RawMessage(worked)},
		"json array":   {[]byte(`[1,2]`), json.RawMessage(worked)},
		"empty body":   {[]byte(``), json.RawMessage(worked)},
	} {
		t.Run(name, func(t *testing.T) {
			got, use := applyVirtualParams(tc.body, tc.params)
			if use.applied || !bytes.Equal(got, tc.body) {
				t.Errorf("got %q applied=%v, want unchanged and unapplied", got, use.applied)
			}
		})
	}
}

// Criteria 19 (unit): injected and kept name the declared fields, dotted for
// nested keys, sorted.
func TestApplyVirtualParams_ReportsInjectedAndKept(t *testing.T) {
	got, use := applyVirtualParams(
		[]byte(`{"model":"m","reasoning_effort":"low","chat_template_kwargs":{"preserve_thinking":true}}`),
		json.RawMessage(worked))
	if !use.applied {
		t.Fatal("applied = false")
	}
	if !reflect.DeepEqual(use.injected, []string{"chat_template_kwargs.enable_thinking"}) ||
		!reflect.DeepEqual(use.kept, []string{"reasoning_effort"}) {
		t.Errorf("injected = %v, kept = %v", use.injected, use.kept)
	}
	m := decode(t, got)
	if !reflect.DeepEqual(m["chat_template_kwargs"], map[string]any{"preserve_thinking": true, "enable_thinking": false}) {
		t.Errorf("merged kwargs = %v", m["chat_template_kwargs"])
	}
}
