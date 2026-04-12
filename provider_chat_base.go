package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ChatTransport abstracts the wire format for a streaming chat API. Lifecycle,
// the tool-call loop, and event emission live in BaseChatProvider; a transport
// only handles format-specific concerns (request shape, response parsing,
// auth, and how to append assistant/tool messages to the running conversation).
type ChatTransport interface {
	// Name returns a short identifier for logging (e.g. "ollama", "openai:lmstudio").
	Name() string

	// Ping verifies the endpoint is reachable. Called during Start.
	Ping(ctx context.Context) error

	// BuildMessages converts session history into the transport's wire format,
	// prepending the system prompt if non-empty.
	BuildMessages(systemPrompt string, msgs []Message) []map[string]any

	// PostChat sends a streaming chat request and returns the HTTP response.
	// The caller owns closing the response body (via StreamChunks).
	PostChat(ctx context.Context, messages []map[string]any, tools []map[string]any) (*http.Response, error)

	// StreamChunks reads and parses the response body, returning the accumulated
	// text, any tool calls, and final stats. It invokes emit for every streamed
	// text or thinking delta. It MUST close resp.Body before returning.
	StreamChunks(resp *http.Response, startTime time.Time, emit func(delta ChatDelta)) NormalizedStreamResult

	// AppendAssistantWithToolCalls adds an assistant message (with tool_calls)
	// to the running messages array for follow-up requests.
	AppendAssistantWithToolCalls(messages []map[string]any, text string, toolCalls []NormalizedToolCall) []map[string]any

	// AppendToolResult adds a tool result message to the running messages array.
	AppendToolResult(messages []map[string]any, tc NormalizedToolCall, result string) []map[string]any
}

// NormalizedToolCall is the transport-agnostic representation of a single
// tool call emitted by the model. The ID is always populated by OpenAI-shaped
// APIs and left empty by Ollama-native (which does not track tool call IDs).
type NormalizedToolCall struct {
	ID        string // empty for providers that don't track IDs
	Name      string
	Arguments json.RawMessage
}

// NormalizedStreamResult is what a transport returns after streaming one
// response. Both the Ollama and OpenAI transports map their wire formats
// into this shape so BaseChatProvider's tool loop can stay format-agnostic.
type NormalizedStreamResult struct {
	FullText  string
	ToolCalls []NormalizedToolCall
	Stats     SessionStats
	Err       error
}

// ChatDelta is a single streamed piece of output from a transport. Exactly
// one of Text or Thinking should be non-empty per call.
type ChatDelta struct {
	Text     string
	Thinking string
}

// BaseChatSettings holds the common knobs shared between Ollama and OpenAI.
// Transport-specific settings (Ollama's think/num_ctx) embed this.
type BaseChatSettings struct {
	Temperature   *float64                   `json:"temperature,omitempty"`
	TopP          *float64                   `json:"top_p,omitempty"`
	TopK          *int                       `json:"top_k,omitempty"`
	MinP          *float64                   `json:"min_p,omitempty"`
	UseRelayTools *bool                      `json:"useRelayTools,omitempty"`
	MCPServers    map[string]MCPServerConfig `json:"mcpServers,omitempty"`
}

// parseBaseSettings extracts BaseChatSettings from a raw JSON blob. See
// fixupMCPServersString for the stringly-encoded mcpServers fallback.
func parseBaseSettings(raw json.RawMessage) BaseChatSettings {
	var s BaseChatSettings
	if len(raw) == 0 {
		return s
	}
	_ = json.Unmarshal(raw, &s)
	fixupMCPServersString(raw, &s.MCPServers)
	return s
}

// fixupMCPServersString handles the case where mcpServers arrives as a
// JSON-encoded string (Eve's text-input field sends it that way) instead of
// a parsed object. No-op if the target is already populated.
func fixupMCPServersString(raw json.RawMessage, target *map[string]MCPServerConfig) {
	if len(*target) > 0 {
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return
	}
	mcpRaw, ok := fields["mcpServers"]
	if !ok {
		return
	}
	var asString string
	if json.Unmarshal(mcpRaw, &asString) == nil && asString != "" {
		_ = json.Unmarshal([]byte(asString), target)
	}
}

