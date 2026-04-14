package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// OpenAIChatTransport implements ChatTransport for any server that speaks
// the OpenAI /v1/chat/completions protocol (OpenAI itself, LM Studio, Ollama's
// /v1 compat layer, OMLX, llama.cpp server, etc.).
type OpenAIChatTransport struct {
	endpoint OpenAIEndpoint
	model    string // bare model id (after prefix stripping)
	client   *http.Client
	settings BaseChatSettings
}

// NewOpenAIChatTransport constructs a transport for a configured endpoint.
// The http.Client is injected so tests can hand in httptest.NewServer clients.
func NewOpenAIChatTransport(endpoint OpenAIEndpoint, model string, settings json.RawMessage, client *http.Client) *OpenAIChatTransport {
	if client == nil {
		client = &http.Client{}
	}
	return &OpenAIChatTransport{
		endpoint: endpoint,
		model:    model,
		client:   client,
		settings: parseBaseSettings(settings),
	}
}

func (t *OpenAIChatTransport) Name() string { return "openai:" + t.endpoint.Name }

// Ping verifies the endpoint is reachable by calling /models. A healthy
// OpenAI-compatible server responds to this with a 200 and a JSON body
// listing available models.
func (t *OpenAIChatTransport) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint.BaseURL+"/models", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	t.addAuth(req)

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("not reachable at %s: %w", t.endpoint.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/models returned %d", resp.StatusCode)
	}
	return nil
}

// addAuth attaches a Bearer token when the endpoint has an API key set.
// No-op for endpoints (like local Ollama) that don't require auth.
func (t *OpenAIChatTransport) addAuth(req *http.Request) {
	if t.endpoint.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.endpoint.APIKey)
	}
}

// BuildMessages converts session history into OpenAI chat format. Images are
// embedded as content blocks in the user message (not a top-level images[]),
// and tool result messages carry a tool_call_id pairing them back to the
// assistant entry that produced them.
func (t *OpenAIChatTransport) BuildMessages(systemPrompt string, msgs []Message) []map[string]any {
	result := make([]map[string]any, 0, len(msgs)+1)

	if systemPrompt != "" {
		result = append(result, map[string]any{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	// We need to pair tool results back to the assistant tool_call that
	// produced them. Track the most recent assistant tool_call IDs so we
	// can attach tool_call_id to tool-role messages.
	var lastAssistantCalls []NormalizedToolCall
	var toolResultIdx int

	for _, msg := range msgs {
		switch msg.Role {
		case "tool":
			entry := map[string]any{
				"role":    "tool",
				"content": extractTextContent(msg),
			}
			// Attach tool_call_id if we have one from the preceding assistant.
			if toolResultIdx < len(lastAssistantCalls) {
				if id := lastAssistantCalls[toolResultIdx].ID; id != "" {
					entry["tool_call_id"] = id
				} else {
					// Synthesize a stable id if the source didn't track one.
					entry["tool_call_id"] = fmt.Sprintf("call_%s_%d", msg.ToolName, toolResultIdx)
				}
				toolResultIdx++
			}
			result = append(result, entry)

		case "assistant":
			entry := map[string]any{
				"role":    "assistant",
				"content": extractTextContent(msg),
			}
			if norm := decodeNormalizedToolCalls(msg.ToolCalls); len(norm) > 0 {
				// Mutate in place so any synthesized IDs propagate to the
				// tool-result pairing pass below.
				for i := range norm {
					if norm[i].ID == "" {
						norm[i].ID = synthesizeToolCallID(norm[i].Name, i)
					}
				}
				entry["tool_calls"] = buildOpenAIToolCallEntries(norm)
				lastAssistantCalls = norm
			} else {
				lastAssistantCalls = nil
			}
			toolResultIdx = 0
			result = append(result, entry)

		default: // "user" and any other role
			text := extractTextContent(msg)
			if len(msg.Files) == 0 {
				result = append(result, map[string]any{
					"role":    msg.Role,
					"content": text,
				})
				continue
			}
			// Images present → use content block array.
			parts := make([]map[string]any, 0, 1+len(msg.Files))
			if text != "" {
				parts = append(parts, map[string]any{
					"type": "text",
					"text": text,
				})
			}
			for _, f := range msg.Files {
				url := f.Data
				if !strings.HasPrefix(url, "data:") {
					mime := f.MimeType
					if mime == "" {
						mime = "image/png"
					}
					url = fmt.Sprintf("data:%s;base64,%s", mime, url)
				}
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": url,
					},
				})
			}
			result = append(result, map[string]any{
				"role":    msg.Role,
				"content": parts,
			})
		}
	}
	return result
}

