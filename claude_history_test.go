package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeJSONL writes lines (one JSON-marshaled value per line) to path.
func writeJSONL(t *testing.T, path string, lines []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var sb strings.Builder
	for _, line := range lines {
		b, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("marshal line: %v", err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// withFakeClaudeHome redirects $HOME for the duration of the test so
// readClaudeHistory looks at our fixture rather than the user's real
// ~/.claude/projects directory. Returns the path to the project dir
// under the fake home.
func withFakeClaudeHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return tmp
}

// projectDirFor mirrors readClaudeHistory's path encoding: EvalSymlinks the
// directory (so /var/... becomes /private/var/... on macOS) and then replace
// "/" with "-".
func projectDirFor(t *testing.T, home, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	encoded := strings.ReplaceAll(resolved, "/", "-")
	return filepath.Join(home, ".claude", "projects", encoded)
}

func TestReadClaudeHistory_PreservesThinkingBlocks(t *testing.T) {
	home := withFakeClaudeHome(t)
	dir := t.TempDir() // any real existing path; readClaudeHistory calls EvalSymlinks
	projectDir := projectDirFor(t, home, dir)
	sessionID := "test-session-001"

	// Assistant message with thinking + text + tool_use blocks. Each line in
	// the JSONL is one block under the same message.id.
	writeJSONL(t, filepath.Join(projectDir, sessionID+".jsonl"), []map[string]any{
		{"type": "user", "sessionId": sessionID, "timestamp": "2026-05-01T00:00:00Z", "message": map[string]any{"role": "user", "content": "hi"}},
		// Three lines that share message.id="msg-A" → grouped into one assistant message.
		{"type": "assistant", "sessionId": sessionID, "timestamp": "2026-05-01T00:00:01Z", "message": map[string]any{
			"id": "msg-A", "role": "assistant", "content": []map[string]any{
				{"type": "thinking", "thinking": "let me think...", "signature": "sig123"},
			},
		}},
		{"type": "assistant", "sessionId": sessionID, "timestamp": "2026-05-01T00:00:02Z", "message": map[string]any{
			"id": "msg-A", "role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "Hello!"},
			},
		}},
	})

	msgs, err := readClaudeHistory(dir, nil, sessionID)
	if err != nil {
		t.Fatalf("readClaudeHistory: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (user + assistant); got: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want user", msgs[0].Role)
	}
	if msgs[1].Role != "assistant" {
		t.Fatalf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}

	// The assistant message should contain BOTH the thinking and text blocks.
	var blocks []map[string]any
	if err := json.Unmarshal(msgs[1].Content, &blocks); err != nil {
		t.Fatalf("unmarshal assistant content: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("assistant blocks = %d, want 2 (thinking + text): %+v", len(blocks), blocks)
	}
	if blocks[0]["type"] != "thinking" {
		t.Errorf("blocks[0].type = %v, want thinking — thinking blocks should NOT be filtered out", blocks[0]["type"])
	}
	if blocks[0]["signature"] != "sig123" {
		t.Errorf("blocks[0].signature = %v, want sig123 — signature must be preserved for Anthropic verification", blocks[0]["signature"])
	}
	if blocks[1]["type"] != "text" {
		t.Errorf("blocks[1].type = %v, want text", blocks[1]["type"])
	}
}

func TestReadClaudeHistory_SurfacesToolResultsAsRoleTool(t *testing.T) {
	home := withFakeClaudeHome(t)
	dir := t.TempDir()
	projectDir := projectDirFor(t, home, dir)
	sessionID := "test-session-002"

	writeJSONL(t, filepath.Join(projectDir, sessionID+".jsonl"), []map[string]any{
		{"type": "user", "sessionId": sessionID, "timestamp": "2026-05-01T00:00:00Z", "message": map[string]any{"role": "user", "content": "do thing"}},
		{"type": "assistant", "sessionId": sessionID, "timestamp": "2026-05-01T00:00:01Z", "message": map[string]any{
			"id": "msg-B", "role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "toolu_01XYZ", "name": "Read", "input": map[string]any{"path": "/tmp/x"}},
			},
		}},
		// Tool result arrives as a user-typed message with content array.
		{"type": "user", "sessionId": sessionID, "timestamp": "2026-05-01T00:00:02Z", "message": map[string]any{
			"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_01XYZ", "content": "file contents..."},
			},
		}},
	})

	msgs, err := readClaudeHistory(dir, nil, sessionID)
	if err != nil {
		t.Fatalf("readClaudeHistory: %v", err)
	}

	// Expect: user, assistant, tool. Three messages.
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3; got: %+v", len(msgs), summarize(msgs))
	}
	tool := msgs[2]
	if tool.Role != "tool" {
		t.Errorf("msgs[2].Role = %q, want tool", tool.Role)
	}
	if tool.ToolUseID != "toolu_01XYZ" {
		t.Errorf("ToolUseID = %q, want toolu_01XYZ", tool.ToolUseID)
	}
	// Content was a string in the source — should round-trip as a JSON string.
	var content string
	if err := json.Unmarshal(tool.Content, &content); err != nil {
		t.Fatalf("unmarshal tool content: %v (raw: %s)", err, string(tool.Content))
	}
	if content != "file contents..." {
		t.Errorf("tool content = %q, want 'file contents...'", content)
	}
}

