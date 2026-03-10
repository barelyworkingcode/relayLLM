package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ResponseCollector captures a complete LLM response for synchronous HTTP callers.
type ResponseCollector struct {
	mu       sync.Mutex
	text     strings.Builder
	stats    SessionStats
	done     chan struct{}
	err      error
	session  *Session
	manager  *SessionManager
	origSink EventSink
}

func NewResponseCollector() *ResponseCollector {
	return &ResponseCollector{
		done: make(chan struct{}),
	}
}

// Install redirects the session's events to this collector.
func (c *ResponseCollector) Install(session *Session, manager *SessionManager) {
	c.session = session
	c.manager = manager
	c.origSink = manager.sink
	manager.sink = c
}

// Uninstall restores the original event sink.
func (c *ResponseCollector) Uninstall() {
	if c.origSink != nil {
		c.manager.sink = c.origSink
	}
}

// SendToSession implements EventSink. Captures text and stats, detects completion.
func (c *ResponseCollector) SendToSession(sessionID string, msg map[string]interface{}) {
	if sessionID != c.session.ID {
		// Forward events for other sessions to the original sink.
		if c.origSink != nil {
			c.origSink.SendToSession(sessionID, msg)
		}
		return
	}

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
		close(c.done)

	case "error":
		errMsg, _ := msg["message"].(string)
		c.err = fmt.Errorf("%s", errMsg)
		close(c.done)

	case "process_exited":
		c.err = fmt.Errorf("provider process exited unexpectedly")
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}

	// Also forward to original sink so WebSocket clients see events.
	if c.origSink != nil {
		c.origSink.SendToSession(sessionID, msg)
	}
}

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
	}

	if err := json.Unmarshal(eventRaw, &event); err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if event.Delta != nil && event.Delta.Type == "text_delta" {
		c.text.WriteString(event.Delta.Text)
	}
	if event.ContentBlock != nil && event.ContentBlock.Type == "text" {
		c.text.WriteString(event.ContentBlock.Text)
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
