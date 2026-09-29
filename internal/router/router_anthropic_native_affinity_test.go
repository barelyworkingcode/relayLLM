package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"relayllm/internal/config"
	regpkg "relayllm/internal/registry"
)

// nativeAffinityRig is an all-native virtual "vNative" over endpoints a then
// b (declared in that order), reached through a modelMap key.
type nativeAffinityRig struct {
	srv      *httptest.Server
	reg      *regpkg.ProxyRegistry
	a, b     *nativeUpstream
	epA, epB config.OpenAIEndpoint
}

const (
	nativeAffinityBodyA = `{"type":"message","id":"msg_a","content":[]}`
	nativeAffinityBodyB = `{"type":"message","id":"msg_b","content":[]}`
)

func newNativeAffinityRig(t *testing.T, aStatus int, aBody string) *nativeAffinityRig {
	t.Helper()
	hdr := map[string]string{"Content-Type": "application/json"}
	g := &nativeAffinityRig{
		a: newNativeUpstream(t, aStatus, hdr, aBody),
		b: newNativeUpstream(t, http.StatusOK, hdr, nativeAffinityBodyB),
	}
	g.epA = config.OpenAIEndpoint{Name: "a", BaseURL: g.a.srv.URL + "/v1", API: anthropicAPI}
	g.epB = config.OpenAIEndpoint{Name: "b", BaseURL: g.b.srv.URL + "/v1", API: anthropicAPI}
	g.reg = nativeRegistry(g.epA, g.epB)
	virtual := &config.VirtualLLMConfig{Models: []config.VirtualLLM{{Name: "vNative", Targets: []config.VirtualLLMTarget{
		{Endpoint: "a", Model: "backend-model"}, {Endpoint: "b", Model: "backend-model"},
	}}}}
	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid",
		map[string]string{"claude-haiku-4-5": "vNative"}), nil, g.reg, virtual)
	g.srv = httptest.NewServer(r.server.Handler)
	t.Cleanup(g.srv.Close)
	return g
}

// turn sends one request for conversation conv (Claude Code's metadata.user_id
// shape) and reports which upstream received it. Exactly one native
// /v1/messages hit is required, so a silent fall back to translation (which
// would hit /chat/completions) fails here rather than passing by accident.
func (g *nativeAffinityRig) turn(t *testing.T, conv string) (served string, status int, body []byte) {
	t.Helper()
	beforeA, beforeB := len(g.a.recorded()), len(g.b.recorded())
	req := fmt.Sprintf(`{"model":"claude-haiku-4-5","max_tokens":8,"metadata":{"user_id":"{\"session_id\":\"%s\"}"},"messages":[{"role":"user","content":"hi"}]}`, conv)
	resp, body := sendAnthropic(t, g.srv.URL+"/v1/messages", req, nil)
	hitsA, hitsB := g.a.recorded()[beforeA:], g.b.recorded()[beforeB:]
	if len(hitsA)+len(hitsB) != 1 {
		t.Fatalf("conversation %s: upstream hits a=%d b=%d, want exactly one", conv, len(hitsA), len(hitsB))
	}
	hit, served := append(hitsA, hitsB...)[0], "a"
	if len(hitsB) == 1 {
		served = "b"
	}
	if hit.path != "/v1/messages" {
		t.Fatalf("conversation %s: upstream path = %q, want the native /v1/messages", conv, hit.path)
	}
	return served, resp.StatusCode, body
}

func (g *nativeAffinityRig) wantServed(t *testing.T, conv, want, why string) {
	t.Helper()
	served, status, body := g.turn(t, conv)
	if served != want {
		t.Fatalf("conversation %s served by %s, want %s: %s", conv, served, want, why)
	}
	wantBody := map[string]string{"a": nativeAffinityBodyA, "b": nativeAffinityBodyB}[want]
	if status != http.StatusOK || string(body) != wantBody {
		t.Fatalf("conversation %s: status %d body %s, want 200 %s", conv, status, body, wantBody)
	}
}

func TestAnthropicNativeAffinity_PinBeatsReachability(t *testing.T) {
	g := newNativeAffinityRig(t, http.StatusOK, nativeAffinityBodyA)
	g.wantServed(t, "conv-x", "a", "both online, a declared first")

	// a drops to last resort by reachability; b is now preferred.
	g.reg.SetStatusForTest(g.epA, false)
	g.wantServed(t, "conv-x", "a", "the pin must beat reachability ordering")
	g.wantServed(t, "conv-y", "b", "a new conversation follows reachability")
}

func TestAnthropicNativeAffinity_PinsForwardAfterPreResponseFailover(t *testing.T) {
	g := newNativeAffinityRig(t, http.StatusOK, nativeAffinityBodyA)
	g.wantServed(t, "conv-z", "a", "both online, a declared first")

	// a still looks online but refuses connections.
	deadA := g.epA
	deadA.BaseURL = "http://127.0.0.1:1/v1"
	g.reg.SetStatusForTest(deadA, true, regpkg.UpstreamModel{ID: "backend-model"})
	g.wantServed(t, "conv-z", "b", "a failed before responding, so b takes over")

	// a recovers and is preferred again by declared order.
	g.reg.SetStatusForTest(g.epA, true, regpkg.UpstreamModel{ID: "backend-model"})
	g.wantServed(t, "conv-z", "b", "the pin moved forward to b and must not flap back")
}

func TestAnthropicNativeAffinity_5xxPassesThroughUnpinned(t *testing.T) {
	const boom = `{"type":"error","error":{"type":"api_error","message":"boom <x>"}}`
	g := newNativeAffinityRig(t, http.StatusInternalServerError, boom)

	served, status, body := g.turn(t, "conv-w")
	if served != "a" || status != http.StatusInternalServerError || string(body) != boom {
		t.Fatalf("served by %s, status %d, body %s; want a's 500 relayed unchanged: %s", served, status, body, boom)
	}

	// A pin on a would put it first again; with none, b leads by reachability.
	g.reg.SetStatusForTest(g.epA, false)
	g.wantServed(t, "conv-w", "b", "a 5xx must not pin the conversation")
}
