package main

import "encoding/json"

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
	Role      string          `json:"role"` // "user" or "assistant"
	Content   json.RawMessage `json:"content"`
	Files     []FileAttachment `json:"files,omitempty"`
}

// SessionStats tracks token usage and cost.
type SessionStats struct {
	InputTokens          int     `json:"inputTokens"`
	OutputTokens         int     `json:"outputTokens"`
	CacheReadTokens      int     `json:"cacheReadTokens"`
	CacheCreationTokens  int     `json:"cacheCreationTokens"`
	CostUsd              float64 `json:"costUsd"`
}

// EventHandler is the callback from provider to session for streaming events.
type EventHandler func(eventType string, data json.RawMessage)
