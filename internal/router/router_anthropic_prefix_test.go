package router

// Prefix stability: a recorded multi-turn Claude Code conversation (one
// request body per line) must reach a native upstream with turn N's body a
// prefix of turn N+1's, so the engine's prompt cache keeps matching. The same
// fixture runs through the translated path, which only reports what it sees.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"relayllm/internal/config"
	regpkg "relayllm/internal/registry"
	"sync"
	"testing"
)

const (
	prefixClientModel   = `"model":"relay/splash"`
	prefixUpstreamModel = `"model":"upstream-model"`
)

func prefixFixture(t *testing.T) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "anthropic_native", "claude_code_conversation.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var lines [][]byte
	for _, l := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			lines = append(lines, l)
		}
	}
	if len(lines) < 4 {
		t.Fatalf("fixture invalid: %d turns, want at least 4", len(lines))
	}
	return lines
}

func prefixDecode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v map[string]any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return v
}

// prefixDropCacheControl removes every "cache_control" member at any depth.
func prefixDropCacheControl(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if k != "cache_control" {
				out[k] = prefixDropCacheControl(e)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = prefixDropCacheControl(e)
		}
		return out
	}
	return v
}

// prefixNormalizeMessage undoes the two ways Claude Code itself rewrites its
// own history between requests: string content vs a single text block, and
// thinking blocks present in one request's history but not the next.
func prefixNormalizeMessage(m any) any {
	msg, ok := m.(map[string]any)
	if !ok {
		return m
	}
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		out[k] = v
	}
	switch c := msg["content"].(type) {
	case string:
		out["content"] = []any{map[string]any{"type": "text", "text": c}}
	case []any:
		kept := []any{}
		for _, blk := range c {
			if b, ok := blk.(map[string]any); ok && (b["type"] == "thinking" || b["type"] == "redacted_thinking") {
				continue
			}
			kept = append(kept, blk)
		}
		out["content"] = kept
	}
	return out
}

// prefixEncode re-encodes with sorted keys (encoding/json sorts map keys).
func prefixEncode(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// prefixDivergence compares two consecutive request views and returns "" when
// turn next extends turn prev, else where it first diverges. keys are the
// top-level members that must be equal; messages must extend.
func prefixDivergence(prev, next map[string]any, keys []string, normalize func(any) any) string {
	for _, k := range keys {
		if prefixEncode(normalize(prev[k])) != prefixEncode(normalize(next[k])) {
			return fmt.Sprintf("%s differs", k)
		}
	}
	pm, _ := prev["messages"].([]any)
	nm, _ := next["messages"].([]any)
	if len(nm) <= len(pm) {
		return fmt.Sprintf("messages did not grow (%d -> %d)", len(pm), len(nm))
	}
	for i := range pm {
		if prefixEncode(normalize(pm[i])) != prefixEncode(normalize(nm[i])) {
			return fmt.Sprintf("messages[%d] differs", i)
		}
	}
	return ""
}

var prefixAnthropicKeys = []string{"system", "tools"}

func prefixStrictDivergence(prev, next map[string]any) string {
	return prefixDivergence(prev, next, prefixAnthropicKeys, prefixDropCacheControl)
}

func prefixCanonDivergence(prev, next map[string]any) string {
	return prefixDivergence(prev, next, prefixAnthropicKeys, func(v any) any {
		v = prefixDropCacheControl(v)
		if m, ok := v.(map[string]any); ok && m["role"] != nil {
			return prefixNormalizeMessage(m)
		}
		return v
	})
}

func prefixVerdict(d string) string {
	if d == "" {
		return "holds"
	}
	return "breaks: " + d
}

// prefixUpstream records every request body in order and answers with reply.
type prefixUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	paths  []string
}

func newPrefixUpstream(t *testing.T, reply func(w http.ResponseWriter, body []byte)) *prefixUpstream {
	t.Helper()
	u := &prefixUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, b)
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()
		reply(w, b)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// prefixRun sends every fixture line to /v1/messages through a router whose
