package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"relayllm/internal/config"
	"relayllm/internal/servermanager"
)

// captureSlog runs fn with the default logger writing JSON records, and
// returns the records it logged.
func captureSlog(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)
	fn()
	var recs []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

func dialectManager(alias string, api []string) *servermanager.ServerManager {
	return servermanager.NewServerManager(servermanager.LlamaProfile, &config.ServerConfig{
		API: api, Models: []config.ServerModelConfig{{Alias: alias}},
	}, "")
}

func TestLogAnthropicDialects_WarnsWhenDeclaredButRouterAnthropicAbsent(t *testing.T) {
	native := []config.OpenAIEndpoint{{Name: "acme", API: []string{config.APIAnthropic}}}
	for name, tc := range map[string]struct {
		managers  []*servermanager.ServerManager
		endpoints []config.OpenAIEndpoint
		wantWarns int
	}{
		"endpoint declares":        {nil, native, 1},
		"managed section declares": {[]*servermanager.ServerManager{dialectManager("m", []string{config.APIAnthropic})}, nil, 1},
		"nothing declares":         {[]*servermanager.ServerManager{dialectManager("m", nil)}, []config.OpenAIEndpoint{{Name: "acme"}}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			recs := captureSlog(t, func() { logAnthropicDialects(nil, tc.managers, tc.endpoints, nil) })
			warns := 0
			for _, r := range recs {
				if r["level"] == "WARN" {
					warns++
				}
			}
			if warns != tc.wantWarns {
				t.Errorf("WARN records = %d, want %d: %v", warns, tc.wantWarns, recs)
			}
		})
	}
}

func TestLogAnthropicDialects_ReportsDialectPerModelMapKey(t *testing.T) {
	anth := []string{config.APIAnthropic}
	managers := []*servermanager.ServerManager{dialectManager("local-native", anth)}
	managers = append(managers, servermanager.NewServerManager(servermanager.MlxProfile, &config.ServerConfig{
		Models: []config.ServerModelConfig{{Alias: "local-plain"}},
	}, ""))
	endpoints := []config.OpenAIEndpoint{
		{Name: "nat", API: anth},
		{Name: "oai", API: []string{config.APIOpenAI}},
	}
	virtual := &config.VirtualLLMConfig{Models: []config.VirtualLLM{
		{Name: "vAll", Targets: []config.VirtualLLMTarget{{Endpoint: "nat", Model: "m"}, {Alias: "local-native"}}},
		{Name: "vMixed", Targets: []config.VirtualLLMTarget{{Endpoint: "nat", Model: "m"}, {Endpoint: "oai", Model: "m"}}},
	}}
	anthropic := &config.AnthropicRouterConfig{ModelMap: map[string]string{
		"k-managed-native": "local-native",
		"k-managed-plain":  "local-plain",
		"k-endpoint-nat":   "nat/m",
		"k-endpoint-oai":   "oai/m",
		"k-virtual-all":    "vAll",
		"k-virtual-mixed":  "vMixed",
	}}
	want := map[string]string{
		"k-managed-native": "native", "k-managed-plain": "translated",
		"k-endpoint-nat": "native", "k-endpoint-oai": "translated",
		"k-virtual-all": "native", "k-virtual-mixed": "translated",
	}

	recs := captureSlog(t, func() { logAnthropicDialects(anthropic, managers, endpoints, virtual) })
	got := map[string]string{}
	var mixedWarns []any
	for _, r := range recs {
		key, _ := r["key"].(string)
		switch r["level"] {
		case "INFO":
			if d, ok := r["dialect"].(string); ok {
				if _, dup := got[key]; dup {
					t.Errorf("dialect logged twice for %q", key)
				}
				got[key] = d
			}
		case "WARN":
			mixedWarns = append(mixedWarns, key)
		}
	}
	for key, d := range want {
		if got[key] != d {
			t.Errorf("dialect for %s = %q, want %q", key, got[key], d)
		}
	}
	if len(mixedWarns) != 1 || mixedWarns[0] != "k-virtual-mixed" {
		t.Errorf("WARN keys = %v, want exactly [k-virtual-mixed]", mixedWarns)
	}
}
