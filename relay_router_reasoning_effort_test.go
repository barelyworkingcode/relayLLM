package main

// Coverage for the router-level reasoning_effort rewrite (RouterConfig /
// rewriteProxyBody in relay_router.go). See CLAUDE.md's Relay-router section
// for why this exists: clients and backends disagree about what a
// reasoning_effort value of "off" looks like on the wire, and at least one
// real combination (llama.cpp + a client whose "off" clamps to "minimal")
// otherwise 500s in a retry loop.
//
// Style matches relay_router_test.go: httptest fakes standing in for
// backends, postBytes to drive the router's real HTTP handler.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newManagedAliasRouter wires a RelayRouter whose single managed alias is
// already "running" against upstream, without spawning a real llama-server —
// same trick relay_router_models_test.go uses (mgr.instances[alias] = ...),
// extended with a real port so a full HTTP round trip through the router's
// reverse proxy actually lands on upstream.
func newManagedAliasRouter(t *testing.T, alias string, upstream *httptest.Server, effortMap map[string]string) *RelayRouter {
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

	r := NewRelayRouter(":0", []*ServerManager{mgr}, nil, nil)
	if effortMap != nil {
		r.setReasoningEffortMap(effortMap)
	}
	return r
}

// bodyRecordingUpstream returns a fake backend that stashes every request
// body it receives (into *seen) and answers 200 with a minimal OpenAI-shaped
// body — enough for the router's reverse proxy to consider the exchange a
// success on every dispatch branch (managed, endpoint, virtual).
func bodyRecordingUpstream(t *testing.T, seen *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "upstream-model"}}})
			return
		}
		*seen, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp","choices":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// Managed-alias path
// ---------------------------------------------------------------------------

// Requirement 1: with no "router" config at all, a body is untouched —
// behavior must be byte-identical to today. The managed-alias route is the
// most sensitive case: unlike the endpoint route it has no other reason to
// decode/remarshal the body, so this also proves rewriteProxyBody's
// short-circuit actually short-circuits rather than round-tripping and
// happening to reproduce the same bytes.
func TestReasoningEffort_NoRouterConfig_ManagedAliasPathUnchanged(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	r := newManagedAliasRouter(t, "reasoning-alias", upstream, nil) // no setReasoningEffortMap call

	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	sent := `{"model":"reasoning-alias","reasoning_effort":"minimal","stream":true}`
	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(sent))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if string(seenBody) != sent {
		t.Errorf("upstream saw %q, want byte-identical %q", string(seenBody), sent)
	}
}

// Requirement 3 (+6, managed-alias branch): a value mapped to "" removes the
// key entirely rather than sending reasoning_effort:"".
func TestReasoningEffort_ManagedAliasPath_EmptyMappedValueRemovesKey(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	r := newManagedAliasRouter(t, "reasoning-alias", upstream, map[string]string{"minimal": ""})

	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"reasoning-alias","reasoning_effort":"minimal","stream":true}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(seenBody, &got); err != nil {
		t.Fatalf("decode upstream body %q: %v", string(seenBody), err)
	}
	if _, present := got["reasoning_effort"]; present {
		t.Errorf("reasoning_effort key present in upstream body %q, want removed entirely", string(seenBody))
	}
	if string(got["model"]) != `"reasoning-alias"` {
		t.Errorf("model field disturbed: got %s", got["model"])
	}
}

// ---------------------------------------------------------------------------
// Endpoint path
// ---------------------------------------------------------------------------

// Requirement 2 (+6, endpoint branch): a mapped value is rewritten, and this
// exercises the combined model+reasoning_effort rewrite pass endpoint routing
// always goes through.
func TestReasoningEffort_EndpointPath_RewritesMappedValue(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "k"}},
	})
	registry.Snapshot(context.Background()) // force a probe so LookupModel sees the endpoint online

	r := NewRelayRouter(":0", nil, registry, nil)
	r.setReasoningEffortMap(map[string]string{"minimal": "none"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"fakeep/upstream-model","reasoning_effort":"minimal"}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var got struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(seenBody, &got); err != nil {
		t.Fatalf("decode upstream body %q: %v", string(seenBody), err)
	}
	if got.Model != "upstream-model" {
		t.Errorf("model: got %q, want bare upstream id", got.Model)
	}
	if got.ReasoningEffort != "none" {
		t.Errorf("reasoning_effort: got %q, want %q", got.ReasoningEffort, "none")
	}
}

