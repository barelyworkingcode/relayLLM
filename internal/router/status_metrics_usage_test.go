package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"relayllm/internal/config"
)

// Native Anthropic responses feed the status metrics: each finished request
// carries the usage its response reported, and the process totals sum them.
// ServeHTTP runs synchronously, so the request has ended when it returns.
func TestAnthropicNative_MetricsCountRequestsAndUsage(t *testing.T) {
	sse := newNativeUpstream(t, 200, map[string]string{"Content-Type": "text/event-stream"}, tapSSEStream)
	js := newNativeUpstream(t, 200, map[string]string{"Content-Type": "application/json"}, tapJSONBody)
	r := newAnthropicRouter(t, anthropicUpstreamCfg(t, "https://unused.invalid", map[string]string{
		"claude-stream": "sse/backend-model",
		"claude-json":   "js/backend-model",
	}), nil, nativeRegistry(
		config.OpenAIEndpoint{Name: "sse", BaseURL: sse.srv.URL + "/v1", API: anthropicAPI},
		config.OpenAIEndpoint{Name: "js", BaseURL: js.srv.URL + "/v1", API: anthropicAPI},
	), nil)

	jsonUsage := TokenUsage{InputTokens: 7, OutputTokens: 9, CacheReadInputTokens: 300, CacheCreationInputTokens: 2}
	for _, model := range []string{"claude-stream", "claude-json"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"`+model+`","max_tokens":8,"stream":`+fmt.Sprint(model == "claude-stream")+`,"messages":[]}`))
		r.server.Handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: status = %d, body = %s", model, rec.Code, rec.Body)
		}
	}

	active, agg, recent := r.Metrics().Snapshot()
	if len(active) != 0 {
		t.Errorf("active = %+v, want none after both requests ended", active)
	}
	if agg.TotalRequests != 2 || len(recent) != 2 {
		t.Fatalf("TotalRequests = %d, recent = %d, want 2 and 2", agg.TotalRequests, len(recent))
	}
	// Order-free: each request's row carries its own response's usage.
	got := map[TokenUsage]int{}
	for _, rr := range recent {
		if rr.Usage == nil {
			t.Fatalf("recent row %+v has no usage", rr)
		}
		got[*rr.Usage]++
	}
	if got[tapSSEUsage] != 1 || got[jsonUsage] != 1 {
		t.Errorf("recent usages = %+v, want one %+v and one %+v", got, tapSSEUsage, jsonUsage)
	}
	wantTotal := TokenUsage{
		InputTokens:              tapSSEUsage.InputTokens + jsonUsage.InputTokens,
		OutputTokens:             tapSSEUsage.OutputTokens + jsonUsage.OutputTokens,
		CacheReadInputTokens:     tapSSEUsage.CacheReadInputTokens + jsonUsage.CacheReadInputTokens,
		CacheCreationInputTokens: tapSSEUsage.CacheCreationInputTokens + jsonUsage.CacheCreationInputTokens,
	}
	if agg.TotalUsage == nil || *agg.TotalUsage != wantTotal {
		t.Errorf("TotalUsage = %+v, want %+v", agg.TotalUsage, wantTotal)
	}
}
