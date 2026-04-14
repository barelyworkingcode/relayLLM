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
	"os"
	"strings"
	"time"
)

// ollamaSettings holds Ollama-specific settings on top of the shared base.
type ollamaSettings struct {
	BaseChatSettings
	Think  *bool `json:"think,omitempty"`
	NumCtx *int  `json:"num_ctx,omitempty"`
}

// parseOllamaSettings parses ollamaSettings in a single pass. The embedded
// BaseChatSettings populates from the same unmarshal; fixupMCPServersString
// handles the stringly-encoded mcpServers fallback.
func parseOllamaSettings(raw json.RawMessage) ollamaSettings {
	var s ollamaSettings
	if len(raw) == 0 {
		return s
	}
	_ = json.Unmarshal(raw, &s)
	fixupMCPServersString(raw, &s.MCPServers)
	return s
}

// OllamaChatTransport implements ChatTransport for Ollama's native /api/chat
// endpoint with NDJSON streaming.
type OllamaChatTransport struct {
	baseURL  string
	model    string
	client   *http.Client
	settings ollamaSettings
}

// NewOllamaChatTransport constructs a transport for Ollama. baseURL defaults
// to $OLLAMA_URL or http://localhost:11434 if empty.
func NewOllamaChatTransport(baseURL, model string, settings json.RawMessage, client *http.Client) *OllamaChatTransport {
	if baseURL == "" {
		baseURL = os.Getenv("OLLAMA_URL")
	}
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if client == nil {
		client = &http.Client{}
	}
	return &OllamaChatTransport{
		baseURL:  strings.TrimRight(baseURL, "/"),
		model:    model,
		client:   client,
		settings: parseOllamaSettings(settings),
	}
}

func (t *OllamaChatTransport) Name() string { return "ollama" }

// Ping verifies reachability and auto-detects the model's max context length
// if the user didn't specify one.
func (t *OllamaChatTransport) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("not reachable at %s: %w", t.baseURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/api/tags returned %d", resp.StatusCode)
	}

	if t.settings.NumCtx == nil || *t.settings.NumCtx == 0 {
		if maxCtx := t.fetchModelContextLength(ctx); maxCtx > 0 {
			t.settings.NumCtx = &maxCtx
			slog.Info("ollama: auto-detected context length", "model", t.model, "num_ctx", maxCtx)
		}
	}
	return nil
}

// BuildMessages converts session history into Ollama chat format, including
// system prompt, image attachments (as top-level images: []string), and
// persisted tool_calls / tool-result messages.
func (t *OllamaChatTransport) BuildMessages(systemPrompt string, msgs []Message) []map[string]any {
	result := make([]map[string]any, 0, len(msgs)+1)

	if systemPrompt != "" {
		result = append(result, map[string]any{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	for _, msg := range msgs {
		switch msg.Role {
		case "tool":
			result = append(result, map[string]any{
				"role":    "tool",
				"content": extractTextContent(msg),
			})

		case "assistant":
			entry := map[string]any{
				"role":    "assistant",
				"content": extractTextContent(msg),
			}
			if norm := decodeNormalizedToolCalls(msg.ToolCalls); len(norm) > 0 {
				// Convert normalized → Ollama wire shape (no "id", no "type").
				tc := make([]map[string]any, len(norm))
				for i, n := range norm {
					tc[i] = map[string]any{
						"function": map[string]any{
							"name":      n.Name,
							"arguments": n.Arguments,
						},
					}
				}
				entry["tool_calls"] = tc
			}
			result = append(result, entry)

		default: // "user" and any other role
			entry := map[string]any{
				"role":    msg.Role,
				"content": extractTextContent(msg),
			}
			if len(msg.Files) > 0 {
				images := make([]string, 0, len(msg.Files))
				for _, f := range msg.Files {
					data := f.Data
					if idx := strings.Index(data, ","); idx != -1 && strings.Contains(data[:idx], "base64") {
						data = data[idx+1:]
					}
					images = append(images, data)
				}
				entry["images"] = images
			}
			result = append(result, entry)
		}
	}
	return result
}

// PostChat sends a streaming /api/chat request.
func (t *OllamaChatTransport) PostChat(ctx context.Context, messages []map[string]any, tools []map[string]any) (*http.Response, error) {
	body := t.buildChatBody(messages, tools)
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return resp, nil
}

// buildChatBody constructs the Ollama /api/chat request body.
func (t *OllamaChatTransport) buildChatBody(messages []map[string]any, tools []map[string]any) map[string]any {
	body := map[string]any{
		"model":      t.model,
		"messages":   messages,
		"stream":     true,
		"keep_alive": "30m",
	}
	if options := t.buildOptions(); len(options) > 0 {
		body["options"] = options
	}
	// Explicit think flag — omitting it is not equivalent to false on
	// thinking-capable models (see memory/feedback).
	body["think"] = t.settings.Think != nil && *t.settings.Think
	if len(tools) > 0 {
		body["tools"] = tools
	}
	return body
}

func (t *OllamaChatTransport) buildOptions() map[string]any {
	options := map[string]any{}
	if t.settings.Temperature != nil {
		options["temperature"] = *t.settings.Temperature
	}
	if t.settings.TopP != nil {
		options["top_p"] = *t.settings.TopP
	}
	if t.settings.TopK != nil {
		options["top_k"] = *t.settings.TopK
	}
	if t.settings.MinP != nil {
		options["min_p"] = *t.settings.MinP
	}
	if t.settings.NumCtx != nil && *t.settings.NumCtx > 0 {
		options["num_ctx"] = *t.settings.NumCtx
	}
	return options
}

// ollamaToolCall is one tool call parsed from an Ollama chat chunk.
type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// ollamaChatChunk is one NDJSON line from Ollama's /api/chat stream.
type ollamaChatChunk struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role      string           `json:"role"`
		Content   string           `json:"content"`
		Thinking  string           `json:"thinking"`
		ToolCalls []ollamaToolCall `json:"tool_calls"`
	} `json:"message"`
	Done               bool   `json:"done"`
	DoneReason         string `json:"done_reason"`
	TotalDuration      int64  `json:"total_duration"`
	LoadDuration       int64  `json:"load_duration"`
	PromptEvalCount    int    `json:"prompt_eval_count"`
	PromptEvalDuration int64  `json:"prompt_eval_duration"`
	EvalCount          int    `json:"eval_count"`
	EvalDuration       int64  `json:"eval_duration"`
}

// StreamChunks reads Ollama's NDJSON response, emits text/thinking deltas
// through the provided callback, and returns the accumulated result.
func (t *OllamaChatTransport) StreamChunks(resp *http.Response, startTime time.Time, emit func(ChatDelta)) NormalizedStreamResult {
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	var fullText strings.Builder
	var toolCalls []NormalizedToolCall
	var stats SessionStats

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var chunk ollamaChatChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			slog.Warn("ollama: invalid NDJSON line", "error", err, "line", line)
			continue
		}

		if chunk.Message.Thinking != "" {
			emit(ChatDelta{Thinking: chunk.Message.Thinking})
		}
		if chunk.Message.Content != "" {
			fullText.WriteString(chunk.Message.Content)
			emit(ChatDelta{Text: chunk.Message.Content})
		}

		for _, tc := range chunk.Message.ToolCalls {
			toolCalls = append(toolCalls, NormalizedToolCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}

		if chunk.Done {
			var tps float64
			if chunk.EvalDuration > 0 && chunk.EvalCount > 0 {
				tps = float64(chunk.EvalCount) / (float64(chunk.EvalDuration) / 1e9)
			}
			var ttft float64
			if chunk.TotalDuration > 0 && chunk.EvalDuration > 0 {
				ttft = float64(chunk.TotalDuration-chunk.EvalDuration) / 1e9
			}
			stats = SessionStats{
				InputTokens:          chunk.PromptEvalCount,
				OutputTokens:         chunk.EvalCount,
				TimeToFirstToken:     ttft,
				TokensPerSecond:      tps,
				PromptEvalCount:      chunk.PromptEvalCount,
				EvalDurationMs:       float64(chunk.EvalDuration) / 1e6,
				PromptEvalDurationMs: float64(chunk.PromptEvalDuration) / 1e6,
			}
			return NormalizedStreamResult{FullText: fullText.String(), ToolCalls: toolCalls, Stats: stats}
		}
	}

	if err := scanner.Err(); err != nil {
		return NormalizedStreamResult{FullText: fullText.String(), Err: err}
	}
	return NormalizedStreamResult{FullText: fullText.String(), ToolCalls: toolCalls}
}

