package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// stopTestEndpoint is the OMLX server used for stop-generation tests.
// The model is large enough to produce a slow, multi-chunk stream that
// gives us a window to call StopGeneration mid-flight.
var stopTestEndpoint = OpenAIEndpoint{
	Name:    "omlx",
	BaseURL: integOMLXURL,
	APIKey:  integOMLXKey,
}

const stopTestModel = "gemma-4-31b-it-mxfp8"

// skipIfOMLXUnavailable skips the test if the OMLX server is not reachable
// or the target model is not loaded.
func skipIfOMLXUnavailable(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, stopTestEndpoint.BaseURL+"/models", nil)
	if stopTestEndpoint.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+stopTestEndpoint.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("OMLX not reachable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("OMLX /models returned %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Skipf("OMLX: decode /models: %v", err)
	}
	found := false
	for _, m := range payload.Data {
		if strings.Contains(m.ID, stopTestModel) {
			found = true
			break
		}
	}
	if !found {
		t.Skipf("model %s not found on OMLX server", stopTestModel)
	}
}

// stopCapture extends capturingHandler with stop-specific tracking:
// counts events received after stop, and records message_complete arrivals.
type stopCapture struct {
	mu sync.Mutex

	text           strings.Builder
	stats          SessionStats
	gotComplete    chan struct{}
	completeCount  int
	gotError       string
	stopped        bool   // set to true when StopGeneration is called
	eventsAfterStopcnt int // events received after stop flag was set
	textAfterStop  strings.Builder
}

func newStopCapture() *stopCapture {
	return &stopCapture{gotComplete: make(chan struct{}, 4)}
}

func (c *stopCapture) handle(eventType string, data json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped && eventType == "llm_event" {
		c.eventsAfterStopcnt++
		var e struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &e) == nil && e.Delta.Type == "text_delta" {
			c.textAfterStop.WriteString(e.Delta.Text)
		}
	}

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
		c.completeCount++
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

func (c *stopCapture) markStopped() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
}

func (c *stopCapture) getEventsAfterStop() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.eventsAfterStopcnt
}

func (c *stopCapture) getText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}

func (c *stopCapture) getTextAfterStop() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.textAfterStop.String()
}

func (c *stopCapture) getCompleteCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.completeCount
}

func (c *stopCapture) getError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gotError
}

// newStopTestProvider creates a BaseChatProvider wired to the OMLX endpoint
// with a pre-seeded user message. Returns the provider and capture handler.
func newStopTestProvider(t *testing.T, prompt string) (*BaseChatProvider, *stopCapture) {
	t.Helper()

	session := &Session{
		ID:           "stop-test",
		Model:        stopTestModel,
		ProviderType: "openai",
		Messages:     []Message{},
	}
	userContent, _ := json.Marshal(prompt)
	session.Messages = append(session.Messages, Message{
		Timestamp: timeNow(),
		Role:      "user",
		Content:   userContent,
	})

	cap := newStopCapture()
	transport := NewOpenAIChatTransport(stopTestEndpoint, stopTestModel, nil, nil)
	provider := NewBaseChatProvider(session, cap.handle, transport, nil, nil)

	if err := provider.Start(); err != nil {
		t.Fatalf("provider start: %v", err)
	}
	return provider, cap
}

// TestStopGeneration_StopsStream verifies that StopGeneration:
// 1. Causes the stream to end promptly (no events keep flowing)
// 2. Does not emit a "context canceled" error to the client
func TestStopGeneration_StopsStream(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	skipIfOMLXUnavailable(t)

	provider, cap := newStopTestProvider(t,
		"Write a very long, detailed essay about the history of mathematics. "+
			"Include every major mathematician and their contributions. "+
			"Make it at least 2000 words.")
	defer provider.Kill()

	if err := provider.SendMessage(
		"Write a very long, detailed essay about the history of mathematics. "+
			"Include every major mathematician and their contributions. "+
			"Make it at least 2000 words.", nil); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Wait for some text to arrive so we know the stream is active.
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for initial text")
		case <-time.After(100 * time.Millisecond):
			if len(cap.getText()) > 50 {
				goto gotText
			}
		}
	}
