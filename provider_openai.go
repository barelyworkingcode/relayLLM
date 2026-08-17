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
	"sync"
	"sync/atomic"
	"time"
)

// BackendAcquirer is implemented by transports whose backend is a managed
// process that can be stopped between turns. BaseChatProvider calls
// AcquireBackend before each turn and holds the returned release until the
// tool loop finishes, which both pins the process against eviction and lets
// the transport re-resolve an endpoint whose port may have changed.
//
// Transports without a managed backend simply don't implement it.
type BackendAcquirer interface {
	AcquireBackend(ctx context.Context) (release func(), err error)
}

// BackendResolver launches-or-reuses a managed server and returns the endpoint
// to talk to plus a lease release. See ServerManager.Acquire.
type BackendResolver func() (OpenAIEndpoint, func(), error)

// OpenAIChatTransport implements ChatTransport for any server that speaks
// the OpenAI /v1/chat/completions protocol (OpenAI itself, LM Studio, Ollama's
// /v1 compat layer, OMLX, llama.cpp server, etc.).
type OpenAIChatTransport struct {
	// endpointMu guards endpoint, which a BackendResolver rewrites between
	// turns. Reads go through ep(); a turn's tool loop runs on a different
	// goroutine than the SendMessage that resolved it, so this is not
	// theoretical.
	endpointMu sync.RWMutex
	endpoint   OpenAIEndpoint

	resolve  BackendResolver // nil for plain HTTP endpoints
	model    string          // bare model id (after prefix stripping)
	client   *http.Client
	settings BaseChatSettings

	// iterCounter scopes synthesized tool_call IDs to a specific invocation of
	// AppendAssistantWithToolCalls so two successive tool-using turns within
	// one conversation don't reuse the same ID (which trips strict gateways).
	iterCounter atomic.Uint64
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

// NewManagedChatTransport constructs a transport backed by a managed server.
// The endpoint is resolved per turn via resolve rather than pinned at
// construction, so the session survives the backing process being evicted and
// relaunched on a different port.
func NewManagedChatTransport(resolve BackendResolver, model string, settings json.RawMessage, client *http.Client) *OpenAIChatTransport {
	t := NewOpenAIChatTransport(OpenAIEndpoint{}, model, settings, client)
	t.resolve = resolve
	return t
}

// ep returns a snapshot of the current endpoint.
func (t *OpenAIChatTransport) ep() OpenAIEndpoint {
	t.endpointMu.RLock()
	defer t.endpointMu.RUnlock()
	return t.endpoint
}

// AcquireBackend resolves the managed backend for the coming turn and returns
// its lease release. A transport with no resolver is a plain HTTP endpoint:
// nothing to acquire, and the release is a no-op.
func (t *OpenAIChatTransport) AcquireBackend(ctx context.Context) (func(), error) {
	if t.resolve == nil {
		return func() {}, nil
	}
	endpoint, release, err := t.resolve()
	if err != nil {
		return nil, err
	}
	t.endpointMu.Lock()
	t.endpoint = endpoint
	t.endpointMu.Unlock()
	return release, nil
}

func (t *OpenAIChatTransport) Name() string { return "openai:" + t.ep().Name }

// Ping verifies the endpoint is reachable by calling /models. We accept any
// 2xx as healthy, and 404 as "endpoint up but /models not implemented" — some
// compat servers (custom proxies, certain llama.cpp builds) don't ship a
// model-listing endpoint, and rejecting them here would make the transport
// unusable for no good reason. 401/403 still surface as errors because they
// indicate misconfigured auth that the chat call would also fail on.
func (t *OpenAIChatTransport) Ping(ctx context.Context) error {
	// A managed backend has no endpoint until the first turn acquires one, and
	// pinging it would mean launching the model at session-create time — the
	// eager behavior the budget exists to avoid. Readiness is instead proven
	// by the health check inside ServerManager.Acquire.
	if t.resolve != nil {
		return nil
	}
	ep := t.ep()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.BaseURL+"/models", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	t.addAuth(req)

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("not reachable at %s: %w", ep.BaseURL, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		slog.Debug("openai: /models not implemented, treating endpoint as healthy",
			"endpoint", ep.Name)
		return nil
	default:
		return fmt.Errorf("/models returned %d", resp.StatusCode)
	}
}

// addAuth attaches a Bearer token when the endpoint has an API key set.
// No-op for endpoints (like local Ollama) that don't require auth.
func (t *OpenAIChatTransport) addAuth(req *http.Request) {
	if key := t.ep().APIKey; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
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
	// can attach tool_call_id to tool-role messages. Any non-tool message
	// resets the pairing window so a stale assistant from an earlier turn
	// can't be re-used by tool messages that arrive after a user turn.
	var lastAssistantCalls []NormalizedToolCall
	var toolResultIdx int

	for idx, msg := range msgs {
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
					// Scope to the assistant's history position so this id
					// matches whatever the assistant entry synthesized above.
					entry["tool_call_id"] = synthesizeToolCallID(
						scopeForHistoryMsg(idx-toolResultIdx-1), msg.ToolName, toolResultIdx)
				}
				toolResultIdx++
			}
			result = append(result, entry)

		case "assistant":
			entry := map[string]any{
				"role":    "assistant",
				"content": extractTextContent(msg),
			}
			if norm := toolCallsFromContent(msg.Content); len(norm) > 0 {
				// Mutate in place so any synthesized IDs propagate to the
				// tool-result pairing pass below. Scope the synthesized id by
				// the assistant's history position so an identically-shaped
				// turn that appears later in the conversation produces a
				// different id (strict gateways reject duplicates).
				scope := scopeForHistoryMsg(idx)
				for i := range norm {
					if norm[i].ID == "" {
						norm[i].ID = synthesizeToolCallID(scope, norm[i].Name, i)
					}
				}
				entry["tool_calls"] = buildOpenAIToolCallEntries(scope, norm)
				lastAssistantCalls = norm
			} else {
				lastAssistantCalls = nil
			}
			toolResultIdx = 0
			result = append(result, entry)

		default: // "user" and any other role
			// Reset pairing window — a user turn ends the tool-result run that
			// belongs to the previous assistant.
			lastAssistantCalls = nil
			toolResultIdx = 0

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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.ep().BaseURL+"/chat/completions", bytes.NewReader(data))
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
		return nil, decodeChatError(resp.StatusCode, bodyBytes)
	}
	return resp, nil
}