func TestReadClaudeHistory_EmbedsSidechainTranscriptOnAgentToolUse(t *testing.T) {
	home := withFakeClaudeHome(t)
	dir := t.TempDir()
	projectDir := projectDirFor(t, home, dir)
	sessionID := "test-session-003"

	// Main session: parent calls Agent tool, then tool_result arrives.
	writeJSONL(t, filepath.Join(projectDir, sessionID+".jsonl"), []map[string]any{
		{"type": "user", "sessionId": sessionID, "timestamp": "2026-05-01T00:00:00Z", "message": map[string]any{"role": "user", "content": "explore"}},
		{"type": "assistant", "sessionId": sessionID, "timestamp": "2026-05-01T00:00:01Z", "message": map[string]any{
			"id": "msg-C", "role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "toolu_AGENT_1", "name": "Agent",
					"input": map[string]any{"subagent_type": "Explore", "description": "investigate things", "prompt": "..."}},
			},
		}},
		{"type": "user", "sessionId": sessionID, "timestamp": "2026-05-01T00:00:30Z", "message": map[string]any{
			"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_AGENT_1", "content": "explored everything"},
			},
		}},
	})

	// Sidechain file: <projectDir>/<sessionID>/subagents/agent-FOO.jsonl
	subDir := filepath.Join(projectDir, sessionID, "subagents")
	subPath := filepath.Join(subDir, "agent-FOO.jsonl")
	writeJSONL(t, subPath, []map[string]any{
		{"type": "user", "isSidechain": true, "agentId": "FOO", "sessionId": sessionID, "message": map[string]any{
			"role": "user", "content": "[sidechain prompt body]",
		}},
		{"type": "assistant", "isSidechain": true, "agentId": "FOO", "attributionAgent": "Explore", "sessionId": sessionID, "message": map[string]any{
			"id": "submsg-1", "role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "looking around"},
				{"type": "tool_use", "id": "toolu_SUB_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
			},
		}},
	})
	// Force mtime to be slightly after the parent so the sidechain is paired.
	now := time.Now()
	_ = os.Chtimes(subPath, now, now)

	msgs, err := readClaudeHistory(dir, nil, sessionID)
	if err != nil {
		t.Fatalf("readClaudeHistory: %v", err)
	}

	// Expect: user, assistant (with Agent tool_use AND agent_transcript), tool_result.
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3; got: %s", len(msgs), summarize(msgs))
	}
	asst := msgs[1]
	if asst.Role != "assistant" {
		t.Fatalf("msgs[1].Role = %q, want assistant", asst.Role)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(asst.Content, &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("assistant blocks = %d, want 2 (tool_use + agent_transcript): %+v", len(blocks), blocks)
	}
	if blocks[0]["type"] != "tool_use" || blocks[0]["name"] != "Agent" {
		t.Errorf("blocks[0] = %+v, want tool_use Agent", blocks[0])
	}
	if blocks[1]["type"] != "agent_transcript" {
		t.Errorf("blocks[1].type = %v, want agent_transcript", blocks[1]["type"])
	}
	if blocks[1]["agentId"] != "FOO" {
		t.Errorf("blocks[1].agentId = %v, want FOO", blocks[1]["agentId"])
	}
	if blocks[1]["persona"] != "Explore" {
		t.Errorf("blocks[1].persona = %v, want Explore", blocks[1]["persona"])
	}
	subMsgs, _ := blocks[1]["messages"].([]any)
	if len(subMsgs) < 2 {
		t.Errorf("agent_transcript messages = %d, want >= 2 (sub-user + sub-assistant)", len(subMsgs))
	}
}

func TestReadClaudeHistory_NoSidechainsLeavesAssistantUntouched(t *testing.T) {
	home := withFakeClaudeHome(t)
	dir := t.TempDir()
	projectDir := projectDirFor(t, home, dir)
	sessionID := "test-session-004"

	writeJSONL(t, filepath.Join(projectDir, sessionID+".jsonl"), []map[string]any{
		{"type": "assistant", "sessionId": sessionID, "timestamp": "2026-05-01T00:00:01Z", "message": map[string]any{
			"id": "msg-D", "role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "no agents here"},
			},
		}},
	})

	msgs, err := readClaudeHistory(dir, nil, sessionID)
	if err != nil {
		t.Fatalf("readClaudeHistory: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	var blocks []map[string]any
	if err := json.Unmarshal(msgs[0].Content, &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "text" {
		t.Errorf("blocks = %+v; want a single text block", blocks)
	}
}

// summarize gives a compact string describing a message slice for failure logs.
func summarize(msgs []Message) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = m.Role
		if m.ToolUseID != "" {
			parts[i] += "(" + m.ToolUseID + ")"
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
