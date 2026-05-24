package main

// Coverage for OllamaChatTransport.buildChatBody — the body Ollama actually
// sees. The think flag must be explicit on every request (memory says
// omitting it is not equivalent to false on thinking-capable models like
// Gemma 4). These tests pin that contract.

import (
	"encoding/json"
	"testing"
)

func newOllamaChatTransportForTest(t *testing.T, settingsJSON string) *OllamaChatTransport {
	t.Helper()
	var raw json.RawMessage
	if settingsJSON != "" {
		raw = json.RawMessage(settingsJSON)
	}
	return NewOllamaChatTransport("http://test", "gemma-4:latest", raw, nil)
}

func TestOllama_BuildChatBody_AlwaysIncludesThinkFlag_FalseByDefault(t *testing.T) {
	tr := newOllamaChatTransportForTest(t, "")
	body := tr.buildChatBody(nil, nil)

	think, present := body["think"]
	if !present {
		t.Fatalf("think key MUST be present even when no setting; got body=%v", body)
	}
	if v, ok := think.(bool); !ok || v {
		t.Errorf("default think: got %v, want false", think)
	}
}

func TestOllama_BuildChatBody_ThinkExplicitlyFalse(t *testing.T) {
	tr := newOllamaChatTransportForTest(t, `{"think": false}`)
	body := tr.buildChatBody(nil, nil)
	if v, _ := body["think"].(bool); v {
		t.Errorf("think with explicit false setting: got true")
	}
}

func TestOllama_BuildChatBody_ThinkExplicitlyTrue(t *testing.T) {
	tr := newOllamaChatTransportForTest(t, `{"think": true}`)
	body := tr.buildChatBody(nil, nil)
	if v, _ := body["think"].(bool); !v {
		t.Errorf("think with explicit true setting: got false")
	}
}

func TestOllama_BuildChatBody_ToolsIncludedWhenProvided(t *testing.T) {
	tr := newOllamaChatTransportForTest(t, "")
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "ping"}},
	}
	body := tr.buildChatBody(nil, tools)
	if _, ok := body["tools"]; !ok {
		t.Errorf("tools key missing when tools provided: %v", body)
	}
}

func TestOllama_BuildChatBody_ToolsOmittedWhenEmpty(t *testing.T) {
	tr := newOllamaChatTransportForTest(t, "")
	body := tr.buildChatBody(nil, nil)
	if _, ok := body["tools"]; ok {
		t.Errorf("tools key should be omitted when empty: %v", body["tools"])
	}
}

func TestOllama_BuildChatBody_StreamAndKeepAliveAlwaysSet(t *testing.T) {
	tr := newOllamaChatTransportForTest(t, "")
	body := tr.buildChatBody(nil, nil)
	if v, _ := body["stream"].(bool); !v {
		t.Error("stream should always be true")
	}
	if v, _ := body["keep_alive"].(string); v == "" {
		t.Errorf("keep_alive should be set; got %q", v)
	}
}

func TestOllama_BuildChatBody_OptionsIncludedWhenSet(t *testing.T) {
	tr := newOllamaChatTransportForTest(t, `{"temperature": 0.7, "top_k": 40, "min_p": 0.05}`)
	body := tr.buildChatBody(nil, nil)
	opts, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("options missing or wrong type: %v", body["options"])
	}
	if v, _ := opts["temperature"].(float64); v != 0.7 {
		t.Errorf("temperature: got %v, want 0.7", opts["temperature"])
	}
	if v, _ := opts["top_k"].(int); v != 40 {
		t.Errorf("top_k: got %v, want 40", opts["top_k"])
	}
	if v, _ := opts["min_p"].(float64); v != 0.05 {
		t.Errorf("min_p: got %v, want 0.05", opts["min_p"])
	}
}

func TestOllama_BuildChatBody_OptionsOmittedWhenAllNil(t *testing.T) {
	tr := newOllamaChatTransportForTest(t, "")
	body := tr.buildChatBody(nil, nil)
	if _, ok := body["options"]; ok {
		t.Errorf("options key should be omitted when no settings set; got %v", body["options"])
	}
}

// Serialization sanity: the body must marshal cleanly with the think flag
// present in the JSON output (proof against accidental json:"omitempty" if
// someone refactors the schema).
func TestOllama_BuildChatBody_ThinkFlagSurvivesJSON(t *testing.T) {
	tr := newOllamaChatTransportForTest(t, "")
	body := tr.buildChatBody(nil, nil)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["think"]; !ok {
		t.Errorf("think key lost in JSON round-trip: %s", string(encoded))
	}
}
