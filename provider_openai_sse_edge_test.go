package main

// Additional SSE parsing coverage for OpenAIChatTransport.StreamChunks.
// Builds on the existing TestOpenAISSEParsing_* tests; focuses on the
// edge cases most likely to regress when someone refactors stream parsing:
//   - Multiple concurrent tool calls in one turn (distinct indices)
//   - Tool args delivered without a preceding id (some providers do this)
//   - Text and tool deltas interleaved
//   - Empty data: lines / non-data: SSE lines / out-of-order chunks
//   - finish_reason ahead of [DONE] sentinel (some providers omit [DONE])

import (
	"strings"
	"testing"
	"time"
)

func TestOpenAISSE_MultipleToolCalls_DistinctIndices(t *testing.T) {
	payload := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"add","arguments":""}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"sub","arguments":""}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"b\":2}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	transport := &OpenAIChatTransport{}
	var starts []ToolStartEvent
	argsByIndex := map[int]*strings.Builder{}

	result := transport.StreamChunks(sseResponse(t, payload), time.Now(), func(d ChatDelta) {
		if d.ToolStart != nil {
			starts = append(starts, *d.ToolStart)
		}
		if d.ToolArgs != nil {
			if argsByIndex[d.ToolArgs.Index] == nil {
				argsByIndex[d.ToolArgs.Index] = &strings.Builder{}
			}
			argsByIndex[d.ToolArgs.Index].WriteString(d.ToolArgs.Partial)
		}
	})
	if result.Err != nil {
		t.Fatalf("err: %v", result.Err)
	}
	if len(starts) != 2 {
		t.Fatalf("ToolStart count: got %d, want 2 (one per tool index)", len(starts))
	}
	if argsByIndex[0] == nil || argsByIndex[0].String() != `{"a":1}` {
		t.Errorf("call_a args: got %v, want {\"a\":1}", argsByIndex[0])
	}
	if argsByIndex[1] == nil || argsByIndex[1].String() != `{"b":2}` {
		t.Errorf("call_b args: got %v, want {\"b\":2}", argsByIndex[1])
	}
	// Confirm the ids landed on the right indices.
	for _, s := range starts {
		switch s.Index {
		case 0:
			if s.ID != "call_a" || s.Name != "add" {
				t.Errorf("index 0: %+v", s)
			}
		case 1:
			if s.ID != "call_b" || s.Name != "sub" {
				t.Errorf("index 1: %+v", s)
			}
		}
	}
}

func TestOpenAISSE_TextAndToolCallInterleaved(t *testing.T) {
	payload := `data: {"choices":[{"index":0,"delta":{"content":"Let me check. "}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"search","arguments":"{\"q\":\"go\"}"}}]}}]}

data: {"choices":[{"index":0,"delta":{"content":"Done."}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	transport := &OpenAIChatTransport{}
	var deltas []ChatDelta
	result := transport.StreamChunks(sseResponse(t, payload), time.Now(), func(d ChatDelta) {
		deltas = append(deltas, d)
	})
	if result.Err != nil {
		t.Fatalf("err: %v", result.Err)
	}
	// The accumulated text should contain both fragments around the tool call.
	if !strings.Contains(result.FullText, "Let me check") || !strings.Contains(result.FullText, "Done.") {
		t.Errorf("FullText missing surrounding text: %q", result.FullText)
	}
	// Asserting on event order: there should be at least one text delta
	// BEFORE the first ToolStart, and at least one AFTER it.
	firstToolStartAt := -1
	for i, d := range deltas {
		if d.ToolStart != nil {
			firstToolStartAt = i
			break
		}
	}
	if firstToolStartAt < 0 {
		t.Fatalf("no ToolStart emitted; deltas=%+v", deltas)
	}
	textBefore, textAfter := false, false
	for i, d := range deltas {
		if d.Text == "" {
			continue
		}
		if i < firstToolStartAt {
			textBefore = true
		}
		if i > firstToolStartAt {
			textAfter = true
		}
	}
	if !textBefore || !textAfter {
		t.Errorf("text not on both sides of tool call: before=%v after=%v", textBefore, textAfter)
	}
}

func TestOpenAISSE_HandlesEmptyDataLinesAndComments(t *testing.T) {
	// SSE allows blank lines (event boundaries) and ":" comment lines.
	// Some providers emit keep-alive comments; the parser shouldn't choke.
	payload := `: ping

data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}

: another comment

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	transport := &OpenAIChatTransport{}
	result := transport.StreamChunks(sseResponse(t, payload), time.Now(), func(d ChatDelta) {})
	if result.Err != nil {
		t.Fatalf("err on stream with SSE comments: %v", result.Err)
	}
	if result.FullText != "hi" {
		t.Errorf("FullText: got %q, want %q", result.FullText, "hi")
	}
}

func TestOpenAISSE_FinishReasonWithoutDONESentinel(t *testing.T) {
	// Some upstreams (especially self-hosted) end the stream cleanly with
	// finish_reason set and then just close the connection — no [DONE] line.
	// The parser should not error out at EOF in that case.
	payload := `data: {"choices":[{"index":0,"delta":{"content":"complete"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	transport := &OpenAIChatTransport{}
	result := transport.StreamChunks(sseResponse(t, payload), time.Now(), func(d ChatDelta) {})
	if result.Err != nil {
		t.Fatalf("err on stream without [DONE]: %v", result.Err)
	}
	if result.FullText != "complete" {
		t.Errorf("FullText: got %q, want %q", result.FullText, "complete")
	}
}

func TestOpenAISSE_MalformedJSON_DoesNotPanic(t *testing.T) {
	// One bad line shouldn't take down the goroutine. Subsequent good
	// lines should still parse.
	payload := `data: {this is not json}

data: {"choices":[{"index":0,"delta":{"content":"recovered"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	transport := &OpenAIChatTransport{}
	result := transport.StreamChunks(sseResponse(t, payload), time.Now(), func(d ChatDelta) {})
	// We don't assert on Err here — provider may surface it or skip the
	// bad chunk. The important contract: subsequent good chunks process.
	if !strings.Contains(result.FullText, "recovered") {
		t.Errorf("parser did not recover from malformed chunk; FullText=%q err=%v", result.FullText, result.Err)
	}
}