// decodeChatError turns a non-2xx response body into a user-facing error.
// OpenAI-compatible servers return {"error":{"message":...}}; we surface that
// directly and translate known patterns (e.g. llama-server's image rejection
// when the model can't parse the attached image) into actionable messages.
func decodeChatError(status int, body []byte) error {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}
	if strings.Contains(msg, "Failed to load image") {
		return fmt.Errorf("couldn't read the attached image — the model may not support this format. Try a standard JPEG or PNG (not CMYK, not progressive)")
	}
	return fmt.Errorf("HTTP %d: %s", status, msg)
}

func (t *OpenAIChatTransport) buildChatBody(messages []map[string]any, tools []map[string]any) map[string]any {
	strict := t.ep().Strict
	body := map[string]any{
		"model":    t.model,
		"messages": messages,
		"stream":   true,
	}
	// stream_options.include_usage is a real OpenAI field that most compat
	// servers ignore safely — but older LM Studio releases and stricter
	// gateways 400 on unknown body fields, so gate it on Strict.
	if !strict {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if t.settings.Temperature != nil {
		body["temperature"] = *t.settings.Temperature
	}
	if t.settings.TopP != nil {
		body["top_p"] = *t.settings.TopP
	}
	if t.settings.TopK != nil && !strict {
		// Not standard OpenAI, but most compatible servers (LM Studio, Ollama /v1)
		// accept it as an extension. OpenAI proper rejects unknown fields on
		// stricter API versions, so omit it when Strict is on.
		body["top_k"] = *t.settings.TopK
	}
	if t.settings.MinP != nil && !strict {
		body["min_p"] = *t.settings.MinP
	}
	if t.settings.RepetitionPenalty != nil && !strict {
		body["repetition_penalty"] = *t.settings.RepetitionPenalty
	}
	if t.settings.PresencePenalty != nil {
		body["presence_penalty"] = *t.settings.PresencePenalty
	}
	if t.settings.MaxTokens != nil {
		body["max_tokens"] = *t.settings.MaxTokens
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

// StreamChunks reads the SSE-formatted response body, emits text/thinking/tool
// deltas through the provided callback, and returns the accumulated result.
//
// SSE format: each event is `data: <json>\n\n`, terminated by `data: [DONE]`.
// Tool calls arrive incrementally: the first delta for a new tool call carries
// the id + name, and subsequent deltas carry argument fragments. The transport
// forwards both as ToolStart / ToolArgs events; BaseChatProvider's stream
// state machine handles ordering and final assembly.
func (t *OpenAIChatTransport) StreamChunks(resp *http.Response, startTime time.Time, emit func(ChatDelta)) NormalizedStreamResult {
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	var fullText strings.Builder
	var usage *openAIUsage
	var firstTokenAt time.Time

	// Track which tool-call indices we've already emitted ToolStart for.
	// The OpenAI wire shape sends id/name on the first chunk, args on
	// subsequent chunks — but rarely, both arrive in the same chunk.
	startedTools := make(map[int]bool)

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
			// Emit ToolStart on the first chunk that carries a name for
			// this tool-call index. The OpenAI spec guarantees name + id
			// arrive together on the first delta.
			if !startedTools[tcd.Index] && tcd.Function.Name != "" {
				emit(ChatDelta{ToolStart: &ToolStartEvent{
					Index: tcd.Index,
					ID:    tcd.ID,
					Name:  tcd.Function.Name,
				}})
				startedTools[tcd.Index] = true
			}
			if tcd.Function.Arguments != "" {
				emit(ChatDelta{ToolArgs: &ToolArgsEvent{
					Index:   tcd.Index,
					Partial: tcd.Function.Arguments,
				}})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return NormalizedStreamResult{FullText: fullText.String(), Err: err}
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
		FullText: fullText.String(),
		Stats:    stats,
	}
}

// AppendAssistantWithToolCalls adds an assistant-with-tool-calls entry in
// OpenAI's wire shape (each call has id, type, and function with string
// arguments). The iterCounter scope ensures that synthesized IDs from one
// live turn don't collide with synthesized IDs from a later turn within the
// same conversation.
func (t *OpenAIChatTransport) AppendAssistantWithToolCalls(messages []map[string]any, text string, toolCalls []NormalizedToolCall) []map[string]any {
	scope := scopeForIter(t.iterCounter.Add(1))
	return append(messages, map[string]any{
		"role":       "assistant",
		"content":    text,
		"tool_calls": buildOpenAIToolCallEntries(scope, toolCalls),
	})
}

// AppendToolResult adds a tool result entry with the required tool_call_id.
// Falls back to a synthesized id only if the caller passed one with no ID —
// in normal flow tc.ID is always populated by the time we get here.
func (t *OpenAIChatTransport) AppendToolResult(messages []map[string]any, tc NormalizedToolCall, result string) []map[string]any {
	id := tc.ID
	if id == "" {
		id = synthesizeToolCallID(scopeForIter(t.iterCounter.Load()), tc.Name, 0)
	}
	return append(messages, map[string]any{
		"role":         "tool",
		"tool_call_id": id,
		"content":      result,
	})
}

// synthesizeToolCallID produces a deterministic fallback id for tool calls
// that didn't carry one (e.g. persisted sessions from before the refactor,
// or transports that don't track ids natively). The scope makes the id
// unique across iterations within a single conversation: without it, two
// identical-looking turns would emit colliding "call_<name>_0" entries that
// strict OpenAI gateways reject.
func synthesizeToolCallID(scope, name string, index int) string {
	if scope == "" {
		return fmt.Sprintf("call_%s_%d", name, index)
	}
	return fmt.Sprintf("call_%s_%s_%d", scope, name, index)
}

// scopeForHistoryMsg builds a scope token from a message's index in the
// persisted history. Stable across runs (deterministic in input), so the
// same history rebuilt twice produces the same wire IDs.
func scopeForHistoryMsg(idx int) string { return fmt.Sprintf("m%d", idx) }

// scopeForIter builds a scope token from the transport's live iteration
// counter. Used by AppendAssistantWithToolCalls so successive tool-using
// turns in one conversation don't collide.
func scopeForIter(n uint64) string { return fmt.Sprintf("t%d", n) }

// buildOpenAIToolCallEntries converts normalized tool calls into the OpenAI
// wire shape ({id, type:"function", function:{name, arguments}}). Used by
// BuildMessages (reading persisted history) and AppendAssistantWithToolCalls
// (running tool loop). The scope parameter feeds synthesizeToolCallID for
// entries with empty IDs; pass "" if you don't need scoping.
func buildOpenAIToolCallEntries(scope string, toolCalls []NormalizedToolCall) []map[string]any {
	out := make([]map[string]any, len(toolCalls))
	for i, n := range toolCalls {
		id := n.ID
		if id == "" {
			id = synthesizeToolCallID(scope, n.Name, i)
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

// UpstreamModel is one model offered by a configured OpenAI-compatible
// endpoint. ContextLength is 0 when the upstream does not advertise one.
type UpstreamModel struct {
	ID            string
	ContextLength int64
}

// FetchOpenAIModels queries /v1/models on the endpoint and returns the raw
// upstream models (IDs carry no endpoint prefix). The error return distinguishes
// "endpoint unreachable / unhealthy" from "endpoint healthy but empty" so the
// ProxyRegistry can record online/offline state accurately.
func FetchOpenAIModels(ctx context.Context, endpoint OpenAIEndpoint) ([]UpstreamModel, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.BaseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if endpoint.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("non-OK status %d", resp.StatusCode)
	}
	var result struct {
		Data []upstreamModelRow `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	models := make([]UpstreamModel, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, UpstreamModel{ID: m.ID, ContextLength: m.contextLength()})
	}
	return models, nil
}

// upstreamModelRow is one entry of an upstream /v1/models response. Only `id`
// is standard OpenAI; the context-length fields are server-specific extensions
// that we read opportunistically so clients get a real context window instead
// of a default.
type upstreamModelRow struct {
	ID string `json:"id"`

	// Context length under the name each server family happens to use.
	MaxModelLen      int64 `json:"max_model_len"`       // vLLM, OMLX
	MaxContextLength int64 `json:"max_context_length"`  // LM Studio
	ContextLength    int64 `json:"context_length"`      // TGI, some gateways
	ContextWindow    int64 `json:"context_window"`      // misc
	Meta             *struct {
		NCtx      int64 `json:"n_ctx"`
		NCtxTrain int64 `json:"n_ctx_train"`
	} `json:"meta"` // llama.cpp
}

// contextLength returns the first context figure the row actually carries,
// or 0 when the server advertises none.
func (m upstreamModelRow) contextLength() int64 {
	candidates := []int64{m.MaxModelLen, m.MaxContextLength, m.ContextLength, m.ContextWindow}
	if m.Meta != nil {
		candidates = append(candidates, m.Meta.NCtx, m.Meta.NCtxTrain)
	}
	for _, c := range candidates {
		if c > 0 {
			return c
		}
	}
	return 0
}
