package main

// Coverage for the router-level Anthropic-compat wiring (relay_router_anthropic.go):
// passthrough byte-fidelity, redirect dispatch + credential isolation, error
// mapping, config gating, and /v1/models catalog rows. Style matches
// relay_router_reasoning_effort_test.go: httptest fakes standing in for
// backends, NewRelayRouter + a pre-serve setter, the router's real handler
// wrapped in a second httptest.NewServer, postBytes to drive it.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newAnthropicRouter builds a *RelayRouter with router.anthropic configured
// against upstream (standing in for api.anthropic.com), plus whatever
// managers/registry/virtual config the caller passes for the redirect path.
func newAnthropicRouter(t *testing.T, cfg *AnthropicRouterConfig, managers []*ServerManager, registry *ProxyRegistry, virtual *VirtualLLMConfig) *RelayRouter {
	t.Helper()
	r := NewRelayRouter(":0", managers, registry, virtual)
	r.setAnthropic(cfg)
	return r
}

func anthropicUpstreamCfg(t *testing.T, upstreamURL string, modelMap map[string]string) *AnthropicRouterConfig {
	t.Helper()
	return &AnthropicRouterConfig{
		Upstream:            upstreamURL,
		ModelMap:            modelMap,
		PingIntervalSeconds: 3600, // effectively off for tests that don't exercise it
	}
}

// ---------------------------------------------------------------------------
// Config gating
// ---------------------------------------------------------------------------

func TestAnthropic_RoutesAbsent_404WhenNotConfigured(t *testing.T) {
	r := NewRelayRouter(":0", nil, nil, nil) // setAnthropic never called
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages", []byte(`{"model":"x","messages":[]}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["type"] != "error" {
		t.Errorf("body = %v, want Anthropic error envelope", body)
	}
}

// ---------------------------------------------------------------------------
// Passthrough
// ---------------------------------------------------------------------------

func TestAnthropic_Passthrough_HeadersAndBodyByteIdentical(t *testing.T) {
	var gotAuth, gotVersion, gotBeta, gotXFF string
	var gotBody []byte
	var gotPath string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("anthropic-version")
		gotBeta = r.Header.Get("anthropic-beta")
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_1","type":"message"}`))
	}))
	defer fake.Close()

	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, fake.URL, nil), nil, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	sent := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(sent))
	req.Header.Set("Authorization", "Bearer oauth-token-xyz")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotAuth != "Bearer oauth-token-xyz" {
		t.Errorf("upstream Authorization = %q, want forwarded verbatim", gotAuth)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("upstream anthropic-version = %q, want forwarded verbatim", gotVersion)
	}
	if gotBeta != "oauth-2025-04-20" {
		t.Errorf("upstream anthropic-beta = %q, want forwarded verbatim", gotBeta)
	}
	if gotXFF != "" {
		t.Errorf("upstream X-Forwarded-For = %q, want absent (passthrough must stay invisible)", gotXFF)
	}
	if string(gotBody) != string(sent) {
		t.Errorf("upstream body = %s, want byte-identical %s", gotBody, sent)
	}
}