// buildMCPManagerFromSettings constructs an MCPManager for the given base
// settings, honoring the useRelayTools flag by auto-injecting a "relay"
// server config from RELAY_MCP_COMMAND / RELAY_MCP_TOKEN env vars.
//
// Returns nil if no MCP servers are configured (tool calling disabled).
func buildMCPManagerFromSettings(s BaseChatSettings) *MCPManager {
	servers := s.MCPServers
	if s.UseRelayTools != nil && *s.UseRelayTools {
		if cmd := os.Getenv("RELAY_MCP_COMMAND"); cmd != "" {
			if servers == nil {
				servers = make(map[string]MCPServerConfig)
			}
			servers["relay"] = MCPServerConfig{
				Command: cmd,
				Args:    []string{"mcp"},
				Env:     map[string]string{"RELAY_TOKEN": os.Getenv("RELAY_MCP_TOKEN")},
			}
		} else {
			slog.Warn("useRelayTools enabled but RELAY_MCP_COMMAND not set")
		}
	}
	if len(servers) == 0 {
		return nil
	}
	return NewMCPManager(servers)
}

// BaseChatProvider implements the Provider interface by delegating all
// format-specific work to a ChatTransport. It owns the provider lifecycle,
// the tool-calling loop, event emission, and MCP orchestration.
type BaseChatProvider struct {
	session    *Session
	handler    EventHandler
	transport  ChatTransport
	mcpManager *MCPManager

	mu       sync.Mutex
	started  atomic.Bool
	cancelFn context.CancelFunc
}

// NewBaseChatProvider constructs a provider around a transport. The mcpManager
// is derived from the session's raw settings JSON; pass nil to disable MCP
// entirely regardless of settings.
func NewBaseChatProvider(session *Session, handler EventHandler, transport ChatTransport, settings json.RawMessage) *BaseChatProvider {
	return &BaseChatProvider{
		session:    session,
		handler:    handler,
		transport:  transport,
		mcpManager: buildMCPManagerFromSettings(parseBaseSettings(settings)),
	}
}

func (p *BaseChatProvider) Start() error {
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := p.transport.Ping(pingCtx); err != nil {
		return fmt.Errorf("%s: ping: %w", p.transport.Name(), err)
	}

	p.started.Store(true)

	if p.mcpManager != nil {
		mcpCtx, mcpCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer mcpCancel()
		if err := p.mcpManager.Start(mcpCtx); err != nil {
			slog.Warn("chat: MCP servers failed to start (tool calling disabled)",
				"transport", p.transport.Name(), "session", p.session.ID, "error", err)
			p.mcpManager = nil
		}
	}

	slog.Info("chat provider started",
		"transport", p.transport.Name(), "session", p.session.ID, "model", p.session.Model)
	return nil
}

func (p *BaseChatProvider) SendMessage(text string, files []FileAttachment) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.started.Load() {
		return fmt.Errorf("%s: provider not started", p.transport.Name())
	}

	messages := p.transport.BuildMessages(p.session.SystemPrompt, p.copyHistory())

	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFn = cancel

	resp, err := p.transport.PostChat(ctx, messages, p.toolDefs())
	if err != nil {
		cancel()
		return fmt.Errorf("%s: %w", p.transport.Name(), err)
	}

	go p.runToolLoop(ctx, cancel, resp, messages, time.Now())
	return nil
}

// copyHistory snapshots the session's message history under the session lock.
// Transports must not reach into session.Messages directly — they receive a
// copy via BuildMessages so the tool loop can mutate its working set safely.
func (p *BaseChatProvider) copyHistory() []Message {
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	msgs := make([]Message, len(p.session.Messages))
	copy(msgs, p.session.Messages)
	return msgs
}

