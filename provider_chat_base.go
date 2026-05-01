package main

import (
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
// tool call emitted by the model. The ID is always populated — synthesized
// by BaseChatProvider's stream state machine when the transport doesn't
// track ids natively (Ollama).
type NormalizedToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// NormalizedStreamResult is what a transport returns after streaming one
// response. Tool calls are NOT in here — BaseChatProvider builds them from
// the streamed deltas (single source of truth). FullText is the concatenated
// text content (excluding thinking and tool args), used for session history
// persistence.
type NormalizedStreamResult struct {
	FullText string
	Stats    SessionStats
	Err      error
}

// ChatDelta is a single streamed piece of output from a transport. Exactly
// one field should be populated per call. ToolStart and ToolArgs let the
// transport stream tool calls incrementally so the canonical event layer
// can emit content_block_start + input_json_delta + content_block_stop.
type ChatDelta struct {
	Text      string
	Thinking  string
	ToolStart *ToolStartEvent
	ToolArgs  *ToolArgsEvent
}

// ToolStartEvent signals that a new tool call has begun streaming. Index
// is the transport's stream-local accumulator key (matching subsequent
// ToolArgs events). ID may be empty for providers that don't track ids
// (Ollama); BaseChatProvider will synthesize one.
type ToolStartEvent struct {
	Index int
	ID    string
	Name  string
}

// ToolArgsEvent carries one fragment of a tool call's JSON-encoded arguments.
// Multiple ToolArgs events for the same Index are concatenated. Ollama emits
// the full arguments in a single event; OpenAI streams fragments.
type ToolArgsEvent struct {
	Index   int
	Partial string
}

// BaseChatSettings holds the common knobs shared between Ollama and OpenAI.
// Transport-specific settings (Ollama's think/num_ctx) embed this.
type BaseChatSettings struct {
	Temperature       *float64                   `json:"temperature,omitempty"`
	TopP              *float64                   `json:"top_p,omitempty"`
	TopK              *int                       `json:"top_k,omitempty"`
	MinP              *float64                   `json:"min_p,omitempty"`
	RepetitionPenalty *float64                   `json:"repetition_penalty,omitempty"`
	PresencePenalty   *float64                   `json:"presence_penalty,omitempty"`
	MaxTokens         *int                       `json:"max_tokens,omitempty"`
	UseRelayTools     *bool                      `json:"useRelayTools,omitempty"`
	MCPServers        map[string]MCPServerConfig `json:"mcpServers,omitempty"`
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
// settings. If useRelayTools is enabled, injects a "relay" MCP server using
// RELAY_MCP_COMMAND and the session's mcpToken (project-scoped) or
// RELAY_MCP_TOKEN (service-level fallback).
//
// Returns nil if no MCP servers are configured (tool calling disabled).
func buildMCPManagerFromSettings(s BaseChatSettings, mcpToken string) *MCPManager {
	servers := s.MCPServers
	if s.UseRelayTools != nil && *s.UseRelayTools {
		if cmd := os.Getenv("RELAY_MCP_COMMAND"); cmd != "" {
			token := mcpToken
			if token == "" {
				token = os.Getenv("RELAY_MCP_TOKEN")
			}
			if servers == nil {
				servers = make(map[string]MCPServerConfig)
			}
			servers["relay"] = MCPServerConfig{
				Command: cmd,
				Args:    []string{"mcp"},
				Env:     map[string]string{"RELAY_TOKEN": token},
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
	session      *Session
	handler      EventHandler
	transport    ChatTransport
	mcpManager   *MCPManager
	builtinTools *BuiltinToolRegistry

	mu         sync.Mutex
	started    atomic.Bool
	cancelFn   context.CancelFunc
	activeBody io.Closer          // resp.Body of the in-flight stream; closed on stop
	generation atomic.Uint64      // incremented on send/stop to discard stale goroutine events
	lastFiles  []FileAttachment   // files from the most recent user message, available to built-in tools
}

// NewBaseChatProvider constructs a provider around a transport. The mcpManager
// is derived from the session's raw settings JSON; pass nil to disable MCP
// entirely regardless of settings. builtinTools may be nil.
func NewBaseChatProvider(session *Session, handler EventHandler, transport ChatTransport, settings json.RawMessage, builtinTools *BuiltinToolRegistry) *BaseChatProvider {
	return &BaseChatProvider{
		session:      session,
		handler:      handler,
		transport:    transport,
		mcpManager:   buildMCPManagerFromSettings(parseBaseSettings(settings), session.McpToken),
		builtinTools: builtinTools,
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

	toolCount := 0
	if p.mcpManager != nil && p.mcpManager.HasTools() {
		toolCount = len(p.mcpManager.tools)
	}
	slog.Info("chat provider started",
		"transport", p.transport.Name(), "session", p.session.ID,
		"model", p.session.Model, "tools", toolCount)
	return nil
}

func (p *BaseChatProvider) SendMessage(text string, files []FileAttachment) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.started.Load() {
		return fmt.Errorf("%s: provider not started", p.transport.Name())
	}

	p.lastFiles = files
	messages := p.transport.BuildMessages(p.session.SystemPrompt, p.copyHistory())

	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFn = cancel
	gen := p.generation.Add(1)

	tools := p.toolDefs()
	slog.Debug("chat: sending message", "transport", p.transport.Name(),
		"session", p.session.ID, "tools", len(tools))

	resp, err := p.transport.PostChat(ctx, messages, tools)
	if err != nil {
		cancel()
		return fmt.Errorf("%s: %w", p.transport.Name(), err)
	}

	go p.runToolLoop(ctx, cancel, resp, messages, time.Now(), gen)
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

// toolDefs returns built-in + MCP tool definitions in the shared chat shape
// ({type:"function", function:{...}}), or nil if no tools are available.
// Both Ollama and OpenAI accept this shape.
func (p *BaseChatProvider) toolDefs() []map[string]any {
	var defs []map[string]any
	if p.builtinTools != nil {
		defs = append(defs, p.builtinTools.ChatToolDefs()...)
	}
	if p.mcpManager != nil && p.mcpManager.HasTools() {
		defs = append(defs, p.mcpManager.ChatToolDefs()...)
	}
	if len(defs) == 0 {
		return nil
	}
	return defs
}

// runToolLoop drives the conversation: stream the first response, and if the
// model emitted tool calls, execute them via MCP and loop with the updated
// message list until the model stops calling tools (or we hit the iteration
// cap). Runs in a goroutine — the session layer only observes streaming
// events and eventually message_complete.
func (p *BaseChatProvider) runToolLoop(ctx context.Context, cancel context.CancelFunc, resp *http.Response, messages []map[string]any, startTime time.Time, gen uint64) {
	defer cancel()

	// Guarded emit: silently discards events if a newer generation has started
	// (i.e. StopGeneration or a new SendMessage was called).
	stale := func() bool { return p.generation.Load() != gen }
	guardedHandler := func(eventType string, data json.RawMessage) {
		if stale() {
			return
		}
		p.handler(eventType, data)
	}
	guardedEmitter := NewEventEmitter(func(eventType string, data json.RawMessage) {
		if stale() {
			return
		}
		p.handler(eventType, data)
	})

	const maxIterations = 10
	const maxToolResultLen = 8192

	var toolMessages []Message

	// All tool-loop iterations are one assistant turn from the client's POV,
	// so emit message_start + system.init once at the top.
	if !stale() {
		guardedEmitter.MessageStart("")
	}
	if !stale() {
		var toolNames []string
		if p.builtinTools != nil {
			for _, def := range p.builtinTools.tools {
				toolNames = append(toolNames, def.Name)
			}
		}
		var mcpNames []string
		if p.mcpManager != nil && p.mcpManager.HasTools() {
			for _, t := range p.mcpManager.tools {
				toolNames = append(toolNames, t.Name)
			}
			for name := range p.mcpManager.servers {
				mcpNames = append(mcpNames, name)
			}
		}
		guardedEmitter.SystemInit(p.session.Model, p.session.Directory, toolNames, mcpNames)
	}

	for iteration := 0; iteration <= maxIterations; iteration++ {
		// Register the response body so StopGeneration can close it immediately.
		p.mu.Lock()
		p.activeBody = resp.Body
		p.mu.Unlock()

		state := newTurnStreamState(guardedEmitter)
		result := p.transport.StreamChunks(resp, startTime, func(d ChatDelta) {
			if stale() {
				return
			}
			switch {
			case d.ToolStart != nil:
				state.onToolStart(d.ToolStart)
			case d.ToolArgs != nil:
				state.onToolArgs(d.ToolArgs)
			case d.Thinking != "":
				state.onThinking(d.Thinking)
			case d.Text != "":
				state.onText(d.Text)
			}
		})
		toolCalls := state.finalize()

		p.mu.Lock()
		p.activeBody = nil
		p.mu.Unlock()

		// If the context was cancelled (stop requested), exit silently.
		// The session layer emits its own message_complete on stop.
		if ctx.Err() != nil {
			slog.Debug("chat: stream cancelled, exiting tool loop",
				"transport", p.transport.Name(), "session", p.session.ID)
			return
		}

		// Prefer state-machine accumulated text; fall back to the transport's
		// FullText for transports that report it without fanning through emit.
		streamText := state.fullText.String()
		if streamText == "" {
			streamText = result.FullText
		}

		if result.Err != nil {
			slog.Error("chat: stream error", "transport", p.transport.Name(), "session", p.session.ID, "error", result.Err)
			guardedHandler("error", mustJSON(map[string]string{"error": result.Err.Error()}))
			return
		}

		// Terminal condition: no more tool calls, no tool handlers, or cap hit.
		if len(toolCalls) == 0 || (p.mcpManager == nil && p.builtinTools == nil) || iteration == maxIterations {
			statsData, _ := json.Marshal(result.Stats)
			guardedHandler("stats_update", statsData)

			if len(state.blocks) > 0 {
				toolMessages = append(toolMessages, Message{
					Timestamp: timeNow(),
					Role:      "assistant",
					Content:   mustJSON(state.blocks),
				})
			}
			if len(toolMessages) > 0 {
				p.session.mu.Lock()
				p.session.Messages = append(p.session.Messages, toolMessages...)
				p.session.mu.Unlock()
			}

			// Nil payload tells session.go's handler to skip its fallback
			// text-only save — we already persisted the canonical blocks above.
			guardedHandler("message_complete", nil)
			return
		}

		// Persist the assistant-with-tool-calls message as canonical content
		// blocks. state.blocks is guaranteed non-empty here — toolCalls came
		// from onToolStart, which appends a tool_use block via closeOpen.
		tcJSON, _ := json.Marshal(toolCalls)
		toolMessages = append(toolMessages, Message{
			Timestamp: timeNow(),
			Role:      "assistant",
			Content:   mustJSON(state.blocks),
			ToolCalls: tcJSON,
		})
		messages = p.transport.AppendAssistantWithToolCalls(messages, streamText, toolCalls)

		// Execute each tool and append its result.
		for _, tc := range toolCalls {
			if ctx.Err() != nil {
				return
			}

			var toolResult string
			var toolErr error
			if p.builtinTools != nil && p.builtinTools.Has(tc.Name) {
				toolResult, toolErr = p.builtinTools.Call(ctx, tc.Name, tc.Arguments, p.lastFiles, guardedHandler)
			} else if p.mcpManager != nil {
				toolResult, toolErr = p.mcpManager.CallTool(ctx, tc.Name, tc.Arguments)
			} else {
				toolErr = fmt.Errorf("no handler for tool %q", tc.Name)
			}
			isError := toolErr != nil
			if toolErr != nil {
				if ctx.Err() != nil {
					return
				}
				toolResult = fmt.Sprintf("Error: %s", toolErr.Error())
				slog.Warn("chat: tool call failed", "transport", p.transport.Name(), "tool", tc.Name, "error", toolErr)
			}
			if len(toolResult) > maxToolResultLen {
				toolResult = toolResult[:maxToolResultLen] + "\n...(truncated)"
			}

			guardedEmitter.ToolResult(tc.ID, tc.Name, toolResult, isError)

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
			if ctx.Err() != nil {
				return
			}
			guardedHandler("error", mustJSON(map[string]string{"error": err.Error()}))
			return
		}
	}
}

func (p *BaseChatProvider) StopGeneration() {
	p.mu.Lock()
	cancel := p.cancelFn
	p.cancelFn = nil
	body := p.activeBody
	p.activeBody = nil
	p.mu.Unlock()

	// Increment generation first — any events the old goroutine emits after
	// this point are silently discarded by the guarded handler.
	p.generation.Add(1)

	// Close the response body to immediately break the scanner mid-read.
	// This is faster than waiting for context cancellation to propagate.
	if body != nil {
		body.Close()
	}
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


func timeNow() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
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

// turnStreamState drives canonical event emission for a single streamed
// response from the transport. It tracks the open content block (so
// transitions emit content_block_stop + content_block_start), assigns
// canonical block indices, and accumulates tool-call state so that at
// stream end the caller can read out the resolved tool calls.
//
// The state machine is intentionally permissive about ordering — text and
// thinking deltas can interleave in any order, and tool calls can start at
// any point. The only invariant is that each block_start has a matching
// block_stop before the next start at the same kind.
type turnStreamState struct {
	emitter *EventEmitter

	// Per-turn block index counter. Resets each turn (each StreamChunks call).
	nextBlockIdx int

	// What kind of block is currently open, and at which index.
	openKind  blockKind
	openIndex int

	// Transport-local index of the currently open tool block, if any. Lets
	// closeOpen look the entry up in O(1) rather than scanning s.tools.
	openToolIdx int

	// Tool tracking: transport's stream-local index → tool block state.
	tools map[int]*toolBlockState

	// Order tools were started, so finalizeTools returns them in order.
	toolOrder []int

	// Per-block accumulator for the open text or thinking block. Reset on
	// every text/thinking block start; consumed when the block closes.
	openBlockText strings.Builder

	// Accumulated user-visible text (excludes thinking and tool args).
	fullText strings.Builder

	// Resolved canonical content blocks for this turn, in stream order.
	// Persisted as the assistant Message.Content so thinking and tool_use
	// blocks survive a page refresh.
	blocks []json.RawMessage
}

type blockKind int

const (
	blockNone blockKind = iota
	blockOpenText
	blockOpenThinking
	blockOpenToolUse
)

type toolBlockState struct {
	blockIdx int
	id       string
	name     string
	args     strings.Builder
}

func newTurnStreamState(emitter *EventEmitter) *turnStreamState {
	return &turnStreamState{
		emitter: emitter,
		tools:   make(map[int]*toolBlockState),
	}
}

// onText handles a streaming text delta. Closes any open thinking/tool block,
// opens a text block if needed, then emits the text_delta.
func (s *turnStreamState) onText(text string) {
	if s.openKind != blockOpenText {
		s.closeOpen()
		s.openKind = blockOpenText
		s.openIndex = s.nextBlockIdx
		s.nextBlockIdx++
		s.openBlockText.Reset()
		s.emitter.TextBlockStart(s.openIndex)
	}
	s.openBlockText.WriteString(text)
	s.fullText.WriteString(text)
	s.emitter.TextDelta(s.openIndex, text)
}

// onThinking handles a streaming thinking delta.
func (s *turnStreamState) onThinking(text string) {
	if s.openKind != blockOpenThinking {
		s.closeOpen()
		s.openKind = blockOpenThinking
		s.openIndex = s.nextBlockIdx
		s.nextBlockIdx++
		s.openBlockText.Reset()
		s.emitter.ThinkingBlockStart(s.openIndex)
	}
	s.openBlockText.WriteString(text)
	s.emitter.ThinkingDelta(s.openIndex, text)
}

// onToolStart handles the first delta for a new tool call. Synthesizes an
// id if the transport didn't provide one.
func (s *turnStreamState) onToolStart(ev *ToolStartEvent) {
	if _, exists := s.tools[ev.Index]; exists {
		return // duplicate start, ignore
	}
	s.closeOpen()
	id := ev.ID
	if id == "" {
		id = SynthesizeToolUseID(ev.Index, ev.Name)
	}
	tb := &toolBlockState{
		blockIdx: s.nextBlockIdx,
		id:       id,
		name:     ev.Name,
	}
	s.tools[ev.Index] = tb
	s.toolOrder = append(s.toolOrder, ev.Index)
	s.openKind = blockOpenToolUse
	s.openIndex = s.nextBlockIdx
	s.openToolIdx = ev.Index
	s.nextBlockIdx++
	s.emitter.ToolUseBlockStart(tb.blockIdx, tb.id, tb.name)
}

// onToolArgs handles a streaming arguments fragment for a known tool call.
func (s *turnStreamState) onToolArgs(ev *ToolArgsEvent) {
	tb, ok := s.tools[ev.Index]
	if !ok {
		// Defensive: transport sent args without a start. Skip.
		return
	}
	tb.args.WriteString(ev.Partial)
	s.emitter.InputJsonDelta(tb.blockIdx, ev.Partial)
}

// closeOpen emits content_block_stop for whatever block is currently open and
// appends the resolved block to s.blocks. For tool blocks it echoes the
// resolved input; for text/thinking it's a bare stop event. Empty
// text/thinking blocks aren't persisted (no user-visible content).
func (s *turnStreamState) closeOpen() {
	switch s.openKind {
	case blockOpenText, blockOpenThinking:
		blockType, contentKey := BlockText, "text"
		if s.openKind == blockOpenThinking {
			blockType, contentKey = BlockThinking, "thinking"
		}
		if text := s.openBlockText.String(); text != "" {
			block, _ := json.Marshal(map[string]any{"type": blockType, contentKey: text})
			s.blocks = append(s.blocks, block)
		}
		s.emitter.BlockStop(s.openIndex)
	case blockOpenToolUse:
		if tb := s.tools[s.openToolIdx]; tb != nil {
			args := strings.TrimSpace(tb.args.String())
			if args == "" {
				args = "{}"
			}
			block, _ := json.Marshal(map[string]any{
				"type":  BlockToolUse,
				"id":    tb.id,
				"name":  tb.name,
				"input": json.RawMessage(args),
			})
			s.blocks = append(s.blocks, block)
			s.emitter.ToolUseBlockStop(tb.blockIdx, tb.id, tb.name, json.RawMessage(args))
		}
	}
	s.openKind = blockNone
}

// finalize closes any still-open block at end of stream. Returns the resolved
// tool calls in the order they were started.
func (s *turnStreamState) finalize() []NormalizedToolCall {
	s.closeOpen()
	out := make([]NormalizedToolCall, 0, len(s.toolOrder))
	for _, idx := range s.toolOrder {
		tb := s.tools[idx]
		args := strings.TrimSpace(tb.args.String())
		if args == "" {
			args = "{}"
		}
		out = append(out, NormalizedToolCall{
			ID:        tb.id,
			Name:      tb.name,
			Arguments: json.RawMessage(args),
		})
	}
	return out
}