func TestAnthropic_Passthrough_ErrorBodyRelayedByteIdentical(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}`))
	}))
	defer fake.Close()

	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, fake.URL, nil), nil, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages", []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	want := `{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}`
	if string(body) != want {
		t.Errorf("body = %s, want byte-identical %s", body, want)
	}
}

func TestAnthropic_Passthrough_APIPrefixReachesUpstream(t *testing.T) {
	var hit bool
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hello" {
			hit = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fake.Close()

	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, fake.URL, nil), nil, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp, err := http.Head(srv.URL + "/api/hello")
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !hit {
		t.Error("fake upstream never saw /api/hello — passthrough prefix routing broken")
	}
}

// ---------------------------------------------------------------------------
// Redirect
// ---------------------------------------------------------------------------

// anthropicRedirectFixture wires a router whose "local-model" modelMap
// target is a managed alias backed by upstream — same trick
// newManagedAliasRouter uses in the reasoning-effort tests, reused here so
// the redirect path exercises a real managed-server Acquire/dispatch, not a
// mock of it.
func anthropicRedirectFixture(t *testing.T, alias string, upstream *httptest.Server, modelMap map[string]string) *RelayRouter {
	t.Helper()
	port := upstream.Listener.Addr().(*net.TCPAddr).Port
	mgr := NewServerManager(llamaProfile, &ServerConfig{
		Models: []ServerModelConfig{{Alias: alias, Args: map[string]any{"model": "/fake"}}},
	}, "")
	inst := &serverInstance{ready: make(chan struct{})}
	inst.port = port
	inst.healthy.Store(true)
	mgr.mu.Lock()
	mgr.instances[alias] = inst
	mgr.mu.Unlock()

	return newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid", modelMap), []*ServerManager{mgr}, nil, nil)
}

// openaiSSEUpstream answers with a canned OpenAI-style SSE stream (one text
// delta, a finish_reason, then a usage-only chunk) and records every request
// header + body it receives.
func openaiSSEUpstream(t *testing.T, seenHeaders *http.Header, seenBody *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seenHeaders = r.Header.Clone()
		*seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"four\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":1}}\n\n")
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnthropic_Redirect_TranslatesAndStreamsBack(t *testing.T) {
	var seenHeaders http.Header
	var seenBody []byte
	upstream := openaiSSEUpstream(t, &seenHeaders, &seenBody)
	r := anthropicRedirectFixture(t, "local-model", upstream, map[string]string{"claude-haiku-4-5": "local-model"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(
		[]byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"what's 2+2"}],"stream":true}`)))
	req.Header.Set("Authorization", "Bearer real-anthropic-oauth-token")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)

	// Credential isolation: the local backend must never see the client's
	// real Anthropic credential.
	if seenHeaders.Get("Authorization") != "" {
		t.Errorf("local backend saw Authorization %q, want none — client credential must not reach a redirected local model", seenHeaders.Get("Authorization"))
	}
	if seenHeaders.Get("anthropic-beta") != "" {
		t.Errorf("local backend saw anthropic-beta %q, want none", seenHeaders.Get("anthropic-beta"))
	}

	var translatedReq map[string]any
	if err := json.Unmarshal(seenBody, &translatedReq); err != nil {
		t.Fatalf("decode translated body: %v", err)
	}
	if translatedReq["model"] != "local-model" {
		t.Errorf("translated model = %v, want local-model", translatedReq["model"])
	}

	events := parseSSEEvents(t, body)
	types := eventTypes(events)
	if len(types) == 0 || types[0] != "message_start" {
		t.Fatalf("event sequence = %v, want starting with message_start", types)
	}
	found := false
	for _, e := range events {
		if e.event == "content_block_delta" {
			if delta, ok := e.data["delta"].(map[string]any); ok && delta["text"] == "four" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no content_block_delta with text %q found in %v", "four", events)
	}
}