// toolDefs returns the MCP tool definitions in the shared chat shape
// ({type:"function", function:{...}}), or nil if MCP is not configured.
// Both Ollama and OpenAI accept this shape.
func (p *BaseChatProvider) toolDefs() []map[string]any {
	if p.mcpManager == nil || !p.mcpManager.HasTools() {
		return nil
	}
	return p.mcpManager.ChatToolDefs()
}

// runToolLoop drives the conversation: stream the first response, and if the
// model emitted tool calls, execute them via MCP and loop with the updated
// message list until the model stops calling tools (or we hit the iteration
// cap). Runs in a goroutine — the session layer only observes streaming
// events and eventually message_complete.
func (p *BaseChatProvider) runToolLoop(ctx context.Context, cancel context.CancelFunc, resp *http.Response, messages []map[string]any, startTime time.Time) {
	defer cancel()

	const maxIterations = 10
	const maxToolResultLen = 8192

	var allText strings.Builder
	var toolMessages []Message // session-history tool messages accumulated across iterations

	for iteration := 0; iteration <= maxIterations; iteration++ {
		// Emit the assistant-start event exactly once, on the first delta
		// that arrives from the transport. This keeps the transport oblivious
		// to the start event while still firing it in the right order.
		var startOnce sync.Once
		tracker := &thinkBlockTracker{emit: p.emitTextDelta}
		result := p.transport.StreamChunks(resp, startTime, func(d ChatDelta) {
			startOnce.Do(p.emitAssistantStart)
			switch {
			case d.Thinking != "":
				tracker.thinking(d.Thinking)
			case d.Text != "":
				tracker.text(d.Text)
			}
		})
		tracker.close()
		allText.WriteString(result.FullText)

		if result.Err != nil {
			slog.Error("chat: stream error", "transport", p.transport.Name(), "session", p.session.ID, "error", result.Err)
			p.emitError(result.Err.Error())
			return
		}

		// Terminal condition: no more tool calls, MCP unavailable, or cap hit.
		if len(result.ToolCalls) == 0 || p.mcpManager == nil || iteration == maxIterations {
			statsData, _ := json.Marshal(result.Stats)
			p.handler("stats_update", statsData)

			if len(toolMessages) > 0 {
				p.session.mu.Lock()
				p.session.Messages = append(p.session.Messages, toolMessages...)
				p.session.mu.Unlock()
			}

			completeData, _ := json.Marshal(map[string]string{"text": allText.String()})
			p.handler("message_complete", completeData)
			return
		}

		// Persist the assistant-with-tool-calls message in session history.
		tcJSON, _ := json.Marshal(result.ToolCalls)
		assistantContent, _ := json.Marshal(result.FullText)
		toolMessages = append(toolMessages, Message{
			Timestamp: timeNow(),
			Role:      "assistant",
			Content:   assistantContent,
			ToolCalls: tcJSON,
		})
		messages = p.transport.AppendAssistantWithToolCalls(messages, result.FullText, result.ToolCalls)

		// Execute each tool and append its result.
		for _, tc := range result.ToolCalls {
			p.emitTextDelta(fmt.Sprintf("\n**[Calling: %s]**\n", tc.Name))

			toolResult, err := p.mcpManager.CallTool(ctx, tc.Name, tc.Arguments)
			if err != nil {
				toolResult = fmt.Sprintf("Error: %s", err.Error())
				slog.Warn("chat: tool call failed", "transport", p.transport.Name(), "tool", tc.Name, "error", err)
			}
			if len(toolResult) > maxToolResultLen {
				toolResult = toolResult[:maxToolResultLen] + "\n...(truncated)"
			}

			preview := toolResult
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			p.emitTextDelta(fmt.Sprintf("**[Result: %s]**\n\n", preview))

			resultContent, _ := json.Marshal(toolResult)
			toolMessages = append(toolMessages, Message{
				Timestamp: timeNow(),
				Role:      "tool",
				Content:   resultContent,
				ToolName:  tc.Name,
			})
			messages = p.transport.AppendToolResult(messages, tc, toolResult)
		}

		// Follow up with the updated message list.
		var err error
		resp, err = p.transport.PostChat(ctx, messages, p.toolDefs())
		if err != nil {
			p.emitError(err.Error())
			return
		}
	}
}

