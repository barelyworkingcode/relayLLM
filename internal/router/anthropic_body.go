package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// anthropicMutation is what the native Anthropic path is allowed to change
// in a request body.
type anthropicMutation struct {
	Model string
	// Hook point for a per-target thinking override. Deliberately NOT built:
	// measured against the local engines, they honour
	// "thinking":{"type":"disabled"} natively, so the router has nothing to
	// rewrite. Add a field here only if an engine is found that ignores it.
}

// mutateAnthropicBody replaces the value of every top-level "model" key and
// leaves every other byte of body exactly as received.
//
// It splices rather than unmarshal/marshal because Go's json.Marshal sorts
// map keys, strips whitespace and HTML-escapes, and Claude Code's prompt
// prefix must reach the engine byte-identical for its prompt cache to keep
// hitting (thinking-block signatures are likewise opaque). Duplicate top-level
// "model" keys are all replaced so the engine cannot pick a different one than
// the router routed on.
//
// Errors: malformed JSON, a non-object body, no top-level "model", or a
// non-string "model".
func mutateAnthropicBody(body []byte, m anthropicMutation) ([]byte, error) {
	if !json.Valid(body) {
		return nil, errors.New("request body is not valid JSON")
	}
	spans, err := topLevelModelSpans(body)
	if err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, errors.New(`request body has no top-level "model" field`)
	}
	var enc bytes.Buffer
	e := json.NewEncoder(&enc)
	e.SetEscapeHTML(false)
	if err := e.Encode(m.Model); err != nil {
		return nil, fmt.Errorf("encode model: %w", err)
	}
	newVal := bytes.TrimRight(enc.Bytes(), "\n")

	out := make([]byte, 0, len(body)+len(newVal))
	prev := 0
	for _, s := range spans {
		out = append(out, body[prev:s[0]]...)
		out = append(out, newVal...)
		prev = s[1]
	}
	out = append(out, body[prev:]...)
	return out, nil
}

// topLevelModelSpans returns the [start,end) byte ranges of each string value
// stored under a top-level "model" key. body must already be valid JSON.
func topLevelModelSpans(body []byte) ([][2]int, error) {
	i := skipJSONSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return nil, errors.New("request body is not a JSON object")
	}
	i++
	var spans [][2]int
	for {
		i = skipJSONSpace(body, i)
		if body[i] == '}' {
			return spans, nil
		}
		keyEnd := skipJSONValue(body, i)
		var key string
		if err := json.Unmarshal(body[i:keyEnd], &key); err != nil {
			return nil, fmt.Errorf("bad object key: %w", err)
		}
		i = skipJSONSpace(body, keyEnd) + 1 // ':'
		i = skipJSONSpace(body, i)
		valEnd := skipJSONValue(body, i)
		if key == "model" {
			if body[i] != '"' {
				return nil, errors.New(`top-level "model" is not a string`)
			}
			spans = append(spans, [2]int{i, valEnd})
		}
		i = skipJSONSpace(body, valEnd)
		if body[i] == ',' {
			i++
		}
	}
}

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipJSONValue returns the index just past the JSON value starting at i.
// The input is known valid, so this only tracks string and bracket nesting.
func skipJSONValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipJSONString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipJSONString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	default: // number, true, false, null
		for i < len(b) && b[i] != ',' && b[i] != '}' && b[i] != ']' &&
			b[i] != ' ' && b[i] != '\t' && b[i] != '\n' && b[i] != '\r' {
			i++
		}
		return i
	}
}

func skipJSONString(b []byte, i int) int {
	i++ // opening quote
	for i < len(b) {
		switch b[i] {
		case '\\':
			i += 2
		case '"':
			return i + 1
		default:
			i++
		}
	}
	return i
}
