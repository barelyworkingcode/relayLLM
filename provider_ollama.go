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
	"sync"
	"sync/atomic"
	"time"
)

// ollamaSettings holds parsed per-session settings for Ollama.
type ollamaSettings struct {
	Temperature   *float64                       `json:"temperature,omitempty"`
	TopP          *float64                       `json:"top_p,omitempty"`
	TopK          *int                           `json:"top_k,omitempty"`
	MinP          *float64                       `json:"min_p,omitempty"`
	Think         *bool                          `json:"think,omitempty"`
	NumCtx        *int                           `json:"num_ctx,omitempty"`
	UseRelayTools *bool                          `json:"useRelayTools,omitempty"`
	MCPServers    map[string]MCPServerConfig     `json:"mcpServers,omitempty"`
}

// OllamaProvider sends full conversation history to Ollama's native /api/chat
// endpoint with NDJSON streaming. Relies on Ollama's automatic KV cache prefix
// reuse for efficient multi-turn conversations.
type OllamaProvider struct {
	session    *Session
	handler    EventHandler
	baseURL    string
	model      string
	settings   ollamaSettings
	client     *http.Client
	mu         sync.Mutex
	started    atomic.Bool
	cancelFn   context.CancelFunc
	mcpManager *MCPManager
}

func NewOllamaProvider(session *Session, handler EventHandler, baseURL string, settings json.RawMessage) *OllamaProvider {
	if baseURL == "" {
		baseURL = os.Getenv("OLLAMA_URL")
	}
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	var parsed ollamaSettings
	if len(settings) > 0 {
		json.Unmarshal(settings, &parsed)

		// Handle mcpServers arriving as a JSON string (e.g. from Eve's text input)
		// instead of a parsed object. Try to extract and re-parse it.
		if len(parsed.MCPServers) == 0 {
			var raw map[string]json.RawMessage
			if json.Unmarshal(settings, &raw) == nil {
				if mcpRaw, ok := raw["mcpServers"]; ok {
					var asString string
					if json.Unmarshal(mcpRaw, &asString) == nil && asString != "" {
						json.Unmarshal([]byte(asString), &parsed.MCPServers)
					}
				}
			}
		}
	}
	p := &OllamaProvider{
		session:  session,
		handler:  handler,
		baseURL:  strings.TrimRight(baseURL, "/"),
		model:    session.Model,
		settings: parsed,
		client:   &http.Client{Timeout: 0},
	}
	// Build Relay MCP config from env vars when useRelayTools is enabled.
	if parsed.UseRelayTools != nil && *parsed.UseRelayTools {
		if cmd := os.Getenv("RELAY_MCP_COMMAND"); cmd != "" {
			if parsed.MCPServers == nil {
				parsed.MCPServers = make(map[string]MCPServerConfig)
			}
			parsed.MCPServers["relay"] = MCPServerConfig{
				Command: cmd,
				Args:    []string{"mcp"},
				Env:     map[string]string{"RELAY_TOKEN": os.Getenv("RELAY_MCP_TOKEN")},
			}
		} else {
			slog.Warn("useRelayTools enabled but RELAY_MCP_COMMAND not set")
		}
	}

	if len(parsed.MCPServers) > 0 {
		p.mcpManager = NewMCPManager(parsed.MCPServers)
	}
	return p
}

func (p *OllamaProvider) Start() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("ollama: build request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: not reachable at %s: %w", p.baseURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: /api/tags returned %d", resp.StatusCode)
	}

	// Auto-detect max context size if user didn't set num_ctx.
	if p.settings.NumCtx == nil || *p.settings.NumCtx == 0 {
		if maxCtx := p.fetchModelContextLength(ctx); maxCtx > 0 {
			p.settings.NumCtx = &maxCtx
			slog.Info("ollama: auto-detected context length", "model", p.model, "num_ctx", maxCtx)
		}
	}

	p.started.Store(true)

	if p.mcpManager != nil {
		mcpCtx, mcpCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer mcpCancel()
		if err := p.mcpManager.Start(mcpCtx); err != nil {
			slog.Warn("ollama: MCP servers failed to start (tool calling disabled)", "session", p.session.ID, "error", err)
			p.mcpManager = nil
		}
	}

	slog.Info("ollama provider started", "session", p.session.ID, "model", p.model, "baseURL", p.baseURL)
	return nil
}