// AppendAssistantWithToolCalls adds an assistant-with-tool-calls entry to the
// running messages array in Ollama's wire shape (no "id", no "type").
func (t *OllamaChatTransport) AppendAssistantWithToolCalls(messages []map[string]any, text string, toolCalls []NormalizedToolCall) []map[string]any {
	ollamaTC := make([]map[string]any, len(toolCalls))
	for i, tc := range toolCalls {
		ollamaTC[i] = map[string]any{
			"function": map[string]any{
				"name":      tc.Name,
				"arguments": tc.Arguments,
			},
		}
	}
	return append(messages, map[string]any{
		"role":       "assistant",
		"content":    text,
		"tool_calls": ollamaTC,
	})
}

// AppendToolResult adds a tool result entry. Ollama does not use tool_call_id.
func (t *OllamaChatTransport) AppendToolResult(messages []map[string]any, _ NormalizedToolCall, result string) []map[string]any {
	return append(messages, map[string]any{
		"role":    "tool",
		"content": result,
	})
}

// fetchModelContextLength queries /api/show for the model's max context length.
// The context_length field is nested under model_info with a model-family prefix
// (e.g. "gemma4.context_length"), so we scan all keys.
func (t *OllamaChatTransport) fetchModelContextLength(ctx context.Context) int {
	body, _ := json.Marshal(map[string]string{"name": t.model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0
	}

	var result struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0
	}

	for key, val := range result.ModelInfo {
		if strings.HasSuffix(key, ".context_length") {
			switch v := val.(type) {
			case float64:
				return int(v)
			case json.Number:
				n, _ := v.Int64()
				return int(n)
			}
		}
	}
	return 0
}

// fetchOllamaModels queries Ollama's /api/tags endpoint and returns available
// models in ModelInfo form for the /api/models aggregation route.
func fetchOllamaModels(baseURL string) []ModelInfo {
	client := &http.Client{Timeout: 3 * time.Second}

	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/tags", nil)
	if err != nil {
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("ollama: model discovery failed", "error", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("ollama: model discovery returned non-OK status", "status", resp.StatusCode, "url", baseURL)
		return nil
	}

	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	models := make([]ModelInfo, 0, len(result.Models))
	for _, m := range result.Models {
		models = append(models, ModelInfo{
			Label:    m.Name,
			Value:    m.Name,
			Group:    "Ollama",
			Provider: "ollama",
		})
	}
	return models
}