// Requirement 4: a value not present in the map passes through untouched.
func TestReasoningEffort_EndpointPath_UnmappedValuePassesThrough(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "k"}},
	})
	registry.Snapshot(context.Background()) // force a probe so LookupModel sees the endpoint online

	r := NewRelayRouter(":0", nil, registry, nil)
	r.setReasoningEffortMap(map[string]string{"minimal": "none"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"fakeep/upstream-model","reasoning_effort":"medium"}`))

	var got struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(seenBody, &got); err != nil {
		t.Fatalf("decode upstream body %q: %v", string(seenBody), err)
	}
	if got.ReasoningEffort != "medium" {
		t.Errorf("reasoning_effort: got %q, want unchanged %q", got.ReasoningEffort, "medium")
	}
}

// Requirement 5: a non-string reasoning_effort is left alone and must not
// error the request.
func TestReasoningEffort_EndpointPath_NonStringValueLeftAlone(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "k"}},
	})
	registry.Snapshot(context.Background()) // force a probe so LookupModel sees the endpoint online

	r := NewRelayRouter(":0", nil, registry, nil)
	r.setReasoningEffortMap(map[string]string{"minimal": "none"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"fakeep/upstream-model","reasoning_effort":5}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("non-string reasoning_effort must not error the request; got %d body=%s", resp.StatusCode, string(body))
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(seenBody, &got); err != nil {
		t.Fatalf("decode upstream body %q: %v", string(seenBody), err)
	}
	if string(got["reasoning_effort"]) != "5" {
		t.Errorf("reasoning_effort: got %s, want untouched 5", got["reasoning_effort"])
	}
}

// Requirement 7: every other field, including a nested object and an array,
// survives the rewrite with its content intact (key order is not preserved —
// json.Marshal of a map sorts keys — so this compares decoded values).
func TestReasoningEffort_EndpointPath_OtherFieldsSurviveContent(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "k"}},
	})
	registry.Snapshot(context.Background()) // force a probe so LookupModel sees the endpoint online

	r := NewRelayRouter(":0", nil, registry, nil)
	r.setReasoningEffortMap(map[string]string{"minimal": "none"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	sent := []byte(`{
		"model": "fakeep/upstream-model",
		"reasoning_effort": "minimal",
		"stream": true,
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": [{"type": "text", "text": "hi"}]}
		],
		"tools": [{"type": "function", "function": {"name": "noop", "parameters": {}}}],
		"metadata": {"nested": {"deep": [1, 2, 3]}}
	}`)
	postBytes(t, srv.URL+"/v1/chat/completions", sent)

	var wantDecoded, gotDecoded map[string]any
	if err := json.Unmarshal(sent, &wantDecoded); err != nil {
		t.Fatalf("decode sent: %v", err)
	}
	if err := json.Unmarshal(seenBody, &gotDecoded); err != nil {
		t.Fatalf("decode upstream body %q: %v", string(seenBody), err)
	}
	// model and reasoning_effort are the only fields the rewrite touches.
	wantDecoded["model"] = "upstream-model"
	wantDecoded["reasoning_effort"] = "none"

	wantJSON, _ := json.Marshal(wantDecoded)
	gotJSON, _ := json.Marshal(gotDecoded)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("upstream body content changed beyond model/reasoning_effort:\n got  %s\n want %s", gotJSON, wantJSON)
	}
}

// ---------------------------------------------------------------------------
// Virtual-model path
// ---------------------------------------------------------------------------

// Requirement 6, virtual branch: a virtual name resolving to an endpoint
// target goes through routeVirtual -> buildVirtualAttempt, a code path
// distinct from routeOpenAI above — the rewrite must apply there too.
func TestReasoningEffort_VirtualModelPath_RewritesMappedValue(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "k"}},
	})
	registry.Snapshot(context.Background()) // force a probe so LookupModel sees the endpoint online

	virtual := &VirtualLLMConfig{Models: []VirtualLLM{{
		Name:    "vCode",
		Targets: []VirtualLLMTarget{{Endpoint: "fakeep", Model: "upstream-model"}},
	}}}
	r := NewRelayRouter(":0", nil, registry, virtual)
	r.setReasoningEffortMap(map[string]string{"minimal": "none"})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"vCode","reasoning_effort":"minimal"}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var got struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(seenBody, &got); err != nil {
		t.Fatalf("decode upstream body %q: %v", string(seenBody), err)
	}
	if got.Model != "upstream-model" {
		t.Errorf("model: got %q, want bare upstream id", got.Model)
	}
	if got.ReasoningEffort != "none" {
		t.Errorf("reasoning_effort: got %q, want %q", got.ReasoningEffort, "none")
	}
}

// ---------------------------------------------------------------------------
// reasoningEffortTemplateKwargs — chat_template_kwargs merge
//
// Coverage for RouterConfig.ReasoningEffortTemplateKwargs / applyReasoningEffortTemplateKwargs
// in relay_router.go. Fixes what the value-map rewrite above cannot: oMLX
// forwards reasoning_effort verbatim into chat_template_kwargs and hands it
// to the model's Jinja template instead of interpreting it server-side, so a
// VALUE swap of reasoning_effort changes nothing for a template that reads
// enable_thinking instead. See RouterConfig's doc comment for the measured
// table (oMLX CodeFast: baseline 101 chars, reasoning_effort:"none" 94
// chars/no effect, chat_template_kwargs:{"enable_thinking":false} 0
// chars/off) and for why both rewrites match against the request's ORIGINAL
// reasoning_effort value rather than the value-map's output.
// ---------------------------------------------------------------------------

// decodeChatTemplateKwargs pulls "chat_template_kwargs" out of an upstream
// body as a generic map, failing the test if it's absent or not an object —
// every test below that calls this expects the key to exist.
func decodeChatTemplateKwargs(t *testing.T, seenBody []byte) map[string]any {
	t.Helper()
	var got struct {
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
	}
	if err := json.Unmarshal(seenBody, &got); err != nil {
		t.Fatalf("decode upstream body %q: %v", string(seenBody), err)
	}
	if got.ChatTemplateKwargs == nil {
		t.Fatalf("chat_template_kwargs missing from upstream body %q", string(seenBody))
	}
	return got.ChatTemplateKwargs
}

// Requirement 1 (template-kwargs half): no config at all, including a body
// that already carries chat_template_kwargs, passes through byte-identical —
// same short-circuit guarantee as the plain reasoning_effort case, now
// proven with a third field (chat_template_kwargs) present in the input.
func TestReasoningEffortTemplateKwargs_NoRouterConfig_BodyUnchanged(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	r := newManagedAliasRouter(t, "reasoning-alias", upstream, nil) // no setters called at all

	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	sent := `{"model":"reasoning-alias","reasoning_effort":"minimal","chat_template_kwargs":{"enable_thinking":true},"stream":true}`
	resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(sent))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if string(seenBody) != sent {
		t.Errorf("upstream saw %q, want byte-identical %q", string(seenBody), sent)
	}
}

// Requirement 2: a matching reasoning_effort merges the configured object
// into chat_template_kwargs, creating the field since the client didn't send
// one.
func TestReasoningEffortTemplateKwargs_ManagedAliasPath_MergesKwargs(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	r := newManagedAliasRouter(t, "reasoning-alias", upstream, nil)
	r.setReasoningEffortTemplateKwargs(map[string]map[string]any{"minimal": {"enable_thinking": false}})

	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"reasoning-alias","reasoning_effort":"minimal"}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	kwargs := decodeChatTemplateKwargs(t, seenBody)
	if got, ok := kwargs["enable_thinking"].(bool); !ok || got != false {
		t.Errorf("chat_template_kwargs.enable_thinking = %v, want false", kwargs["enable_thinking"])
	}
}

// Requirement 3: the client's own chat_template_kwargs.enable_thinking:true
// survives untouched — the merge must not clobber a client-supplied key,
// mirroring oMLX's own merged.setdefault(...) semantics (see RouterConfig).
func TestReasoningEffortTemplateKwargs_ClientValueSurvives(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	r := newManagedAliasRouter(t, "reasoning-alias", upstream, nil)
	r.setReasoningEffortTemplateKwargs(map[string]map[string]any{"minimal": {"enable_thinking": false}})

	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"reasoning-alias","reasoning_effort":"minimal","chat_template_kwargs":{"enable_thinking":true}}`))

	kwargs := decodeChatTemplateKwargs(t, seenBody)
	if got, ok := kwargs["enable_thinking"].(bool); !ok || got != true {
		t.Errorf("chat_template_kwargs.enable_thinking = %v, want client's true to survive", kwargs["enable_thinking"])
	}
}

