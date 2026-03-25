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

// lmStudioSettings holds parsed per-session settings for LM Studio.
type lmStudioSettings struct {
	Temperature   *float64 `json:"temperature,omitempty"`
	Reasoning     *bool    `json:"reasoning,omitempty"`
	ContextLength *int     `json:"contextLength,omitempty"`
	Integrations  []string `json:"integrations,omitempty"`
}

// LMStudioProvider manages an LM Studio HTTP connection using the native /api/v1/chat endpoint.
type LMStudioProvider struct {
	session    *Session
	handler    EventHandler
	baseURL    string
	apiToken   string
	model      string
	settings   lmStudioSettings
	client     *http.Client
	mu         sync.Mutex
	started    atomic.Bool
	cancelFn   context.CancelFunc
	responseID string // last response_id for stateful chain
}

func NewLMStudioProvider(session *Session, handler EventHandler, baseURL string, settings json.RawMessage) *LMStudioProvider {
	var parsed lmStudioSettings
	if len(settings) > 0 {
		json.Unmarshal(settings, &parsed)
	}
	return &LMStudioProvider{
		session:  session,
		handler:  handler,
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiToken: os.Getenv("LM_STUDIO_API_TOKEN"),
		model:    session.Model,
		settings: parsed,
		client:   &http.Client{Timeout: 0}, // no timeout — streaming
	}
}

func (p *LMStudioProvider) Start() error {
	// Verify LM Studio is reachable by hitting the models endpoint.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("lmstudio: build request: %w", err)
	}
	if p.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiToken)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("lmstudio: not reachable at %s: %w", p.baseURL, err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lmstudio: /v1/models returned %d", resp.StatusCode)
	}

	p.started.Store(true)
	slog.Info("lmstudio provider started", "session", p.session.ID, "model", p.model, "baseURL", p.baseURL)
	return nil
}

func (p *LMStudioProvider) SendMessage(text string, files []FileAttachment) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.started.Load() {
		return fmt.Errorf("lmstudio provider not started")
	}

	// Build input field.
	var input interface{}
	if len(files) > 0 {
		// Array format with images + message.
		var parts []interface{}
		for _, f := range files {
			dataURL := fmt.Sprintf("data:%s;base64,%s", f.MimeType, f.Data)
			parts = append(parts, map[string]string{
				"type":     "image",
				"data_url": dataURL,
			})
		}
		parts = append(parts, map[string]string{
			"type":    "message",
			"content": text,
		})
		input = parts
	} else {
		input = text
	}

	body := map[string]interface{}{
		"model":  p.model,
		"input":  input,
		"stream": true,
	}
	if p.responseID != "" {
		body["previous_response_id"] = p.responseID
	}
	if p.settings.Temperature != nil {
		body["temperature"] = *p.settings.Temperature
	}
	if p.settings.Reasoning != nil && *p.settings.Reasoning {
		body["reasoning"] = true
	}
	if p.settings.ContextLength != nil && *p.settings.ContextLength > 0 {
		body["context_length"] = *p.settings.ContextLength
	}
	if len(p.settings.Integrations) > 0 {
		body["integrations"] = p.settings.Integrations
	}

	err := p.doSend(body)
	if err != nil && p.responseID != "" && isResponseIDError(err) {
		// Retry without previous_response_id — LM Studio may have restarted.
		slog.Warn("lmstudio: previous_response_id rejected, retrying fresh", "session", p.session.ID)
		p.responseID = ""
		delete(body, "previous_response_id")
		err = p.doSend(body)
	}
	return err
}

func isResponseIDError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "previous_response_id") ||
		strings.Contains(msg, "404") ||
		strings.Contains(msg, "not found")
}