// PostChat sends a streaming /chat/completions request.
func (t *OpenAIChatTransport) PostChat(ctx context.Context, messages []map[string]any, tools []map[string]any) (*http.Response, error) {
	body := t.buildChatBody(messages, tools)
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint.BaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	t.addAuth(req)

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return resp, nil
}

func (t *OpenAIChatTransport) buildChatBody(messages []map[string]any, tools []map[string]any) map[string]any {
	body := map[string]any{
		"model":    t.model,
		"messages": messages,
		"stream":   true,
		// Ask the server to include usage stats in the final chunk. Servers
		// that don't support this field ignore it safely; servers that do
		// (OpenAI, LM Studio) will emit a final chunk with {usage:{...}}.
		"stream_options": map[string]any{"include_usage": true},
	}
	if t.settings.Temperature != nil {
		body["temperature"] = *t.settings.Temperature
	}
	if t.settings.TopP != nil {
		body["top_p"] = *t.settings.TopP
	}
	if t.settings.TopK != nil {
		// Not standard OpenAI, but most compatible servers (LM Studio, Ollama /v1)
		// accept it as an extension. Harmless on servers that ignore it.
		body["top_k"] = *t.settings.TopK
	}
	if len(tools) > 0 {
		body["tools"] = tools
		// Explicitly set tool_choice to "auto" so OpenAI-compatible servers
		// (LM Studio, Ollama /v1, oMLX) know the model may call tools.
		// OpenAI proper defaults to "auto", but compat servers may not.
		body["tool_choice"] = "auto"
	}
	return body
}