// Requirement 4: a client chat_template_kwargs with an unrelated key keeps
// that key AND gains ours — the merge is additive, not a replace.
func TestReasoningEffortTemplateKwargs_DifferentKeySurvivesAndOursAdded(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	r := newManagedAliasRouter(t, "reasoning-alias", upstream, nil)
	r.setReasoningEffortTemplateKwargs(map[string]map[string]any{"minimal": {"enable_thinking": false}})

	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"reasoning-alias","reasoning_effort":"minimal","chat_template_kwargs":{"custom_flag":"keep-me"}}`))

	kwargs := decodeChatTemplateKwargs(t, seenBody)
	if kwargs["custom_flag"] != "keep-me" {
		t.Errorf("chat_template_kwargs.custom_flag = %v, want client's %q to survive", kwargs["custom_flag"], "keep-me")
	}
	if got, ok := kwargs["enable_thinking"].(bool); !ok || got != false {
		t.Errorf("chat_template_kwargs.enable_thinking = %v, want ours (false) added", kwargs["enable_thinking"])
	}
}

// Requirement 5 (the ordering regression pin): both knobs configured for the
// same source value must both fire off the client's ORIGINAL
// reasoning_effort ("minimal") — not off "none", what reasoningEffortMap
// rewrites it into. If applyReasoningEffortTemplateKwargs were ever wired to
// read reasoning_effort AFTER applyReasoningEffortMap ran, this would need
// reconfiguring under the key "none" instead and this test would catch the
// silent behavior change.
func TestReasoningEffortTemplateKwargs_BothKnobsFireTogether(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	r := newManagedAliasRouter(t, "reasoning-alias", upstream, map[string]string{"minimal": "none"})
	r.setReasoningEffortTemplateKwargs(map[string]map[string]any{"minimal": {"enable_thinking": false}})

	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"reasoning-alias","reasoning_effort":"minimal"}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var got struct {
		ReasoningEffort    string         `json:"reasoning_effort"`
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
	}
	if err := json.Unmarshal(seenBody, &got); err != nil {
		t.Fatalf("decode upstream body %q: %v", string(seenBody), err)
	}
	if got.ReasoningEffort != "none" {
		t.Errorf("reasoning_effort: got %q, want %q", got.ReasoningEffort, "none")
	}
	if got.ChatTemplateKwargs == nil {
		t.Fatalf("chat_template_kwargs missing from upstream body %q", string(seenBody))
	}
	if enable, ok := got.ChatTemplateKwargs["enable_thinking"].(bool); !ok || enable != false {
		t.Errorf("chat_template_kwargs.enable_thinking = %v, want false", got.ChatTemplateKwargs["enable_thinking"])
	}
}

// Requirement 6 (template-kwargs half): a non-matching reasoning_effort
// value, and a non-string one, must not trigger the merge — chat_template_kwargs
// stays entirely absent in both cases.
func TestReasoningEffortTemplateKwargs_NonMatchingLeavesBodyAlone(t *testing.T) {
	for name, body := range map[string]string{
		"unmapped_value":     `{"model":"reasoning-alias","reasoning_effort":"medium"}`,
		"non_string_value":   `{"model":"reasoning-alias","reasoning_effort":5}`,
		"missing_altogether": `{"model":"reasoning-alias"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var seenBody []byte
			upstream := bodyRecordingUpstream(t, &seenBody)
			r := newManagedAliasRouter(t, "reasoning-alias", upstream, nil)
			r.setReasoningEffortTemplateKwargs(map[string]map[string]any{"minimal": {"enable_thinking": false}})

			srv := httptest.NewServer(r.server.Handler)
			defer srv.Close()

			resp := postBytes(t, srv.URL+"/v1/chat/completions", []byte(body))
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(b))
			}

			var got map[string]json.RawMessage
			if err := json.Unmarshal(seenBody, &got); err != nil {
				t.Fatalf("decode upstream body %q: %v", string(seenBody), err)
			}
			if _, present := got["chat_template_kwargs"]; present {
				t.Errorf("chat_template_kwargs present in upstream body %q, want absent", string(seenBody))
			}
		})
	}
}