func (p *BaseChatProvider) StopGeneration() {
	p.mu.Lock()
	cancel := p.cancelFn
	p.cancelFn = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	slog.Info("chat generation stopped", "transport", p.transport.Name(), "session", p.session.ID)
}

func (p *BaseChatProvider) Kill() {
	p.StopGeneration()
	if p.mcpManager != nil {
		p.mcpManager.Close()
	}
	p.started.Store(false)
	slog.Info("chat provider killed", "transport", p.transport.Name(), "session", p.session.ID)
}

func (p *BaseChatProvider) DeleteSession() error           { return nil }
func (p *BaseChatProvider) Alive() bool                    { return p.started.Load() }
func (p *BaseChatProvider) GetState() json.RawMessage      { return json.RawMessage(`{}`) }
func (p *BaseChatProvider) RestoreState(_ json.RawMessage) {}

// emitTextDelta streams a text_delta event to the session layer.
func (p *BaseChatProvider) emitTextDelta(text string) {
	event := map[string]any{
		"type": "assistant",
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	}
	data, _ := json.Marshal(event)
	p.handler("llm_event", data)
}

// emitError sends a safely JSON-encoded error event.
func (p *BaseChatProvider) emitError(msg string) {
	data, _ := json.Marshal(map[string]string{"error": msg})
	p.handler("error", data)
}

// emitAssistantStart sends the initial "assistant" event that signals the
// start of a streamed message. Transports call this from their StreamChunks
// implementation via the emit callback's first text/thinking delta.
func (p *BaseChatProvider) emitAssistantStart() {
	event := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{},
		},
	}
	data, _ := json.Marshal(event)
	p.handler("llm_event", data)
}

func timeNow() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// decodeNormalizedToolCalls unmarshals a persisted tool_calls JSON blob into
// []NormalizedToolCall, falling back to the pre-refactor Ollama wire shape
// ([{function:{name, arguments}}]). Returns nil if neither form applies.
func decodeNormalizedToolCalls(raw json.RawMessage) []NormalizedToolCall {
	if len(raw) == 0 {
		return nil
	}
	// Try the new normalized shape first.
	var norm []NormalizedToolCall
	if err := json.Unmarshal(raw, &norm); err == nil {
		// If the decoder got entries but none carry a Name, the blob is
		// probably in the legacy Ollama shape — fall through to that.
		allEmpty := len(norm) > 0
		for _, n := range norm {
			if n.Name != "" {
				allEmpty = false
				break
			}
		}
		if !allEmpty {
			return norm
		}
	}
	// Legacy Ollama shape: [{function:{name, arguments}}]
	var legacy []struct {
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &legacy); err == nil {
		out := make([]NormalizedToolCall, 0, len(legacy))
		for _, l := range legacy {
			if l.Function.Name == "" {
				continue
			}
			out = append(out, NormalizedToolCall{
				Name:      l.Function.Name,
				Arguments: l.Function.Arguments,
			})
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// thinkBlockTracker wraps thinking deltas in <think>...</think> tags as text
// deltas arrive interleaved. Both Ollama (chunk.message.thinking) and OpenAI
// reasoning models (delta.reasoning_content) stream thinking this way.
type thinkBlockTracker struct {
	emit   func(string)
	active bool
}

func (t *thinkBlockTracker) thinking(text string) {
	if !t.active {
		t.active = true
		t.emit("<think>\n")
	}
	t.emit(text)
}

func (t *thinkBlockTracker) text(text string) {
	if t.active {
		t.active = false
		t.emit("\n</think>\n\n")
	}
	t.emit(text)
}

func (t *thinkBlockTracker) close() {
	if t.active {
		t.active = false
		t.emit("\n</think>\n\n")
	}
}
