package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func virtualSettings(params string) string {
	p := ""
	if params != "" {
		p = `, "params": ` + params
	}
	return `{"virtual-llms": {"models": [{"name": "vChat", "targets": [
		{"endpoint": "gpubox", "model": "example-model"` + p + `}]}]}}`
}

// Criteria 1, 8, 9: params load as declared; a non-object fails the load and
// names the virtual model, the target and the JSON type found.
func TestLoadConfig_VirtualParams(t *testing.T) {
	good := map[string]bool{ // params text -> expected to be kept as declared
		`{"chat_template_kwargs": {"enable_thinking": false}, "reasoning_effort": "medium"}`: true,
		``:     false,
		`null`: false,
		`{}`:   false,
	}
	for params, kept := range good {
		cfg, err := loadFrom(t, map[string]string{"settings.json": virtualSettings(params)})
		if err != nil {
			t.Fatalf("params %q: load: %v", params, err)
		}
		got := cfg.Virtual.Models[0].Targets[0].Params
		if kept {
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil || m["reasoning_effort"] != "medium" {
				t.Errorf("params %q: Params = %s, want the declared object", params, got)
			}
		}
	}

	bad := map[string]string{`"low"`: "string", `5`: "number", `true`: "boolean", `["x"]`: "array"}
	for params, typ := range bad {
		_, err := loadFrom(t, map[string]string{"settings.json": virtualSettings(params)})
		want := `virtual-llms "vChat" targets[0] (endpoint "gpubox" model "example-model"): params must be a JSON object, got ` + typ
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("params %s: err = %v, want it to contain %q", params, err, want)
		}
	}
}
