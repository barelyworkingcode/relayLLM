package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// userMsg builds a Message with Role "user" and the given raw JSON content
// (already marshaled), mirroring how session.go stores user turns.
func userMsg(t *testing.T, content string) Message {
	t.Helper()
	return Message{Role: "user", Content: json.RawMessage(content), Timestamp: "2026-01-01T00:00:00Z"}
}

func TestSessionPreview(t *testing.T) {
	longAscii := strings.Repeat("a", 150)
	wantLongAscii := strings.Repeat("a", 120) + "…"

	longMulti := strings.Repeat("é", 150)
	wantLongMulti := strings.Repeat("é", 120) + "…"

	tests := []struct {
		name string
		msgs []Message
		want string
	}{
		{
			name: "plain string content",
			msgs: []Message{userMsg(t, `"hello there"`)},
			want: "hello there",
		},
		{
			name: "json-encoded string content with whitespace",
			msgs: []Message{userMsg(t, `"  hello\n\tworld  "`)},
			want: "hello world",
		},
		{
			// extractTextContent only tries flattenTextBlocks for
			// non-user/non-tool roles; for "user" it falls back to the raw
			// JSON bytes when the content isn't a plain string. In
			// practice session.Messages never stores user turns as block
			// arrays (SendMessage always marshals a plain string), so this
			// pins that fallback rather than exercising real data.
			name: "block-array content falls back to raw JSON via extractTextContent",
			msgs: []Message{
				{
					Role:      "user",
					Timestamp: "2026-01-01T00:00:00Z",
					Content:   json.RawMessage(`[{"type":"text","text":"part one "},{"type":"text","text":"part two"}]`),
				},
			},
			want: `[{"type":"text","text":"part one "},{"type":"text","text":"part two"}]`,
		},
		{
			name: "slash command skipped in favor of first real user turn",
			msgs: []Message{
				userMsg(t, `"/compact"`),
				{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Content: json.RawMessage(`"ok"`)},
				userMsg(t, `"actual question"`),
			},
			want: "actual question",
		},
		{
			name: "truncation at 120 runes, ascii",
			msgs: []Message{userMsg(t, mustMarshal(t, longAscii))},
			want: wantLongAscii,
		},
		{
			name: "truncation at 120 runes, multibyte",
			msgs: []Message{userMsg(t, mustMarshal(t, longMulti))},
			want: wantLongMulti,
		},
		{
			name: "no user message",
			msgs: []Message{
				{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Content: json.RawMessage(`"hi"`)},
			},
			want: "",
		},
		{
			name: "empty message list",
			msgs: nil,
			want: "",
		},
		{
			name: "only slash commands",
			msgs: []Message{userMsg(t, `"/help"`), userMsg(t, `"/compact"`)},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionPreview(tt.msgs)
			if got != tt.want {
				t.Errorf("sessionPreview() = %q, want %q", got, tt.want)
			}
			if got != "" {
				if n := len([]rune(got)); n > 121 {
					t.Errorf("sessionPreview() returned %d runes, want <= 121 (120 + ellipsis)", n)
				}
			}
		})
	}
}

func mustMarshal(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestLastMessageAt(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		want string
	}{
		{
			name: "no messages",
			msgs: nil,
			want: "",
		},
		{
			name: "single message",
			msgs: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Content: json.RawMessage(`"hi"`)}},
			want: "2026-01-01T00:00:00Z",
		},
		{
			name: "multiple messages returns last",
			msgs: []Message{
				{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Content: json.RawMessage(`"hi"`)},
				{Role: "assistant", Timestamp: "2026-01-01T00:00:05Z", Content: json.RawMessage(`"hello"`)},
				{Role: "user", Timestamp: "2026-01-01T00:00:10Z", Content: json.RawMessage(`"bye"`)},
			},
			want: "2026-01-01T00:00:10Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lastMessageAt(tt.msgs); got != tt.want {
				t.Errorf("lastMessageAt() = %q, want %q", got, tt.want)
			}
		})
	}
}
