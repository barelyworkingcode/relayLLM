package router

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
)

const (
	usageTapMaxLine = 1 << 20 // longest SSE line parsed; a longer one is skipped
	usageTapMaxBody = 8 << 20 // largest JSON body buffered for parsing
)

// newAnthropicUsageTap wraps an upstream response body so the token usage it
// reports can be counted. It is read-only: every byte Read returns is exactly
// what the upstream sent, so a thinking signature or cache figure is never
// touched. onUsage is called once, at EOF or Close, with what was seen, and
// not at all when nothing was.
//
// sse selects the parser: message_start / message_delta events for a stream,
// the top-level "usage" object for a JSON body. Parse failures and oversized
// input silently stop the accounting, never the response.
func newAnthropicUsageTap(body io.ReadCloser, sse bool, onUsage func(TokenUsage)) io.ReadCloser {
	return &usageTap{rc: body, sse: sse, onUsage: onUsage}
}

type usageTap struct {
	rc      io.ReadCloser
	sse     bool
	onUsage func(TokenUsage)

	once  sync.Once
	seen  bool
	usage TokenUsage

	line     []byte // partial SSE line carried across chunks
	skipLine bool   // current line exceeded usageTapMaxLine; drop until newline
	buf      []byte // JSON body, bounded
	overflow bool
}

func (t *usageTap) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.feed(p[:n])
	}
	if err == io.EOF {
		t.finish()
	}
	return n, err
}

func (t *usageTap) Close() error {
	t.finish()
	return t.rc.Close()
}

func (t *usageTap) feed(b []byte) {
	if !t.sse {
		if t.overflow {
			return
		}
		if len(t.buf)+len(b) > usageTapMaxBody {
			t.overflow, t.buf = true, nil
			return
		}
		t.buf = append(t.buf, b...)
		return
	}
	for len(b) > 0 {
		nl := bytes.IndexByte(b, '\n')
		if nl < 0 {
			t.appendLine(b)
			return
		}
		t.appendLine(b[:nl])
		if !t.skipLine {
			t.parseSSELine(t.line)
		}
		t.line, t.skipLine = t.line[:0], false
		b = b[nl+1:]
	}
}

func (t *usageTap) appendLine(b []byte) {
	if t.skipLine {
		return
	}
	if len(t.line)+len(b) > usageTapMaxLine {
		t.skipLine, t.line = true, t.line[:0]
		return
	}
	t.line = append(t.line, b...)
}

type usageWire struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// merge overlays the non-zero fields: message_delta repeats cumulative counts
// for the fields it carries and omits (or zeroes) the rest.
func (t *usageTap) merge(u *usageWire) {
	if u == nil {
		return
	}
	t.seen = true
	if u.InputTokens > 0 {
		t.usage.InputTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		t.usage.OutputTokens = u.OutputTokens
	}
	if u.CacheReadInputTokens > 0 {
		t.usage.CacheReadInputTokens = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		t.usage.CacheCreationInputTokens = u.CacheCreationInputTokens
	}
}

func (t *usageTap) parseSSELine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	if !bytes.Contains(data, []byte(`"message_start"`)) && !bytes.Contains(data, []byte(`"message_delta"`)) {
		return
	}
	var ev struct {
		Type    string     `json:"type"`
		Usage   *usageWire `json:"usage"`
		Message struct {
			Usage *usageWire `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		t.merge(ev.Message.Usage)
	case "message_delta":
		t.merge(ev.Usage)
	}
}

func (t *usageTap) finish() {
	t.once.Do(func() {
		if t.sse {
			if len(t.line) > 0 && !t.skipLine {
				t.parseSSELine(t.line) // final line without a trailing newline
			}
		} else if !t.overflow && len(t.buf) > 0 {
			var env struct {
				Usage *usageWire `json:"usage"`
			}
			if json.Unmarshal(t.buf, &env) == nil {
				t.merge(env.Usage)
			}
		}
		t.buf, t.line = nil, nil
		if t.seen && t.onUsage != nil {
			t.onUsage(t.usage)
		}
	})
}