func (p *OllamaProvider) SendMessage(text string, files []FileAttachment) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.started.Load() {
		return fmt.Errorf("ollama provider not started")
	}

	messages := p.buildMessages()

	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFn = cancel

	resp, err := p.postChat(ctx, messages)
	if err != nil {
		cancel()
		return fmt.Errorf("ollama: %w", err)
	}

	go p.runToolLoop(ctx, cancel, resp, messages, time.Now())
	return nil
}

// ollamaToolCall captures a single tool call from an Ollama response.
type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// ollamaChatChunk represents one NDJSON line from Ollama's /api/chat stream.
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

// streamResult captures the output of streaming a single Ollama response.
type streamResult struct {
	fullText  string
	toolCalls []ollamaToolCall
	stats     SessionStats
	err       error
}

// runToolLoop streams the initial response and, if tool calls are present,
// executes them via MCP and sends results back to Ollama in a loop.
// This runs entirely in a goroutine — the session layer only sees streaming
// text events and eventually message_complete.
func (p *OllamaProvider) runToolLoop(ctx context.Context, cancel context.CancelFunc, resp *http.Response, messages []map[string]interface{}, startTime time.Time) {
	defer cancel()

	const maxIterations = 10
	var allText strings.Builder
	var toolMessages []Message // accumulated tool-related messages for session history

	for iteration := 0; iteration <= maxIterations; iteration++ {
		result := p.streamAndCollect(resp)
		allText.WriteString(result.fullText)

		if result.err != nil {
			slog.Error("ollama: stream error", "session", p.session.ID, "error", result.err)
			p.emitError(result.err.Error())
			return
		}

		// No tool calls or MCP not available — we're done.
		if len(result.toolCalls) == 0 || p.mcpManager == nil || iteration == maxIterations {
			statsData, _ := json.Marshal(result.stats)
			p.handler("stats_update", statsData)

			// Inject accumulated tool messages into session history.
			if len(toolMessages) > 0 {
				p.session.mu.Lock()
				p.session.Messages = append(p.session.Messages, toolMessages...)
				p.session.mu.Unlock()
			}

			completeData, _ := json.Marshal(map[string]string{"text": allText.String()})
			p.handler("message_complete", completeData)
			return
		}

		// Tool calls detected — execute them.
		// 1. Build assistant message with tool_calls for history.
		tcJSON, _ := json.Marshal(result.toolCalls)
		assistantContent, _ := json.Marshal(result.fullText)
		toolMessages = append(toolMessages, Message{
			Timestamp: timeNow(),
			Role:      "assistant",
			Content:   assistantContent,
			ToolCalls: tcJSON,
		})

		// Also add to the Ollama message list for the next request.
		assistantEntry := map[string]interface{}{
			"role":    "assistant",
			"content": result.fullText,
		}
		// Rebuild tool_calls in Ollama format for the conversation.
		ollamaTC := make([]map[string]interface{}, len(result.toolCalls))
		for i, tc := range result.toolCalls {
			ollamaTC[i] = map[string]interface{}{
				"function": map[string]interface{}{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			}
		}
		assistantEntry["tool_calls"] = ollamaTC
		messages = append(messages, assistantEntry)

		// 2. Execute each tool call.
		for _, tc := range result.toolCalls {
			toolName := tc.Function.Name
			p.emitTextDelta(fmt.Sprintf("\n**[Calling: %s]**\n", toolName))

			toolResult, err := p.mcpManager.CallTool(ctx, toolName, tc.Function.Arguments)
			if err != nil {
				toolResult = fmt.Sprintf("Error: %s", err.Error())
				slog.Warn("ollama: tool call failed", "tool", toolName, "error", err)
			}

			// Truncate large tool results to avoid blowing up context.
			const maxResultLen = 8192
			if len(toolResult) > maxResultLen {
				toolResult = toolResult[:maxResultLen] + "\n...(truncated)"
			}

			// Show result preview to user.
			preview := toolResult
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			p.emitTextDelta(fmt.Sprintf("**[Result: %s]**\n\n", preview))

			// Store tool result message.
			resultContent, _ := json.Marshal(toolResult)
			toolMessages = append(toolMessages, Message{
				Timestamp: timeNow(),
				Role:      "tool",
				Content:   resultContent,
				ToolName:  toolName,
			})

			// Add to Ollama message list.
			messages = append(messages, map[string]interface{}{
				"role":    "tool",
				"content": toolResult,
			})
		}

		// 3. Send follow-up request to Ollama with updated messages.
		var err error
		resp, err = p.postChat(ctx, messages)
		if err != nil {
			p.emitError(err.Error())
			return
		}
	}
}

// streamAndCollect reads a single Ollama NDJSON response, streaming deltas
// to the client and collecting the full text and any tool calls.
func (p *OllamaProvider) streamAndCollect(resp *http.Response) streamResult {
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	var fullText strings.Builder
	var toolCalls []ollamaToolCall
	var stats SessionStats
	sentStart := false
	thinkingActive := false

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

		if !sentStart {
			sentStart = true
			event := map[string]interface{}{
				"type": "assistant",
				"message": map[string]interface{}{
					"content": []interface{}{},
				},
			}
			eventData, _ := json.Marshal(event)
			p.handler("llm_event", eventData)
		}

		if chunk.Message.Thinking != "" {
			if !thinkingActive {
				thinkingActive = true
				p.emitTextDelta("<think>\n")
			}
			p.emitTextDelta(chunk.Message.Thinking)
		}

		if chunk.Message.Content != "" {
			if thinkingActive {
				thinkingActive = false
				p.emitTextDelta("\n</think>\n\n")
			}
			fullText.WriteString(chunk.Message.Content)
			p.emitTextDelta(chunk.Message.Content)
		}

		// Collect tool calls.
		for _, tc := range chunk.Message.ToolCalls {
			p.emitTextDelta(fmt.Sprintf("\n[Tool: %s]\n", tc.Function.Name))
			if len(tc.Function.Arguments) > 0 {
				p.emitTextDelta(string(tc.Function.Arguments) + "\n")
			}
			toolCalls = append(toolCalls, ollamaToolCall{Function: tc.Function})
		}

		if chunk.Done {
			if thinkingActive {
				p.emitTextDelta("\n</think>\n\n")
			}

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

			return streamResult{fullText: fullText.String(), toolCalls: toolCalls, stats: stats}
		}
	}

	if err := scanner.Err(); err != nil {
		return streamResult{fullText: fullText.String(), err: err}
	}

	return streamResult{fullText: fullText.String(), toolCalls: toolCalls}
}

