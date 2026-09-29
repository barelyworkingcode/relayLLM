package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"relayllm/internal/config"
	"relayllm/internal/registry"
	"relayllm/internal/router"
)

// A native Anthropic exchange shows up in /api/status/detailed: the request
// is counted, its recent-request row carries the merged usage, and
// overview.throughput.totalUsage reports the process total.
func TestDetailedStatus_NativeAnthropicUsage(t *testing.T) {
	const stream = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":12,"cache_read_input_tokens":900,"cache_creation_input_tokens":40,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":27}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	defer up.Close()

	ep := config.OpenAIEndpoint{Name: "acme", BaseURL: up.URL + "/v1", API: []string{config.APIAnthropic}}
	reg := registry.NewProxyRegistry(&config.OpenAIConfig{Endpoints: []config.OpenAIEndpoint{ep}})
	reg.SetStatusForTest(ep, true, registry.UpstreamModel{ID: "backend-model"})
	rtr := router.BuildRelayRouter(nil, reg, nil, &config.RouterConfig{Anthropic: &config.AnthropicRouterConfig{
		ModelMap: map[string]string{"claude-haiku-4-5": "acme/backend-model"},
	}}, "", "")

	rec := httptest.NewRecorder()
	rtr.SocketHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-haiku-4-5","max_tokens":8,"stream":true,"messages":[]}`)))
	if rec.Code != 200 || rec.Body.String() != stream {
		t.Fatalf("native exchange: status %d body %q", rec.Code, rec.Body)
	}

	// Round-trip through JSON: the wire shape is what a client sees.
	raw, err := json.Marshal(buildDetailedStatus(context.Background(), DetailedStatusDeps{Router: rtr, StartTime: time.Now()}))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Overview struct {
			Throughput struct {
				TotalRequests float64        `json:"totalRequests"`
				TotalUsage    map[string]any `json:"totalUsage"`
			} `json:"throughput"`
		} `json:"overview"`
		RecentRequests []struct {
			Usage map[string]any `json:"usage"`
		} `json:"recentRequests"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"inputTokens": 12.0, "outputTokens": 27.0, "cacheReadInputTokens": 900.0, "cacheCreationInputTokens": 40.0}
	if got.Overview.Throughput.TotalRequests != 1 {
		t.Errorf("totalRequests = %v, want 1", got.Overview.Throughput.TotalRequests)
	}
	if !reflect.DeepEqual(got.Overview.Throughput.TotalUsage, want) {
		t.Errorf("totalUsage = %v, want %v", got.Overview.Throughput.TotalUsage, want)
	}
	if len(got.RecentRequests) != 1 || !reflect.DeepEqual(got.RecentRequests[0].Usage, want) {
		t.Errorf("recentRequests = %+v, want one row with usage %v", got.RecentRequests, want)
	}
}
