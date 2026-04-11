package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- SSE parsing tests ---

// sseResponse builds an http.Response whose body is the given SSE payload.
// We go through httptest.NewServer so StreamChunks exercises the real
// bufio/HTTP reader stack, not a hand-rolled mock.
func sseResponse(t *testing.T, payload string) *http.Response {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return resp
}

func TestOpenAISSEParsing_TextOnly(t *testing.T) {
	payload := `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}

data: {"choices":[{"index":0,"delta":{"content":" world"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}

data: [DONE]

`
	transport := &OpenAIChatTransport{}
	var deltas []ChatDelta
	result := transport.StreamChunks(sseResponse(t, payload), time.Now(), func(d ChatDelta) {
		deltas = append(deltas, d)
	})

	if result.Err != nil {
		t.Fatalf("unexpected err: %v", result.Err)
	}
	if result.FullText != "Hello world" {
		t.Errorf("FullText = %q, want %q", result.FullText, "Hello world")
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want none", result.ToolCalls)
	}
	if result.Stats.InputTokens != 12 || result.Stats.OutputTokens != 2 {
		t.Errorf("Stats tokens = in=%d out=%d, want in=12 out=2",
			result.Stats.InputTokens, result.Stats.OutputTokens)
	}
	if len(deltas) != 2 || deltas[0].Text != "Hello" || deltas[1].Text != " world" {
		t.Errorf("deltas = %+v, want [Hello][ world]", deltas)
	}
}

func TestOpenAISSEParsing_FragmentedToolCall(t *testing.T) {
	// A real tool call splits name + arguments across several deltas,
	// keyed by a shared index. We assert that the accumulator stitches
	// them back together correctly.
	payload := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"search","arguments":""}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"uery\":\"foo\"}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	transport := &OpenAIChatTransport{}
	result := transport.StreamChunks(sseResponse(t, payload), time.Now(), func(ChatDelta) {})

	if result.Err != nil {
		t.Fatalf("unexpected err: %v", result.Err)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(result.ToolCalls))
	}
	tc := result.ToolCalls[0]
	if tc.ID != "call_abc" {
		t.Errorf("ID = %q, want call_abc", tc.ID)
	}
	if tc.Name != "search" {
		t.Errorf("Name = %q, want search", tc.Name)
	}
	// Arguments should be the reconstructed JSON string.
	var args map[string]string
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("args not valid JSON: %v (raw: %s)", err, string(tc.Arguments))
	}
	if args["query"] != "foo" {
		t.Errorf("args[query] = %q, want foo", args["query"])
	}
}

func TestOpenAISSEParsing_ReasoningContent(t *testing.T) {
	// LM Studio / reasoning models emit delta.reasoning_content for thinking.
	payload := `data: {"choices":[{"index":0,"delta":{"reasoning_content":"Let me think..."}}]}

data: {"choices":[{"index":0,"delta":{"content":"Answer"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	transport := &OpenAIChatTransport{}
	var gotThinking, gotText string
	transport.StreamChunks(sseResponse(t, payload), time.Now(), func(d ChatDelta) {
		gotThinking += d.Thinking
		gotText += d.Text
	})

	if gotThinking != "Let me think..." {
		t.Errorf("thinking = %q", gotThinking)
	}
	if gotText != "Answer" {
		t.Errorf("text = %q", gotText)
	}
}

// --- BuildMessages tests ---

func TestOpenAIBuildMessages_TextUserOnly(t *testing.T) {
	transport := &OpenAIChatTransport{}
	msgs := []Message{
		{Role: "user", Content: json.RawMessage(`"Hi"`)},
	}
	out := transport.BuildMessages("be helpful", msgs)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (system + user)", len(out))
	}
	if out[0]["role"] != "system" || out[0]["content"] != "be helpful" {
		t.Errorf("system = %+v", out[0])
	}
	if out[1]["role"] != "user" || out[1]["content"] != "Hi" {
		t.Errorf("user = %+v", out[1])
	}
}

func TestOpenAIBuildMessages_UserWithImage(t *testing.T) {
	transport := &OpenAIChatTransport{}
	msgs := []Message{
		{
			Role:    "user",
			Content: json.RawMessage(`"what is this?"`),
			Files: []FileAttachment{
				{Name: "cat.png", MimeType: "image/png", Data: "abc123"},
			},
		},
	}
	out := transport.BuildMessages("", msgs)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	parts, ok := out[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content is %T, want []map[string]any", out[0]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (text + image)", len(parts))
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "what is this?" {
		t.Errorf("text part = %+v", parts[0])
	}
	if parts[1]["type"] != "image_url" {
		t.Errorf("image part type = %v", parts[1]["type"])
	}
	imgURL := parts[1]["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(imgURL, "data:image/png;base64,abc123") {
		t.Errorf("image url = %q", imgURL)
	}
}

func TestOpenAIBuildMessages_ToolCallRoundTrip(t *testing.T) {
	transport := &OpenAIChatTransport{}
	// Simulate a persisted session: user → assistant with tool_calls → tool result → follow-up.
	toolCallsJSON, _ := json.Marshal([]NormalizedToolCall{
		{ID: "call_abc", Name: "search", Arguments: json.RawMessage(`{"q":"foo"}`)},
	})
	msgs := []Message{
		{Role: "user", Content: json.RawMessage(`"find foo"`)},
		{Role: "assistant", Content: json.RawMessage(`"looking it up"`), ToolCalls: toolCallsJSON},
		{Role: "tool", ToolName: "search", Content: json.RawMessage(`"result: 42"`)},
	}
	out := transport.BuildMessages("", msgs)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	// Assistant entry should have tool_calls with id/type/function.
	asst := out[1]
	tcList, ok := asst["tool_calls"].([]map[string]any)
	if !ok {
		t.Fatalf("assistant tool_calls is %T", asst["tool_calls"])
	}
	if len(tcList) != 1 || tcList[0]["id"] != "call_abc" || tcList[0]["type"] != "function" {
		t.Errorf("assistant tool_calls = %+v", tcList)
	}
	// Tool entry should carry the same tool_call_id.
	if out[2]["tool_call_id"] != "call_abc" {
		t.Errorf("tool_call_id = %v, want call_abc", out[2]["tool_call_id"])
	}
}

// --- AppendToolResult ---

func TestOpenAIAppendToolResult(t *testing.T) {
	transport := &OpenAIChatTransport{}
	tc := NormalizedToolCall{ID: "call_xyz", Name: "search"}
	messages := []map[string]any{}
	out := transport.AppendToolResult(messages, tc, "result text")
	if len(out) != 1 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0]["role"] != "tool" || out[0]["tool_call_id"] != "call_xyz" || out[0]["content"] != "result text" {
		t.Errorf("entry = %+v", out[0])
	}
}

// --- Config loading ---

func TestOpenAIConfigLoad_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "endpoints.json")
	body := `{"endpoints":[{"name":"lm","baseURL":"http://localhost:1234/v1","apiKey":"k","group":"LM"},{"name":"oai","baseURL":"http://x/","apiKey":""}]}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOpenAIConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Endpoints) != 2 {
		t.Fatalf("endpoints = %d", len(cfg.Endpoints))
	}
	if got := cfg.Find("lm"); got == nil || got.BaseURL != "http://localhost:1234/v1" || got.Group != "LM" {
		t.Errorf("lm = %+v", got)
	}
	// Trailing slash stripped; Group defaults to Name when empty.
	if got := cfg.Find("oai"); got == nil || got.BaseURL != "http://x" || got.Group != "oai" {
		t.Errorf("oai = %+v", got)
	}
	// Unknown lookup returns nil.
	if got := cfg.Find("nope"); got != nil {
		t.Errorf("nope = %+v, want nil", got)
	}
	// Names in declaration order.
	names := cfg.Names()
	if len(names) != 2 || names[0] != "lm" || names[1] != "oai" {
		t.Errorf("names = %v", names)
	}
}