// Requirement 7, endpoint branch: the merge must fire on routeOpenAI too, a
// code path distinct from the managed-alias route above.
func TestReasoningEffortTemplateKwargs_EndpointPath_MergesKwargs(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "k"}},
	})
	registry.Snapshot(context.Background()) // force a probe so LookupModel sees the endpoint online

	r := NewRelayRouter(":0", nil, registry, nil)
	r.setReasoningEffortTemplateKwargs(map[string]map[string]any{"minimal": {"enable_thinking": false}})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"fakeep/upstream-model","reasoning_effort":"minimal"}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	kwargs := decodeChatTemplateKwargs(t, seenBody)
	if got, ok := kwargs["enable_thinking"].(bool); !ok || got != false {
		t.Errorf("chat_template_kwargs.enable_thinking = %v, want false", kwargs["enable_thinking"])
	}
}

// Requirement 7, virtual branch: the merge must fire on routeVirtual ->
// buildVirtualAttempt too, a third code path distinct from both above.
func TestReasoningEffortTemplateKwargs_VirtualModelPath_MergesKwargs(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "fakeep", BaseURL: upstream.URL + "/v1", APIKey: "k"}},
	})
	registry.Snapshot(context.Background()) // force a probe so LookupModel sees the endpoint online

	virtual := &VirtualLLMConfig{Models: []VirtualLLM{{
		Name:    "vCode",
		Targets: []VirtualLLMTarget{{Endpoint: "fakeep", Model: "upstream-model"}},
	}}}
	r := NewRelayRouter(":0", nil, registry, virtual)
	r.setReasoningEffortTemplateKwargs(map[string]map[string]any{"minimal": {"enable_thinking": false}})
	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"vCode","reasoning_effort":"minimal"}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	kwargs := decodeChatTemplateKwargs(t, seenBody)
	if got, ok := kwargs["enable_thinking"].(bool); !ok || got != false {
		t.Errorf("chat_template_kwargs.enable_thinking = %v, want false", kwargs["enable_thinking"])
	}
}

// Requirement 8: inner values are arbitrary JSON, not just bools — a string
// and a number must round-trip to the upstream unmodified.
func TestReasoningEffortTemplateKwargs_NonBoolValuesRoundTrip(t *testing.T) {
	var seenBody []byte
	upstream := bodyRecordingUpstream(t, &seenBody)
	r := newManagedAliasRouter(t, "reasoning-alias", upstream, nil)
	r.setReasoningEffortTemplateKwargs(map[string]map[string]any{
		"minimal": {"reasoning_mode": "brief", "reasoning_budget": 128},
	})

	srv := httptest.NewServer(r.server.Handler)
	defer srv.Close()

	resp := postBytes(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"reasoning-alias","reasoning_effort":"minimal"}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	kwargs := decodeChatTemplateKwargs(t, seenBody)
	if kwargs["reasoning_mode"] != "brief" {
		t.Errorf("chat_template_kwargs.reasoning_mode = %v, want %q", kwargs["reasoning_mode"], "brief")
	}
	if got, ok := kwargs["reasoning_budget"].(float64); !ok || got != 128 {
		t.Errorf("chat_template_kwargs.reasoning_budget = %v, want 128", kwargs["reasoning_budget"])
	}
}
