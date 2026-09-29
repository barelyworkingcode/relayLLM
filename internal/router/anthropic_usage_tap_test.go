package router

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// A native Anthropic stream: usage split across message_start (input and
// cache figures) and message_delta (output), a signed thinking block, a ping
// and an SSE comment.
const tapSSEStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_up1","type":"message","role":"assistant","model":"backend-model","content":[],"usage":{"input_tokens":12,"cache_read_input_tokens":900,"cache_creation_input_tokens":40,"output_tokens":1}}}` + "\n\n" +
	": keep-alive comment\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Plan <a> & \u00e9"}}` + "\n\n" +
	"event: ping\n" +
	`data: {"type": "ping"}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQBCkYIBxgCKkB+/sig=="}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":27}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

var tapSSEUsage = TokenUsage{InputTokens: 12, OutputTokens: 27, CacheReadInputTokens: 900, CacheCreationInputTokens: 40}

const tapJSONBody = "{\"id\":\"msg_up2\", \"type\":\"message\",\n \"content\":[{\"type\":\"thinking\",\"thinking\":\"hmm <x>\",\"signature\":\"sig/+==\"},{\"type\":\"text\",\"text\":\"Hi \\u00e9\"}],\n" +
	" \"usage\": {\"input_tokens\":7,\"cache_read_input_tokens\":300,\"cache_creation_input_tokens\":2,\"output_tokens\":9}}"

func readThroughTap(t *testing.T, r io.Reader, sse bool) (string, []TokenUsage) {
	t.Helper()
	var calls []TokenUsage
	tap := newAnthropicUsageTap(io.NopCloser(r), sse, func(u TokenUsage) { calls = append(calls, u) })
	got, err := io.ReadAll(tap)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := tap.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return string(got), calls
}

func TestAnthropicUsageTap_BytesUnchangedAndUsageMerged(t *testing.T) {
	huge := `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + strings.Repeat("x", 2<<20) + `"}}` + "\n\n"
	withHugeLine := strings.Replace(tapSSEStream, "event: message_delta\n", huge+"event: message_delta\n", 1)

	cases := []struct {
		name    string
		body    string
		sse     bool
		oneByte bool
		want    TokenUsage
	}{
		{"sse whole", tapSSEStream, true, false, tapSSEUsage},
		{"sse split mid-line", tapSSEStream, true, true, tapSSEUsage},
		{"sse crlf line endings", strings.ReplaceAll(tapSSEStream, "\n", "\r\n"), true, false, tapSSEUsage},
		{"sse oversized line passes untouched", withHugeLine, true, false, tapSSEUsage},
		{"json whole", tapJSONBody, false, false, TokenUsage{InputTokens: 7, OutputTokens: 9, CacheReadInputTokens: 300, CacheCreationInputTokens: 2}},
		{"json split", tapJSONBody, false, true, TokenUsage{InputTokens: 7, OutputTokens: 9, CacheReadInputTokens: 300, CacheCreationInputTokens: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r io.Reader = strings.NewReader(tc.body)
			if tc.oneByte {
				r = iotest.OneByteReader(r)
			}
			got, calls := readThroughTap(t, r, tc.sse)
			if got != tc.body {
				t.Errorf("tap changed the bytes (len got %d, want %d)", len(got), len(tc.body))
			}
			if len(calls) != 1 || calls[0] != tc.want {
				t.Errorf("onUsage calls = %+v, want exactly one with %+v", calls, tc.want)
			}
		})
	}
}