func (p *LMStudioProvider) doSend(body map[string]interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("lmstudio: marshal request: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFn = cancel

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/v1/chat", bytes.NewReader(data))
	if err != nil {
		cancel()
		return fmt.Errorf("lmstudio: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiToken)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return fmt.Errorf("lmstudio: request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		return fmt.Errorf("lmstudio: HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	go p.streamResponse(resp, cancel)
	return nil
}

func (p *LMStudioProvider) streamResponse(resp *http.Response, cancel context.CancelFunc) {
	defer resp.Body.Close()
	defer cancel()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	var fullText strings.Builder
	var reasoning strings.Builder
	var currentEvent string

	state := &streamState{}

	for scanner.Scan() {
		line := scanner.Text()

		// SSE format: "event: <name>" followed by "data: <json>"
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		if strings.HasPrefix(line, "data:") {
			dataStr := strings.TrimPrefix(line, "data:")
			dataStr = strings.TrimSpace(dataStr)

			if dataStr == "[DONE]" {
				break
			}

			p.handleSSEEvent(currentEvent, []byte(dataStr), &fullText, &reasoning, state)
			currentEvent = ""
			continue
		}

		// Empty lines separate events in SSE — just continue.
	}

	if err := scanner.Err(); err != nil {
		slog.Error("lmstudio: stream read error", "session", p.session.ID, "error", err)
		errData, _ := json.Marshal(err.Error())
		p.handler("error", errData)
	}
}

// streamState tracks per-stream state across SSE events.
type streamState struct {
	reasoningStarted bool // saw reasoning.start, streaming <think> tag
	messageStarted   bool // saw message.start, closed </think> if needed
}

// handleSSEEvent normalizes LM Studio SSE events into Claude's stream-json format
// so Eve can render them without provider-specific logic.
// Reasoning is streamed inline as <think>...</think> tags which Eve renders as collapsible blocks.
func (p *LMStudioProvider) handleSSEEvent(eventName string, data []byte, fullText *strings.Builder, reasoning *strings.Builder, state *streamState) {
	switch eventName {
	case "reasoning.start":
		state.reasoningStarted = true
		// Start the assistant message bubble with an opening <think> tag.
		event := map[string]interface{}{
			"type": "assistant",
			"message": map[string]interface{}{
				"content": []interface{}{},
			},
		}
		eventData, _ := json.Marshal(event)
		p.handler("llm_event", eventData)

		p.emitTextDelta("<think>\n")

	case "reasoning.delta":
		var delta struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(data, &delta); err != nil {
			return
		}
		reasoning.WriteString(delta.Content)
		p.emitTextDelta(delta.Content)

	case "reasoning.end":
		// Close the think block.
		p.emitTextDelta("\n</think>\n\n")

	case "message.start":
		state.messageStarted = true
		// If there was no reasoning phase, start the assistant message now.
		if !state.reasoningStarted {
			event := map[string]interface{}{
				"type": "assistant",
				"message": map[string]interface{}{
					"content": []interface{}{},
				},
			}
			eventData, _ := json.Marshal(event)
			p.handler("llm_event", eventData)
		}

	case "message.delta":
		var delta struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(data, &delta); err != nil {
			return
		}
		fullText.WriteString(delta.Content)
		p.emitTextDelta(delta.Content)

	case "tool_call.start":
		var tc struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &tc); err == nil {
			p.emitTextDelta(fmt.Sprintf("\n[Tool: %s]\n", tc.Name))
		}
		p.handler("llm_event", data)

	case "tool_call.arguments":
		p.handler("llm_event", data)

	case "tool_call.success":
		var tc struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(data, &tc); err == nil {
			summary := tc.Output
			if len(summary) > 200 {
				summary = summary[:200] + "..."
			}
			p.emitTextDelta(fmt.Sprintf("[Result: %s]\n", summary))
		}
		p.handler("llm_event", data)

	case "tool_call.failure":
		var tc struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &tc); err == nil {
			p.emitTextDelta(fmt.Sprintf("[Tool Error: %s]\n", tc.Error))
		}
		p.handler("llm_event", data)

	case "chat.end":
		var envelope struct {
			Result struct {
				ResponseID string `json:"response_id"`
				Output     []struct {
					Type    string `json:"type"`
					Content string `json:"content"`
				} `json:"output"`
				Stats *struct {
					InputTokens       int     `json:"input_tokens"`
					TotalOutputTokens int     `json:"total_output_tokens"`
					ReasoningTokens   int     `json:"reasoning_output_tokens"`
					TokensPerSecond   float64 `json:"tokens_per_second"`
					TimeToFirstToken  float64 `json:"time_to_first_token_seconds"`
				} `json:"stats"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &envelope); err == nil {
			if envelope.Result.ResponseID != "" {
				p.mu.Lock()
				p.responseID = envelope.Result.ResponseID
				p.mu.Unlock()
			}

			if envelope.Result.Stats != nil {
				s := envelope.Result.Stats
				stats := SessionStats{
					InputTokens:  s.InputTokens,
					OutputTokens: s.TotalOutputTokens,
				}
				statsData, _ := json.Marshal(stats)
				p.handler("stats_update", statsData)
			}

			// Extract authoritative text from the output array when present.
			// This captures tool output text that may not appear in streaming deltas.
			var outputText strings.Builder
			for _, entry := range envelope.Result.Output {
				if entry.Type == "message" && entry.Content != "" {
					outputText.WriteString(entry.Content)
				}
			}
			if outputText.Len() > 0 {
				fullText.Reset()
				fullText.WriteString(outputText.String())
			}
		}

		// Emit message_complete with the full text so session can store assistant message.
		completeData, _ := json.Marshal(map[string]string{
			"text": fullText.String(),
		})
		p.handler("message_complete", completeData)

	case "error":
		p.handler("error", data)

	default:
		if len(data) > 0 && eventName != "" {
			event := map[string]interface{}{
				"type": eventName,
				"data": json.RawMessage(data),
			}
			eventData, _ := json.Marshal(event)
			p.handler("llm_event", eventData)
		}
	}
}

// emitTextDelta sends a normalized Claude-format text_delta event.
func (p *LMStudioProvider) emitTextDelta(text string) {
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

func (p *LMStudioProvider) StopGeneration() {
	p.mu.Lock()
	cancel := p.cancelFn
	p.cancelFn = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	slog.Info("lmstudio generation stopped", "session", p.session.ID)
}

func (p *LMStudioProvider) Kill() {
	p.StopGeneration()
	p.started.Store(false)
	slog.Info("lmstudio provider killed", "session", p.session.ID)
}

func (p *LMStudioProvider) Alive() bool {
	return p.started.Load()
}

func (p *LMStudioProvider) DeleteSession() error {
	return nil
}

func (p *LMStudioProvider) GetState() json.RawMessage {
	p.mu.Lock()
	rid := p.responseID
	p.mu.Unlock()
	state := map[string]string{
		"responseId": rid,
	}
	data, _ := json.Marshal(state)
	return data
}

func (p *LMStudioProvider) RestoreState(state json.RawMessage) {
	if state == nil {
		return
	}
	var s struct {
		ResponseID string `json:"responseId"`
	}
	if err := json.Unmarshal(state, &s); err == nil {
		p.mu.Lock()
		p.responseID = s.ResponseID
		p.mu.Unlock()
	}
}

// fetchLMStudioModels queries the LM Studio /v1/models endpoint and returns available models.
func fetchLMStudioModels(baseURL string) []ModelInfo {
	client := &http.Client{Timeout: 3 * time.Second}
	apiToken := os.Getenv("LM_STUDIO_API_TOKEN")

	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil
	}
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("lmstudio: model discovery failed", "error", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
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
		models = append(models, ModelInfo{
			Label:    m.ID,
			Value:    m.ID,
			Group:    "LM Studio",
			Provider: "lmstudio",
		})
	}
	return models
}
