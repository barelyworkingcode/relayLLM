package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// readClaudeHistory reads conversation history from Claude CLI's JSONL session file.
// Claude persists complete conversations at ~/.claude/projects/<encoded-dir>/<sessionID>.jsonl
func readClaudeHistory(directory, claudeSessionID string) ([]Message, error) {
	if claudeSessionID == "" {
		return nil, fmt.Errorf("no claude session ID")
	}

	// Resolve symlinks to match Claude CLI's path encoding.
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("eval symlinks: %w", err)
	}

	// Encode path: replace "/" with "-", producing e.g. "-Users-jonathan-source-project"
	encoded := strings.ReplaceAll(resolved, "/", "-")

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home dir: %w", err)
	}

	jsonlPath := filepath.Join(home, ".claude", "projects", encoded, claudeSessionID+".jsonl")

	f, err := os.Open(jsonlPath)
	if err != nil {
		return nil, fmt.Errorf("open jsonl: %w", err)
	}
	defer f.Close()

	// Parse JSONL entries, grouping assistant messages by message ID.
	type jsonlEntry struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			ID      string          `json:"id"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}

	// Collect assistant content blocks grouped by message ID (preserving order).
	type assistantGroup struct {
		messageID string
		timestamp string
		blocks    []json.RawMessage
	}

	var messages []Message
	assistantGroups := make(map[string]*assistantGroup)
	var assistantOrder []string

	flushAssistant := func(msgID string) {
		g, ok := assistantGroups[msgID]
		if !ok || len(g.blocks) == 0 {
			return
		}

		// Merge all content blocks, filtering out thinking blocks.
		var merged []json.RawMessage
		for _, block := range g.blocks {
			var blockType struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(block, &blockType) == nil && blockType.Type == "thinking" {
				continue
			}
			merged = append(merged, block)
		}

		if len(merged) > 0 {
			content, _ := json.Marshal(merged)
			messages = append(messages, Message{
				Timestamp: g.timestamp,
				Role:      "assistant",
				Content:   content,
			})
		}

		delete(assistantGroups, msgID)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry jsonlEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		// Only process user/assistant entries for this session.
		if entry.SessionID != claudeSessionID {
			continue
		}

		switch entry.Type {
		case "user":
			// Flush any pending assistant group.
			for _, id := range assistantOrder {
				flushAssistant(id)
			}
			assistantOrder = nil

			content := entry.Message.Content
			if len(content) == 0 {
				continue
			}

			// If content is a string, it's a real user message.
			if content[0] == '"' {
				messages = append(messages, Message{
					Timestamp: entry.Timestamp,
					Role:      "user",
					Content:   content,
				})
				continue
			}

			// If content is an array, check for tool_result (automated) vs real user input.
			if content[0] == '[' {
				var blocks []struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(content, &blocks) == nil && len(blocks) > 0 {
					if blocks[0].Type == "tool_result" {
						// Automated tool response — skip.
						continue
					}
				}
				// Real user message with array content (e.g. text blocks).
				// Extract text blocks only.
				var fullBlocks []json.RawMessage
				json.Unmarshal(content, &fullBlocks)
				var textParts []string
				for _, fb := range fullBlocks {
					var block struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					if json.Unmarshal(fb, &block) == nil && block.Type == "text" {
						textParts = append(textParts, block.Text)
					}
				}
				if len(textParts) > 0 {
					combined := strings.Join(textParts, "\n")
					contentJSON, _ := json.Marshal(combined)
					messages = append(messages, Message{
						Timestamp: entry.Timestamp,
						Role:      "user",
						Content:   contentJSON,
					})
				}
			}

		case "assistant":
			msgID := entry.Message.ID
			if msgID == "" {
				continue
			}

			g, exists := assistantGroups[msgID]
			if !exists {
				g = &assistantGroup{
					messageID: msgID,
					timestamp: entry.Timestamp,
				}
				assistantGroups[msgID] = g
				assistantOrder = append(assistantOrder, msgID)
			}

			// Each JSONL line has one content block in an array.
			var blocks []json.RawMessage
			if json.Unmarshal(entry.Message.Content, &blocks) == nil {
				g.blocks = append(g.blocks, blocks...)
			}
		}
	}

	// Flush remaining assistant groups.
	for _, id := range assistantOrder {
		flushAssistant(id)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan error: %w", err)
	}

	return messages, nil
}