func TestOpenAIConfigLoad_EnvFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.json")

	t.Setenv("OPENAI_BASE_URL", "http://env-server/v1")
	t.Setenv("OPENAI_API_KEY", "env-key")
	t.Setenv("OPENAI_ENDPOINT_NAME", "env-endpoint")

	cfg, err := LoadOpenAIConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Endpoints) != 1 {
		t.Fatalf("endpoints = %d", len(cfg.Endpoints))
	}
	e := cfg.Endpoints[0]
	if e.Name != "env-endpoint" || e.BaseURL != "http://env-server/v1" || e.APIKey != "env-key" {
		t.Errorf("env endpoint = %+v", e)
	}
}

func TestOpenAIConfigLoad_EmptyWhenNothingConfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.json")

	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")

	cfg, err := LoadOpenAIConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Endpoints) != 0 {
		t.Errorf("endpoints = %d, want 0", len(cfg.Endpoints))
	}
}

// --- Provider-type derivation ---

func TestDeriveProviderType_OpenAIPrefix(t *testing.T) {
	cfg := &OpenAIConfig{
		Endpoints: []OpenAIEndpoint{
			{Name: "lmstudio", BaseURL: "http://x/v1"},
			{Name: "omlx", BaseURL: "http://y/v1"},
		},
	}
	cases := []struct {
		model string
		want  string
	}{
		{"haiku", "claude"},
		{"sonnet", "claude"},
		{"opus", "claude"},
		{"lmstudio/qwen-7b", "openai"},
		{"omlx/llama3", "openai"},
		{"unknown/qwen-7b", "ollama"}, // unknown prefix falls through
		{"qwen:7b", "ollama"},         // no slash
		{"gemma3:4b", "ollama"},
		{"/leading", "ollama"}, // malformed
	}
	for _, tc := range cases {
		got := deriveProviderType(tc.model, cfg)
		if got != tc.want {
			t.Errorf("deriveProviderType(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

// --- End-to-end transport round-trip with httptest ---

// TestOpenAIChatTransport_Roundtrip exercises the whole PostChat →
// StreamChunks path through a real HTTP server, verifying that the
// auth header is sent, the request body is well-formed, and the
// SSE response is parsed correctly.
func TestOpenAIChatTransport_Roundtrip(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			io.WriteString(w, `{"data":[{"id":"test-model"}]}`)
		case "/chat/completions":
			gotAuth = r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}

data: [DONE]

`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	endpoint := OpenAIEndpoint{Name: "test", BaseURL: srv.URL, APIKey: "sk-test"}
	transport := NewOpenAIChatTransport(endpoint, "test-model", nil, srv.Client())

	if err := transport.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	msgs := transport.BuildMessages("", []Message{
		{Role: "user", Content: json.RawMessage(`"hi"`)},
	})
	resp, err := transport.PostChat(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	result := transport.StreamChunks(resp, time.Now(), func(ChatDelta) {})
	if result.Err != nil {
		t.Fatalf("stream: %v", result.Err)
	}
	if result.FullText != "hi" {
		t.Errorf("text = %q", result.FullText)
	}
	if result.Stats.InputTokens != 3 || result.Stats.OutputTokens != 1 {
		t.Errorf("stats = %+v", result.Stats)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody["model"] != "test-model" {
		t.Errorf("body.model = %v", gotBody["model"])
	}
	if gotBody["stream"] != true {
		t.Errorf("body.stream = %v", gotBody["stream"])
	}
}