// modelMap sends relay/splash to endpoint ep, and returns what ep received.
func prefixRun(t *testing.T, lines [][]byte, api []string, wantPath string, reply func(http.ResponseWriter, []byte)) [][]byte {
	t.Helper()
	up := newPrefixUpstream(t, reply)
	ep := config.OpenAIEndpoint{Name: "ep", BaseURL: up.srv.URL + "/v1", API: api}
	reg := regpkg.NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{ep}})
	reg.SetStatusForTest(ep, true, regpkg.UpstreamModel{ID: "upstream-model"})
	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid",
		map[string]string{"relay/splash": "ep/upstream-model"}), nil, reg, nil)
	srv := httptest.NewServer(r.server.Handler)
	t.Cleanup(srv.Close)

	for i, line := range lines {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(line))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("turn %d: request: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("turn %d: status %d, body %s", i, resp.StatusCode, body)
		}
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.bodies) != len(lines) {
		t.Fatalf("upstream saw %d requests, want %d", len(up.bodies), len(lines))
	}
	for i, p := range up.paths {
		if p != wantPath {
			t.Fatalf("turn %d reached %s, want %s", i, p, wantPath)
		}
	}
	return up.bodies
}

func TestAnthropicPrefix_NativePathKeepsClientPrefix(t *testing.T) {
	lines := prefixFixture(t)

	client := make([]map[string]any, len(lines))
	for i, l := range lines {
		client[i] = prefixDecode(t, l)
	}
	for i := 0; i+1 < len(client); i++ {
		if d := prefixCanonDivergence(client[i], client[i+1]); d != "" {
			t.Fatalf("fixture invalid: turn %d -> %d at the client: %s", i, i+1, d)
		}
	}

	got := prefixRun(t, lines, []string{config.APIOpenAI, config.APIAnthropic}, "/v1/messages",
		func(w http.ResponseWriter, _ []byte) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"msg_p1","type":"message","role":"assistant","model":"upstream-model",`+
				`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,`+
				`"usage":{"input_tokens":10,"output_tokens":1}}`)
		})

	upstream := make([]map[string]any, len(got))
	for i, line := range lines {
		// The client's model member is the object's first member and occurs
		// once, so replacing that exact span is replacing the top-level value.
		if n := bytes.Count(line, []byte(prefixClientModel)); n != 1 || bytes.Index(line, []byte(prefixClientModel)) != 1 {
			t.Fatalf("fixture invalid: turn %d does not start with a unique %s", i, prefixClientModel)
		}
		want := bytes.Replace(line, []byte(prefixClientModel), []byte(prefixUpstreamModel), 1)
		if !bytes.Equal(got[i], want) {
			at := 0
			for at < len(want) && at < len(got[i]) && want[at] == got[i][at] {
				at++
			}
			t.Errorf("turn %d: upstream body differs from the client body beyond model, first at byte %d (got %d bytes, want %d)",
				i, at, len(got[i]), len(want))
		}
		upstream[i] = prefixDecode(t, got[i])
	}

	for i := 0; i+1 < len(upstream); i++ {
		if d := prefixCanonDivergence(upstream[i], upstream[i+1]); d != "" {
			t.Errorf("native upstream turn %d -> %d: prefix broken: %s", i, i+1, d)
		}
		strictClient := prefixStrictDivergence(client[i], client[i+1])
		strictNative := prefixStrictDivergence(upstream[i], upstream[i+1])
		t.Logf("strict (cache_control dropped only) turn %d -> %d: client %s; native upstream %s",
			i, i+1, prefixVerdict(strictClient), prefixVerdict(strictNative))
		if strictClient != strictNative {
			t.Errorf("turn %d -> %d: native path changed the strict prefix result (client %q, upstream %q)",
				i, i+1, strictClient, strictNative)
		}
	}
}

func TestAnthropicPrefix_TranslatedPathReported(t *testing.T) {
	lines := prefixFixture(t)

	got := prefixRun(t, lines, nil, "/v1/chat/completions", func(w http.ResponseWriter, body []byte) {
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`,
			`data: [DONE]`,
		} {
			io.WriteString(w, chunk+"\n\n")
		}
	})

	for i := 0; i+1 < len(got); i++ {
		d := prefixDivergence(prefixDecode(t, got[i]), prefixDecode(t, got[i+1]), []string{"tools"}, func(v any) any { return v })
		t.Logf("translated upstream turn %d -> %d: %s", i, i+1, prefixVerdict(d))
	}
}
