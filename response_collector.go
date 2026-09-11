package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrResponseTimeout is returned by Wait when the timeout elapses before the
// provider signals completion. Callers use errors.Is to distinguish this
// from an error the provider itself reported, since only a timeout leaves
// the provider still generating with nothing left waiting on it.
var ErrResponseTimeout = errors.New("response timeout")

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
	case HandlerLLMEvent:
		eventRaw, _ := msg["event"].(json.RawMessage)
		c.extractText(eventRaw)

	case HandlerStatsUpdate:
		if stats, ok := msg["stats"].(SessionStats); ok {
			c.mu.Lock()
			c.stats = stats
			c.mu.Unlock()
		}

	case HandlerMessageComplete:
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
// tool blocks are deliberately excluded — HTTP callers see the final reply
// text only.
func (c *ResponseCollector) extractText(eventRaw json.RawMessage) {
	if eventRaw == nil {
		return
	}

	var event struct {
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(eventRaw, &event); err != nil {
		return
	}
	if event.Delta == nil || event.Delta.Type != "text_delta" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.text.WriteString(event.Delta.Text)
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
		return "", SessionStats{}, fmt.Errorf("%w after %v", ErrResponseTimeout, timeout)
	}
}
