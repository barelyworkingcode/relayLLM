package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

	projectDir := filepath.Join(home, ".claude", "projects", encoded)
	jsonlPath := filepath.Join(projectDir, claudeSessionID+".jsonl")

	f, err := os.Open(jsonlPath)
	if err != nil {
		return nil, fmt.Errorf("open jsonl: %w", err)
	}
	defer f.Close()

	sidechainQueue := loadSidechainQueue(filepath.Join(projectDir, claudeSessionID))

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

		// Pass all content blocks through, including thinking. Eve's renderer
		// handles thinking via the existing <think> tag wrapping path; clients
		// that want to ignore thinking can still filter on their end. (Stripping
		// here would also drop signatures Anthropic uses to verify thinking
		// integrity on follow-up calls.)
		// For each Agent/Task tool_use, append a sibling agent_transcript block
		// carrying the matched sub-agent transcript so the client can render
		// the nested thread on rejoin.
		expanded := make([]json.RawMessage, 0, len(g.blocks))
		for _, block := range g.blocks {
			expanded = append(expanded, block)
			var meta struct {
				Type string `json:"type"`
				Name string `json:"name"`
				ID   string `json:"id"`
			}
			if json.Unmarshal(block, &meta) != nil {
				continue
			}
			if meta.Type != "tool_use" {
				continue
			}
			if meta.Name != "Agent" && meta.Name != "Task" {
				continue
			}
			transcript := sidechainQueue.consumeNext()
			if transcript == nil {
				continue
			}
			expanded = append(expanded, transcript)
		}

		content, _ := json.Marshal(expanded)
		messages = append(messages, Message{
			Timestamp: g.timestamp,
			Role:      "assistant",
			Content:   content,
		})

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

			// If content is an array, it may carry tool_result blocks (Claude
			// emits these after each tool_use), text blocks (real user input
			// with rich content), or a mix. Emit one Message per tool_result
			// block so Eve can pair each back to its tool_use block by id.
			// Text blocks are joined into a single user message as before.
			if content[0] == '[' {
				var fullBlocks []json.RawMessage
				_ = json.Unmarshal(content, &fullBlocks)
				var textParts []string
				for _, fb := range fullBlocks {
					var meta struct {
						Type       string          `json:"type"`
						Text       string          `json:"text"`
						ToolUseID  string          `json:"tool_use_id"`
						ResultBody json.RawMessage `json:"content"`
					}
					if json.Unmarshal(fb, &meta) != nil {
						continue
					}
					switch meta.Type {
					case "text":
						textParts = append(textParts, meta.Text)
					case "tool_result":
						// Emit each tool_result as its own role="tool" message.
						// ToolUseID lets the client locate the originating
						// tool_use block in rendered history.
						resultContent := meta.ResultBody
						if len(resultContent) == 0 {
							resultContent = json.RawMessage(`""`)
						}
						messages = append(messages, Message{
							Timestamp: entry.Timestamp,
							Role:      "tool",
							Content:   resultContent,
							ToolUseID: meta.ToolUseID,
						})
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

// claudeSidechainQueue holds parsed sub-agent transcripts in mtime order.
// Each parent Agent/Task tool_use call consumes the next entry to embed an
// agent_transcript content block alongside the parent's tool_use block.
//
// Rationale for the temporal pairing: persisted sidechain entries carry an
// agentId but neither parent's tool_use input nor the sidechain's
// parentToolUseId field carries the cross-link. The mtime order of
// agent-*.jsonl files matches spawn order in practice — same heuristic the
// live dispatcher uses with its LIFO stack.
type claudeSidechainQueue struct {
	transcripts []json.RawMessage // serialized agent_transcript blocks, mtime-ordered
}

// consumeNext returns the next transcript in mtime order. Pairing is purely
// temporal — Anthropic doesn't persist a tool_use_id ↔ agentId mapping
// anywhere, so the caller's parent toolUseId isn't useful here.
func (q *claudeSidechainQueue) consumeNext() json.RawMessage {
	if q == nil || len(q.transcripts) == 0 {
		return nil
	}
	next := q.transcripts[0]
	q.transcripts = q.transcripts[1:]
	return next
}

// loadSidechainQueue reads <sessionDir>/subagents/agent-*.jsonl files in
// mtime order and builds an agent_transcript block per file. Returns an
// empty queue (not nil) when no sidechains exist so callers can use it
// without a nil check.
func loadSidechainQueue(sessionDir string) *claudeSidechainQueue {
	q := &claudeSidechainQueue{}
	dir := filepath.Join(sessionDir, "subagents")
	files, err := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	if err != nil || len(files) == 0 {
		return q
	}
	type fileMeta struct {
		path  string
		mtime int64
	}
	metas := make([]fileMeta, 0, len(files))
	for _, p := range files {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		metas = append(metas, fileMeta{path: p, mtime: st.ModTime().UnixNano()})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].mtime < metas[j].mtime })

	for _, m := range metas {
		blocks, agentID, persona := parseSidechainFile(m.path)
		if len(blocks) == 0 {
			continue
		}
		transcript := map[string]any{
			"type":     "agent_transcript",
			"agentId":  agentID,
			"persona":  persona,
			"messages": blocks,
		}
		raw, err := json.Marshal(transcript)
		if err != nil {
			continue
		}
		q.transcripts = append(q.transcripts, raw)
	}
	return q
}

// parseSidechainFile reads one agent-*.jsonl and returns the sub-agent's
// message stream as a list of {role, content} entries the renderer can
// replay, plus the agentId and persona inferred from the entries.
func parseSidechainFile(path string) (messages []map[string]any, agentID, persona string) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	type entry struct {
		Type             string          `json:"type"`
		AgentID          string          `json:"agentId"`
		AttributionAgent string          `json:"attributionAgent"`
		IsSidechain      bool            `json:"isSidechain"`
		Message          struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if !e.IsSidechain {
			continue
		}
		if agentID == "" && e.AgentID != "" {
			agentID = e.AgentID
		}
		if persona == "" && e.AttributionAgent != "" {
			persona = e.AttributionAgent
		}
		// Pass entries through with the same shape used in the parent
		// transcript: {role, content}. Content stays as raw JSON so the
		// frontend can dispatch through its existing block renderers.
		if e.Message.Role == "" {
			continue
		}
		messages = append(messages, map[string]any{
			"role":    e.Message.Role,
			"content": json.RawMessage(e.Message.Content),
		})
	}
	return messages, agentID, persona
}