// openAIStreamChunk mirrors the OpenAI /chat/completions streaming chunk shape.
// Fields not directly used are kept for forward-compat.
type openAIStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string                `json:"role"`
			Content          string                `json:"content"`
			ReasoningContent string                `json:"reasoning_content"` // LM Studio / reasoning models
			Reasoning        string                `json:"reasoning"`         // alt spelling seen in some servers
			ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // streamed as string fragments
	} `json:"function"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunks reads the SSE-formatted response body, emits text/thinking
// deltas through the provided callback, and returns the accumulated result.
//
// SSE format: each event is `data: <json>\n\n`, terminated by `data: [DONE]`.
// Tool call arguments stream as string fragments across multiple chunks and
// must be concatenated per (choice_index, tool_call_index).
func (t *OpenAIChatTransport) StreamChunks(resp *http.Response, startTime time.Time, emit func(ChatDelta)) NormalizedStreamResult {
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	var fullText strings.Builder
	var usage *openAIUsage

	// Tool call accumulator: index → in-progress call. OpenAI streams the
	// name + id once and then arguments as fragments, all keyed by the
	// same index.
	toolAcc := make(map[int]*accumulatingToolCall)
	var toolOrder []int // insertion order so we emit calls in a stable sequence

	var firstTokenAt time.Time

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// Some servers prefix comments with ":" or include event: lines.
			// Skip anything that isn't a data: line.
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			slog.Warn("openai: invalid SSE chunk", "error", err, "line", line)
			continue
		}

		if chunk.Usage != nil {
			usage = chunk.Usage
		}

		if len(chunk.Choices) == 0 {
			// Usage-only final chunk. Nothing else to do.
			continue
		}
		delta := chunk.Choices[0].Delta

		// Thinking / reasoning content (servers use different field names).
		reasoning := delta.ReasoningContent
		if reasoning == "" {
			reasoning = delta.Reasoning
		}
		if reasoning != "" {
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
			emit(ChatDelta{Thinking: reasoning})
		}

		if delta.Content != "" {
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
			fullText.WriteString(delta.Content)
			emit(ChatDelta{Text: delta.Content})
		}

		for _, tcd := range delta.ToolCalls {
			acc, existed := toolAcc[tcd.Index]
			if !existed {
				acc = &accumulatingToolCall{}
				toolAcc[tcd.Index] = acc
				toolOrder = append(toolOrder, tcd.Index)
			}
			if tcd.ID != "" {
				acc.id = tcd.ID
			}
			if tcd.Function.Name != "" && acc.name == "" {
				acc.name = tcd.Function.Name
			}
			if tcd.Function.Arguments != "" {
				acc.args.WriteString(tcd.Function.Arguments)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return NormalizedStreamResult{FullText: fullText.String(), Err: err}
	}

	// Finalize tool calls in insertion order.
	var toolCalls []NormalizedToolCall
	for _, idx := range toolOrder {
		acc := toolAcc[idx]
		if acc.name == "" {
			continue
		}
		args := strings.TrimSpace(acc.args.String())
		if args == "" {
			args = "{}"
		}
		toolCalls = append(toolCalls, NormalizedToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: json.RawMessage(args),
		})
	}

	// Compute stats. OpenAI-compatible servers don't expose Ollama-style
	// eval durations, so we derive TTFT and TPS from wall clock.
	stats := SessionStats{}
	if usage != nil {
		stats.InputTokens = usage.PromptTokens
		stats.OutputTokens = usage.CompletionTokens
	}
	if !firstTokenAt.IsZero() {
		stats.TimeToFirstToken = firstTokenAt.Sub(startTime).Seconds()
		if stats.OutputTokens > 0 {
			elapsed := time.Since(firstTokenAt).Seconds()
			if elapsed > 0 {
				stats.TokensPerSecond = float64(stats.OutputTokens) / elapsed
			}
		}
	}

	return NormalizedStreamResult{
		FullText:  fullText.String(),
		ToolCalls: toolCalls,
		Stats:     stats,
	}
}

// accumulatingToolCall holds in-progress state for one tool call as SSE
// deltas arrive. OpenAI streams name + id once and then arguments as
// string fragments; we concatenate them here.
type accumulatingToolCall struct {
	id   string
	name string
	args strings.Builder
}

// AppendAssistantWithToolCalls adds an assistant-with-tool-calls entry in
// OpenAI's wire shape (each call has id, type, and function with string
// arguments).
func (t *OpenAIChatTransport) AppendAssistantWithToolCalls(messages []map[string]any, text string, toolCalls []NormalizedToolCall) []map[string]any {
	return append(messages, map[string]any{
		"role":       "assistant",
		"content":    text,
		"tool_calls": buildOpenAIToolCallEntries(toolCalls),
	})
}

// AppendToolResult adds a tool result entry with the required tool_call_id.
func (t *OpenAIChatTransport) AppendToolResult(messages []map[string]any, tc NormalizedToolCall, result string) []map[string]any {
	id := tc.ID
	if id == "" {
		id = synthesizeToolCallID(tc.Name, 0)
	}
	return append(messages, map[string]any{
		"role":         "tool",
		"tool_call_id": id,
		"content":      result,
	})
}

// synthesizeToolCallID produces a deterministic fallback id for tool calls
// that didn't carry one (e.g. persisted sessions from before the refactor,
// or transports that don't track ids natively). The format matches what
// OpenAI's own ids look like enough that servers don't balk.
func synthesizeToolCallID(name string, index int) string {
	return fmt.Sprintf("call_%s_%d", name, index)
}

// buildOpenAIToolCallEntries converts normalized tool calls into the OpenAI
// wire shape ({id, type:"function", function:{name, arguments}}). Used by
// BuildMessages (reading persisted history) and AppendAssistantWithToolCalls
// (running tool loop).
func buildOpenAIToolCallEntries(toolCalls []NormalizedToolCall) []map[string]any {
	out := make([]map[string]any, len(toolCalls))
	for i, n := range toolCalls {
		id := n.ID
		if id == "" {
			id = synthesizeToolCallID(n.Name, i)
		}
		args := string(n.Arguments)
		if args == "" {
			args = "{}"
		}
		out[i] = map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      n.Name,
				"arguments": args,
			},
		}
	}
	return out
}

// FetchOpenAIModels queries /v1/models on the endpoint and returns ModelInfo
// entries. Model values are prefixed with the endpoint name so the session
// layer can route them back to the right endpoint at session-create time.
func FetchOpenAIModels(endpoint OpenAIEndpoint) []ModelInfo {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodGet, endpoint.BaseURL+"/models", nil)
	if err != nil {
		return nil
	}
	if endpoint.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("openai: model discovery failed", "endpoint", endpoint.Name, "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("openai: model discovery returned non-OK status", "endpoint", endpoint.Name, "status", resp.StatusCode)
		return nil
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	models := make([]ModelInfo, 0, len(result.Data))
	for _, m := range result.Data {
		value := endpoint.Name + "/" + m.ID
		models = append(models, ModelInfo{
			Label:    value,
			Value:    value,
			Group:    endpoint.Group,
			Provider: "openai",
		})
	}
	return models
}
