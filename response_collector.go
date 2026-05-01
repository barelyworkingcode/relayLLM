package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ResponseCollector captures a complete LLM response for synchronous HTTP callers.
// Registered via SessionManager.collectors map — does not replace the global sink.
type ResponseCollector struct {
	mu       sync.Mutex
	text     strings.Builder
	stats    SessionStats
	done     chan struct{}
	doneOnce sync.Once
	err      error
}

func NewResponseCollector() *ResponseCollector {
	return &ResponseCollector{
		done: make(chan struct{}),
	}
}

// HandleEvent processes a routed event, capturing text and stats.
func (c *ResponseCollector) HandleEvent(msg map[string]interface{}) {
	msgType, _ := msg["type"].(string)

	switch msgType {
	case "llm_event":
		eventRaw, _ := msg["event"].(json.RawMessage)
		c.extractText(eventRaw)

	case "stats_update":
		if stats, ok := msg["stats"].(SessionStats); ok {
			c.mu.Lock()
			c.stats = stats
			c.mu.Unlock()
		}

	case "message_complete":
		c.doneOnce.Do(func() { close(c.done) })

	case "error":
		errMsg, _ := msg["message"].(string)
		c.err = fmt.Errorf("%s", errMsg)
		c.doneOnce.Do(func() { close(c.done) })

	case "process_exited":
		c.err = fmt.Errorf("provider process exited unexpectedly")
		c.doneOnce.Do(func() { close(c.done) })
	}
}

// extractText pulls user-visible text out of a canonical event. Thinking and
// tool blocks are deliberately excluded — HTTP callers only see the final
// reply text. See docs/event-protocol.md for event shapes.
func (c *ResponseCollector) extractText(eventRaw json.RawMessage) {
	if eventRaw == nil {
		return
	}

	var event struct {
		Type  string `json:"type"`
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
		ContentBlock *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_block"`
		// Claude CLI stream-json: assistant message_start may carry full
		// content array on the first event.
		Message *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}

	if err := json.Unmarshal(eventRaw, &event); err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Canonical: text_delta inside content_block_delta.
	if event.Delta != nil && event.Delta.Type == "text_delta" {
		c.text.WriteString(event.Delta.Text)
		return
	}

	// Claude CLI legacy: text content_block carries inline text.
	if event.ContentBlock != nil && event.ContentBlock.Type == "text" && event.ContentBlock.Text != "" {
		c.text.WriteString(event.ContentBlock.Text)
		return
	}

	// Claude CLI: full message at message_start (rare but possible).
	if event.Type == "assistant" && event.Message != nil {
		for _, block := range event.Message.Content {
			if block.Type == "text" {
				c.text.WriteString(block.Text)
			}
		}
	}
}

// Wait blocks until the response is complete or timeout.
func (c *ResponseCollector) Wait(timeout time.Duration) (string, SessionStats, error) {
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err != nil {
			return "", c.stats, c.err
		}
		return c.text.String(), c.stats, nil
	case <-time.After(timeout):
		return "", SessionStats{}, fmt.Errorf("response timeout after %v", timeout)
	}
}