func TestAnthropic_Redirect_NonStreamingAggregation(t *testing.T) {
	var seenHeaders http.Header
	var seenBody []byte
	upstream := openaiSSEUpstream(t, &seenHeaders, &seenBody)
	r := anthropicRedirectFixture(t, "local-model", upstream, map[string]string{"claude-haiku-4-5": "local-model"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages",
		[]byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`)) // no "stream" -> non-streaming
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json for a non-streaming response", ct)
	}
	var msg map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if msg["type"] != "message" {
		t.Errorf("type = %v, want message", msg["type"])
	}
	content := msg["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "four" {
		t.Errorf("content = %v, want one text block %q", content, "four")
	}
}

// openaiToolCallSSEUpstream answers with a canned OpenAI-style SSE stream
// carrying one complete tool call, exercising the redirect path's full
// tool_use round trip end to end (translator-level coverage for this
// already lives in anthropic_translate_test.go; this proves the HTTP wiring
// on top of it).
func openaiToolCallSSEUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}}]}`+"\n\n")
		flusher.Flush()
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnthropic_Redirect_ToolCallEndToEnd(t *testing.T) {
	upstream := openaiToolCallSSEUpstream(t)
	r := anthropicRedirectFixture(t, "local-model", upstream, map[string]string{"claude-haiku-4-5": "local-model"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages", []byte(`{
		"model": "claude-haiku-4-5",
		"messages": [{"role": "user", "content": "what's the weather in SF"}],
		"tools": [{"name": "get_weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
		"stream": true
	}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	events := parseSSEEvents(t, body)

	var toolBlock map[string]any
	var toolDelta map[string]any
	for _, e := range events {
		if e.event == "content_block_start" {
			if cb, ok := e.data["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				toolBlock = cb
			}
		}
		if e.event == "content_block_delta" {
			if d, ok := e.data["delta"].(map[string]any); ok && d["type"] == "input_json_delta" {
				toolDelta = d
			}
		}
	}
	if toolBlock == nil {
		t.Fatalf("no tool_use content_block_start found; events: %v", eventTypes(events))
	}
	if toolBlock["name"] != "get_weather" || toolBlock["id"] != "call_1" {
		t.Errorf("tool_use block = %v, want name=get_weather id=call_1", toolBlock)
	}
	if toolDelta == nil || toolDelta["partial_json"] != `{"city":"SF"}` {
		t.Errorf("tool_use delta = %v, want partial_json %q", toolDelta, `{"city":"SF"}`)
	}

	last := events[len(events)-2]
	if last.event != "message_delta" || last.data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("final message_delta = %v, want stop_reason=tool_use", last)
	}
}

func TestAnthropic_Redirect_ContextOverflowErrorNormalized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"the request exceeds the available context size (32768 tokens)"}}`))
	}))
	defer upstream.Close()

	r := anthropicRedirectFixture(t, "local-model", upstream, map[string]string{"claude-haiku-4-5": "local-model"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages",
		[]byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if len(msg) < len("prompt is too long:") || msg[:len("prompt is too long:")] != "prompt is too long:" {
		t.Errorf("message = %q, want prefix %q", msg, "prompt is too long:")
	}
}

func TestAnthropic_Redirect_BackendDown_MapsToOverloadedError(t *testing.T) {
	// A managed alias whose "already running" instance points at a closed
	// port: Acquire succeeds (the instance exists in the manager's map,
	// exactly like anthropicRedirectFixture's live-instance trick), but the
	// dial itself fails, exercising newUpstreamProxy's default 502
	// ErrorHandler path through the translating writer. Deliberately not an
	// OpenAI endpoint: ProxyRegistry.LookupModel gates on its own
	// reachability probe first, which would fail closed with "unknown
	// model" before ever reaching a dial — a different (and already
	// covered) code path, not the mid-dispatch failure this test targets.
	closedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadPort := closedListener.Addr().(*net.TCPAddr).Port
	closedListener.Close()

	mgr := NewServerManager(llamaProfile, &ServerConfig{
		Models: []ServerModelConfig{{Alias: "dead-model", Args: map[string]any{"model": "/fake"}}},
	}, "")
	inst := &serverInstance{ready: make(chan struct{})}
	inst.port = deadPort
	inst.healthy.Store(true)
	mgr.mu.Lock()
	mgr.instances["dead-model"] = inst
	mgr.mu.Unlock()

	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid", map[string]string{"claude-haiku-4-5": "dead-model"}), []*ServerManager{mgr}, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages",
		[]byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", resp.StatusCode, body)
	}
	var decoded map[string]any
	json.Unmarshal(body, &decoded)
	errObj := decoded["error"].(map[string]any)
	if errObj["type"] != "overloaded_error" {
		t.Errorf("error.type = %v, want overloaded_error", errObj["type"])
	}
}

// ---------------------------------------------------------------------------
// modelMap also dispatchable via the plain OpenAI path, and listed in /v1/models
// ---------------------------------------------------------------------------

func TestAnthropic_ModelMapDispatchableViaOpenAIPath(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	r := anthropicRedirectFixture(t, "local-model", upstream, map[string]string{"relay/coder": "local-model"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(`{"model":"relay/coder","messages":[]}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got struct {
		Model string `json:"model"`
	}
	json.Unmarshal(seenBody, &got)
	if got.Model != "local-model" {
		t.Errorf("upstream saw model = %q, want rewritten to local-model", got.Model)
	}
}

func TestAnthropic_ModelsCatalog_IncludesSortedMappedRows(t *testing.T) {
	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid", map[string]string{
		"claude-haiku-4-5": "local-model",
		"relay/coder":      "vCode",
	}), nil, nil, nil)
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	ids := modelIDs(body.Data)

	var haikuIdx, coderIdx = -1, -1
	for i, id := range ids {
		switch id {
		case "claude-haiku-4-5":
			haikuIdx = i
		case "relay/coder":
			coderIdx = i
		}
	}
	if haikuIdx == -1 || coderIdx == -1 {
		t.Fatalf("catalog ids = %v, want both mapped keys present", ids)
	}
	if haikuIdx > coderIdx {
		t.Errorf("mapped rows not sorted: claude-haiku-4-5 at %d, relay/coder at %d", haikuIdx, coderIdx)
	}
}