gotText:
	textBeforeStop := cap.getText()
	t.Logf("text before stop (%d chars): %q", len(textBeforeStop), truncStr(textBeforeStop, 200))

	// Stop generation and mark the capture so we can track post-stop events.
	cap.markStopped()
	provider.StopGeneration()

	// Give some time for any straggler events to arrive.
	time.Sleep(500 * time.Millisecond)

	eventsAfter := cap.getEventsAfterStop()
	textAfter := cap.getTextAfterStop()
	t.Logf("events after stop: %d, text after stop: %q", eventsAfter, truncStr(textAfter, 200))

	// The generation counter should have suppressed all post-stop events.
	if eventsAfter > 0 {
		t.Errorf("expected 0 events after stop, got %d (text: %q)", eventsAfter, truncStr(textAfter, 200))
	}

	// Should NOT have a "context canceled" error.
	if errMsg := cap.getError(); errMsg != "" {
		t.Errorf("unexpected error after stop: %s", errMsg)
	}
}

// TestStopGeneration_NoBleed verifies that stopping one generation and
// immediately starting another produces a clean response with no text
// from the first generation bleeding in.
func TestStopGeneration_NoBleed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	skipIfOMLXUnavailable(t)

	// Use a session directly so we can drive two SendMessage calls.
	session := &Session{
		ID:           "stop-bleed-test",
		Model:        stopTestModel,
		ProviderType: "openai",
		Messages:     []Message{},
	}

	cap := newStopCapture()
	transport := NewOpenAIChatTransport(stopTestEndpoint, stopTestModel, nil, nil)
	provider := NewBaseChatProvider(session, cap.handle, transport, nil, nil)
	if err := provider.Start(); err != nil {
		t.Fatalf("provider start: %v", err)
	}
	defer provider.Kill()

	// --- Turn 1: ask about ELEPHANTS, stop mid-stream ---
	prompt1 := "Write a very long essay about elephants. " +
		"Mention the word ELEPHANT in every sentence. Make it at least 2000 words."
	userContent1, _ := json.Marshal(prompt1)
	session.mu.Lock()
	session.Messages = append(session.Messages, Message{
		Timestamp: timeNow(), Role: "user", Content: userContent1,
	})
	session.mu.Unlock()

	if err := provider.SendMessage(prompt1, nil); err != nil {
		t.Fatalf("send turn 1: %v", err)
	}

	// Wait for some elephant text.
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for turn 1 text")
		case <-time.After(100 * time.Millisecond):
			if len(cap.getText()) > 100 {
				goto gotTurn1
			}
		}
	}
gotTurn1:
	turn1Text := cap.getText()
	t.Logf("turn 1 text (%d chars): %q", len(turn1Text), truncStr(turn1Text, 200))

	// Verify turn 1 was actually about elephants.
	if !strings.Contains(strings.ToLower(turn1Text), "elephant") {
		t.Logf("WARNING: turn 1 text doesn't contain 'elephant' — model may not have followed instructions")
	}

	// Stop turn 1.
	provider.StopGeneration()
	// Brief pause to let goroutine exit.
	time.Sleep(200 * time.Millisecond)

	// --- Turn 2: ask about PENGUINS, verify no elephant bleed ---
	// Reset capture for turn 2.
	cap2 := newStopCapture()
	provider.handler = cap2.handle

	prompt2 := "What is 2 + 2? Reply with ONLY the number, nothing else."
	userContent2, _ := json.Marshal(prompt2)
	session.mu.Lock()
	session.Messages = append(session.Messages, Message{
		Timestamp: timeNow(), Role: "user", Content: userContent2,
	})
	session.mu.Unlock()

	if err := provider.SendMessage(prompt2, nil); err != nil {
		t.Fatalf("send turn 2: %v", err)
	}

	select {
	case <-cap2.gotComplete:
	case <-time.After(60 * time.Second):
		t.Fatal("timed out waiting for turn 2 message_complete")
	}

	turn2Text := cap2.getText()
	t.Logf("turn 2 response: %q", turn2Text)

	// Turn 2 should contain "4" and NOT contain elephant content.
	if !strings.Contains(turn2Text, "4") {
		t.Errorf("turn 2: expected '4' in response, got: %q", turn2Text)
	}
	if strings.Contains(strings.ToLower(turn2Text), "elephant") {
		t.Errorf("turn 2: elephant text bled into response: %q", truncStr(turn2Text, 500))
	}

	// Should have exactly 1 message_complete (from turn 2 only).
	if n := cap2.getCompleteCount(); n != 1 {
		t.Errorf("expected 1 message_complete for turn 2, got %d", n)
	}

	// No error events.
	if errMsg := cap2.getError(); errMsg != "" {
		t.Errorf("unexpected error in turn 2: %s", errMsg)
	}
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
