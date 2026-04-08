package main

import (
	"encoding/json"
	"strings"
)

// Provider abstracts an LLM backend (Claude CLI, Gemini CLI, LM Studio HTTP).
type Provider interface {
	// Start spawns the provider process or initializes the connection.
	Start() error

	// SendMessage sends a user message. Events stream via the EventHandler.
	SendMessage(text string, files []FileAttachment) error

	// StopGeneration aborts the in-flight response without killing the provider.
	StopGeneration()

	// Kill terminates the provider process.
	Kill()

	// DeleteSession removes provider-specific session data from disk.
	DeleteSession() error

	// Alive returns true if the provider process is running.
	Alive() bool

	// GetState returns provider-specific state for persistence.
	GetState() json.RawMessage

	// RestoreState restores provider-specific state after reload.
	RestoreState(state json.RawMessage)
}

// FileAttachment represents an attached file in a message.
type FileAttachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64 encoded
}

// Message represents a persisted chat message.
type Message struct {
	Timestamp string          `json:"timestamp"`
	Role      string          `json:"role"` // "user", "assistant", or "tool"
	Content   json.RawMessage `json:"content"`
	Files     []FileAttachment `json:"files,omitempty"`
	ToolName  string          `json:"toolName,omitempty"`  // set when Role="tool"
	ToolCalls json.RawMessage `json:"toolCalls,omitempty"` // set on assistant msgs that invoked tools
}

// SessionStats tracks token usage and cost.
type SessionStats struct {
	InputTokens          int     `json:"inputTokens"`
	OutputTokens         int     `json:"outputTokens"`
	CacheReadTokens      int     `json:"cacheReadTokens"`
	CacheCreationTokens  int     `json:"cacheCreationTokens"`
	CostUsd              float64 `json:"costUsd"`
	TimeToFirstToken     float64 `json:"timeToFirstToken,omitempty"`
	TokensPerSecond      float64 `json:"tokensPerSecond,omitempty"`
	PromptEvalCount      int     `json:"promptEvalCount,omitempty"`
	EvalDurationMs       float64 `json:"evalDurationMs,omitempty"`
	PromptEvalDurationMs float64 `json:"promptEvalDurationMs,omitempty"`
}

// extractTextContent extracts plain text from a Message's JSON content.
// User messages may be stored as a JSON string or raw text.
// Assistant messages are stored as [{type: "text", text: "..."}] blocks.
func extractTextContent(msg Message) string {
	if msg.Role == "user" || msg.Role == "tool" {
		var text string
		if json.Unmarshal(msg.Content, &text) == nil {
			return text
		}
		return string(msg.Content)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return string(msg.Content)
}

// EventHandler is the callback from provider to session for streaming events.
type EventHandler func(eventType string, data json.RawMessage)