// ---------------------------------------------------------------------------
// Ping goroutine lifecycle
//
// Regression coverage for a live crash: a client disconnecting mid-stream
// makes httputil.ReverseProxy's body copy fail, and the stdlib's documented
// response to that is panic(http.ErrAbortHandler) — recovered higher up by
// net/http's own per-connection serve loop, not by anything in this
// package. handleAnthropicRedirect used to call tw.finish() as a bare
// follow-up statement after p.handleProxy(tw, innerReq); on the panic path
// that line never ran, leaking the ping goroutine startPing had spawned.
// Observed live: the leaked goroutine kept ticking past the request's
// lifetime and eventually wrote to a *http.response Go had already reset
// and returned to its sync.Pool for a later request on the same keep-alive
// connection — a nil-pointer panic in bufio.Writer.Write, which (unlike
// ErrAbortHandler inside the handler goroutine itself) net/http does not
// recover, crashing the whole process. Fixed by deferring tw.finish() so
// its stopPing() call reliably runs during unwind. This test exercises the
// defer/panic contract directly rather than trying to reproduce the exact
// stdlib trigger, which is inherently timing-dependent.
// ---------------------------------------------------------------------------

func TestAnthropic_PingGoroutineStoppedEvenWhenWrappedCallPanics(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := newAnthropicStreamTranslator(true, "m", 0)
	tw := &translatingResponseWriter{
		real:         rec,
		wantStream:   true,
		translator:   tr,
		pingInterval: 5 * time.Millisecond,
	}

	func() {
		defer func() { recover() }()  // stands in for net/http's own ErrAbortHandler recovery
		defer tw.finish()             // exactly handleAnthropicRedirect's pattern
		tw.WriteHeader(http.StatusOK) // starts the ping goroutine (beginReal -> startPing)
		panic(http.ErrAbortHandler)
	}()

	// stopPing's pingWG.Wait() inside finish() only returns once the ping
	// goroutine has actually exited, so by the time the outer func above
	// returns, no further ticks should ever fire. Confirm the recorder's
	// body stops growing across a window several ping intervals wide.
	time.Sleep(20 * time.Millisecond)
	lenAfterFinish := rec.Body.Len()
	time.Sleep(40 * time.Millisecond) // several ping intervals, if the goroutine leaked
	if rec.Body.Len() != lenAfterFinish {
		t.Errorf("body grew from %d to %d bytes after finish() returned — the ping goroutine leaked past the panic instead of being stopped by the defer",
			lenAfterFinish, rec.Body.Len())
	}
}

// ---------------------------------------------------------------------------
// Ping keepalive
// ---------------------------------------------------------------------------

func TestAnthropic_Redirect_PingDuringSlowBackend(t *testing.T) {
	// Headers arrive immediately (as real llama-server/oMLX backends do),
	// then the stream goes silent mid-generation before the next chunk.
	// Pings cover exactly this gap, not the pre-header gap (model launch /
	// prompt processing), which is Claude Code's own request timeout's
	// problem instead.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond) // longer than the test's ping interval
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	port := upstream.Listener.Addr().(*net.TCPAddr).Port
	mgr := NewServerManager(llamaProfile, &ServerConfig{
		Models: []ServerModelConfig{{Alias: "slow-model", Args: map[string]any{"model": "/fake"}}},
	}, "")
	inst := &serverInstance{ready: make(chan struct{})}
	inst.port = port
	inst.healthy.Store(true)
	mgr.mu.Lock()
	mgr.instances["slow-model"] = inst
	mgr.mu.Unlock()

	r := NewRelayRouter(":0", []*ServerManager{mgr}, nil, nil)
	r.setAnthropic(&AnthropicRouterConfig{
		Upstream:            "https://unused.invalid",
		ModelMap:            map[string]string{"claude-haiku-4-5": "slow-model"},
		PingIntervalSeconds: 0, // 0 -> default (15s)... overridden below via direct state mutation
	})
	r.anthropic.pingInterval = 50 * time.Millisecond // fast enough for a hermetic test

	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/messages",
		[]byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	events := parseSSEEvents(t, body)
	pinged := false
	for _, e := range events {
		if e.event == "ping" {
			pinged = true
		}
	}
	if !pinged {
		t.Errorf("no ping event seen while backend was slow; events: %v", eventTypes(events))
	}
}
