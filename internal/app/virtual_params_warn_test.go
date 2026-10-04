package app

import (
	"encoding/json"
	"strings"
	"testing"

	"relayllm/internal/config"
)

// A params target naming an endpoint that does not exist gets one warning
// that says it declares params; a params-free target gets none.
func TestWarnVirtualModelConfig_ParamsOnUnknownEndpoint(t *testing.T) {
	for name, tc := range map[string]struct {
		params json.RawMessage
		want   int
	}{
		"params declared": {json.RawMessage(`{"reasoning_effort":"low"}`), 1},
		"no params":       {nil, 0},
	} {
		t.Run(name, func(t *testing.T) {
			virtual := &config.VirtualLLMConfig{Models: []config.VirtualLLM{{
				Name: "vChat", Targets: []config.VirtualLLMTarget{{Endpoint: "gone", Model: "example-model", Params: tc.params}},
			}}}
			recs := captureSlog(t, func() { warnVirtualModelConfig(virtual, nil, nil) })
			n := 0
			for _, r := range recs {
				if msg, _ := r["msg"].(string); r["level"] == "WARN" && strings.Contains(msg+" "+asString(r), "declares params") {
					n++
				}
			}
			if n != tc.want {
				t.Errorf("'declares params' warnings = %d, want %d: %v", n, tc.want, recs)
			}
		})
	}
}

func asString(r map[string]any) string {
	b, _ := json.Marshal(r)
	return string(b)
}
