package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// These integration tests exercise the full BaseChatProvider + OpenAIChatTransport
// path against real local servers. They are skipped with `go test -short` and
// individually skipped with t.Skip when the target server is unreachable.
//
// Servers expected to be running:
//   - Ollama         : http://localhost:11434/v1 (no auth)
//   - LM Studio      : http://localhost:1234/v1  (api key)
//   - OMLX / mlx     : http://localhost:8000/v1  (api key)
//
// Endpoints that are down are skipped cleanly — running only Ollama locally
// does not cause the LM Studio / OMLX tests to fail.

const (
	integOllamaURL   = "http://localhost:11434/v1"
	integLMStudioURL = "http://localhost:1234/v1"
	integOMLXURL     = "http://localhost:8000/v1"

	// Local dev API keys provided by the user. These are not production
	// secrets — they are personal local-server tokens.
	integLMStudioKey = "sk-lm-yyIMFoXZ:KFsWdHRu2lFuc5ayeS1g"
	integOMLXKey     = "omlx-sygde9eyq9mc0fvx"
)

// capturingHandler collects all events emitted by BaseChatProvider so the
// test can make assertions on them after message_complete fires.
type capturingHandler struct {
	mu          sync.Mutex
	text        strings.Builder
	stats       SessionStats
	gotComplete chan struct{}
	gotError    string
}

func newCapturingHandler() *capturingHandler {
	return &capturingHandler{gotComplete: make(chan struct{}, 1)}
}

func (c *capturingHandler) handle(eventType string, data json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch eventType {
	case "llm_event":
		var e struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(data, &e); err == nil && e.Delta.Type == "text_delta" {
			c.text.WriteString(e.Delta.Text)
		}
	case "stats_update":
		_ = json.Unmarshal(data, &c.stats)
	case "message_complete":
		select {
		case c.gotComplete <- struct{}{}:
		default:
		}
	case "error":
		var e struct{ Error string }
		_ = json.Unmarshal(data, &e)
		c.gotError = e.Error
		select {
		case c.gotComplete <- struct{}{}:
		default:
		}
	}
}

// runChatRoundtrip is the shared helper all three integration tests use.
// It pings /models to decide whether the server is available, picks a model
// (preferring names matching modelFilter), and drives a short chat turn
// through BaseChatProvider end-to-end.
func runChatRoundtrip(t *testing.T, endpoint OpenAIEndpoint, modelFilter string) {
	t.Helper()

	// Pick a model from /v1/models, using a short timeout so an offline
	// server skips fast instead of stalling the suite.
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, endpoint.BaseURL+"/models", nil)
	if err != nil {
		t.Fatalf("build models req: %v", err)
	}
	if endpoint.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("%s: not reachable at %s: %v", endpoint.Name, endpoint.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("%s: /models returned %d", endpoint.Name, resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Skipf("%s: decode /models: %v", endpoint.Name, err)
	}
	if len(payload.Data) == 0 {
		t.Skipf("%s: no models available", endpoint.Name)
	}

	modelID := payload.Data[0].ID
	if modelFilter != "" {
		for _, m := range payload.Data {
			if strings.Contains(strings.ToLower(m.ID), strings.ToLower(modelFilter)) {
				modelID = m.ID
				break
			}
		}
	}
	t.Logf("%s: using model %s", endpoint.Name, modelID)

	// Build a minimal session + provider by hand — no SessionManager or
	// HTTP server needed. This keeps the test focused on the provider stack.
	session := &Session{
		ID:           "test-" + endpoint.Name,
		Model:        modelID,
		ProviderType: "openai",
		Messages:     []Message{},
	}
	// Seed the user message into session history the same way the session
	// layer does before calling SendMessage.
	userContent, _ := json.Marshal("Say hi in exactly three words.")
	session.Messages = append(session.Messages, Message{
		Timestamp: timeNow(),
		Role:      "user",
		Content:   userContent,
	})

	cap := newCapturingHandler()
	transport := NewOpenAIChatTransport(endpoint, modelID, nil, nil)
	provider := NewBaseChatProvider(session, cap.handle, transport, nil, nil)

	if err := provider.Start(); err != nil {
		t.Fatalf("%s: provider start: %v", endpoint.Name, err)
	}
	defer provider.Kill()

	if err := provider.SendMessage("Say hi in exactly three words.", nil); err != nil {
		t.Fatalf("%s: send: %v", endpoint.Name, err)
	}

	// Wait for completion with a generous timeout — local models on CPU
	// can take a while to respond.
	select {
	case <-cap.gotComplete:
	case <-time.After(120 * time.Second):
		t.Fatalf("%s: timed out waiting for message_complete", endpoint.Name)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()

	if cap.gotError != "" {
		t.Fatalf("%s: error event: %s", endpoint.Name, cap.gotError)
	}
	text := cap.text.String()
	if strings.TrimSpace(text) == "" {
		t.Fatalf("%s: empty response text", endpoint.Name)
	}
	t.Logf("%s: response: %q", endpoint.Name, text)
	t.Logf("%s: stats: in=%d out=%d ttft=%.2fs tps=%.1f",
		endpoint.Name, cap.stats.InputTokens, cap.stats.OutputTokens,
		cap.stats.TimeToFirstToken, cap.stats.TokensPerSecond)

	// Token counts are a soft assertion: some servers (notably Ollama's
	// /v1 compat layer) may not populate usage in streaming responses.
	// We warn rather than fail when they're zero.
	if cap.stats.OutputTokens == 0 {
		t.Logf("%s: WARNING output tokens = 0 (server may not support stream_options.include_usage)", endpoint.Name)
	}
}

func TestIntegrationOpenAI_Ollama(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	runChatRoundtrip(t, OpenAIEndpoint{
		Name:    "ollama-oai",
		BaseURL: integOllamaURL,
	}, "qwen")
}

func TestIntegrationOpenAI_LMStudio(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	runChatRoundtrip(t, OpenAIEndpoint{
		Name:    "lmstudio",
		BaseURL: integLMStudioURL,
		APIKey:  integLMStudioKey,
	}, "qwen")
}

func TestIntegrationOpenAI_OMLX(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	runChatRoundtrip(t, OpenAIEndpoint{
		Name:    "omlx",
		BaseURL: integOMLXURL,
		APIKey:  integOMLXKey,
	}, "gemma-3") // small known-working model; first model in list may be broken
}