// timeNow returns the current UTC time formatted as RFC3339.
func timeNow() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// buildOptions returns the Ollama options sub-object from provider settings.
func (p *OllamaProvider) buildOptions() map[string]interface{} {
	options := map[string]interface{}{}
	if p.settings.Temperature != nil {
		options["temperature"] = *p.settings.Temperature
	}
	if p.settings.TopP != nil {
		options["top_p"] = *p.settings.TopP
	}
	if p.settings.TopK != nil {
		options["top_k"] = *p.settings.TopK
	}
	if p.settings.MinP != nil {
		options["min_p"] = *p.settings.MinP
	}
	if p.settings.NumCtx != nil && *p.settings.NumCtx > 0 {
		options["num_ctx"] = *p.settings.NumCtx
	}
	return options
}

// buildMessages converts session.Messages into Ollama chat format,
// including system prompt, image attachments, and tool messages.
func (p *OllamaProvider) buildMessages() []map[string]interface{} {
	p.session.mu.Lock()
	msgs := make([]Message, len(p.session.Messages))
	copy(msgs, p.session.Messages)
	p.session.mu.Unlock()

	result := make([]map[string]interface{}, 0, len(msgs)+1)

	// Prepend system prompt if set.
	if p.session.SystemPrompt != "" {
		result = append(result, map[string]interface{}{
			"role":    "system",
			"content": p.session.SystemPrompt,
		})
	}

	for _, msg := range msgs {
		switch msg.Role {
		case "tool":
			entry := map[string]interface{}{
				"role":    "tool",
				"content": extractTextContent(msg),
			}
			result = append(result, entry)

		case "assistant":
			entry := map[string]interface{}{
				"role":    "assistant",
				"content": extractTextContent(msg),
			}
			// Restore tool_calls if present on this assistant message.
			if len(msg.ToolCalls) > 0 {
				var tc []map[string]interface{}
				if json.Unmarshal(msg.ToolCalls, &tc) == nil {
					entry["tool_calls"] = tc
				}
			}
			result = append(result, entry)

		default: // "user" and any other role
			entry := map[string]interface{}{
				"role":    msg.Role,
				"content": extractTextContent(msg),
			}
			// Attach images as raw base64 strings (Ollama format).
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

// buildChatBody constructs the Ollama /api/chat request body with all settings.
func (p *OllamaProvider) buildChatBody(messages []map[string]interface{}) map[string]interface{} {
	body := map[string]interface{}{
		"model":      p.model,
		"messages":   messages,
		"stream":     true,
		"keep_alive": "30m",
	}
	if options := p.buildOptions(); len(options) > 0 {
		body["options"] = options
	}
	if p.settings.Think != nil && *p.settings.Think {
		body["think"] = true
	} else {
		body["think"] = false
	}
	if p.mcpManager != nil && p.mcpManager.HasTools() {
		body["tools"] = p.mcpManager.OllamaToolDefs()
	}
	return body
}

// postChat sends a streaming chat request to Ollama and returns the response.
func (p *OllamaProvider) postChat(ctx context.Context, messages []map[string]interface{}) (*http.Response, error) {
	data, err := json.Marshal(p.buildChatBody(messages))
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return resp, nil
}

// emitError sends a safely JSON-encoded error event to the client.
func (p *OllamaProvider) emitError(msg string) {
	data, _ := json.Marshal(map[string]string{"error": msg})
	p.handler("error", data)
}

func (p *OllamaProvider) emitTextDelta(text string) {
	event := map[string]interface{}{
		"type": "assistant",
		"delta": map[string]interface{}{
			"type": "text_delta",
			"text": text,
		},
	}
	eventData, _ := json.Marshal(event)
	p.handler("llm_event", eventData)
}

func (p *OllamaProvider) StopGeneration() {
	p.mu.Lock()
	cancel := p.cancelFn
	p.cancelFn = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	slog.Info("ollama generation stopped", "session", p.session.ID)
}

func (p *OllamaProvider) Kill() {
	p.StopGeneration()
	if p.mcpManager != nil {
		p.mcpManager.Close()
	}
	p.started.Store(false)
	slog.Info("ollama provider killed", "session", p.session.ID)
}

func (p *OllamaProvider) DeleteSession() error { return nil }
func (p *OllamaProvider) Alive() bool          { return p.started.Load() }
func (p *OllamaProvider) GetState() json.RawMessage {
	return json.RawMessage(`{}`)
}
func (p *OllamaProvider) RestoreState(state json.RawMessage) {}

// fetchModelContextLength queries /api/show for the model's max context length.
// The context_length field is nested under model_info with a model-family prefix
// (e.g. "gemma4.context_length"), so we scan all keys.
func (p *OllamaProvider) fetchModelContextLength(ctx context.Context) int {
	body, _ := json.Marshal(map[string]string{"name": p.model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0
	}

	var result struct {
		ModelInfo map[string]interface{} `json:"model_info"`
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

// fetchOllamaModels queries the Ollama /api/tags endpoint and returns available models.
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
