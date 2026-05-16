package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

const (
	ProtocolVersion    = "2"
	ProtocolVersionNum = 2
)

const (
	BlockText     = "text"
	BlockThinking = "thinking"
	BlockToolUse  = "tool_use"
)

const (
	DeltaText      = "text_delta"
	DeltaThinking  = "thinking_delta"
	DeltaInputJSON = "input_json_delta"
)

const (
	EvtSystem    = "system"
	EvtAssistant = "assistant"
	EvtResult    = "result"
)

const (
	ResultToolResultSubtype   = "tool_result"
	ResultToolProgressSubtype = "tool_progress"
	ResultErrorSubtype        = "error"
)

const (
	SystemInitSubtype              = "init"
	SystemPermissionRequestSubtype = "permission_request"
	SystemQuestionSubtype          = "question"
	SystemStatusSubtype            = "status"
	SystemAPIErrorSubtype          = "api_error"
	SystemBridgeStatusSubtype      = "bridge_status"
	SystemStopHookSummarySubtype   = "stop_hook_summary"
)

// versionField is the pre-marshaled `v` value spliced into raw events by
// EmitVersionedRaw. Kept as RawMessage so the unmarshal/remarshal round-trip
// doesn't have to format the int per call.
var versionField = json.RawMessage(fmt.Sprintf("%d", ProtocolVersionNum))

// Embedded envelopes carry V plus the type discriminator(s) so each event
// struct doesn't repeat them. Constructors stamp V centrally — no caller can
// forget it.

type subtypedEnvelope struct {
	V       int    `json:"v"`
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
}

func newSystemEnv(subtype string) subtypedEnvelope {
	return subtypedEnvelope{V: ProtocolVersionNum, Type: EvtSystem, Subtype: subtype}
}

func newResultEnv(subtype string) subtypedEnvelope {
	return subtypedEnvelope{V: ProtocolVersionNum, Type: EvtResult, Subtype: subtype}
}

type assistantEnvelope struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

func newAssistantEnv() assistantEnvelope {
	return assistantEnvelope{V: ProtocolVersionNum, Type: EvtAssistant}
}

// ---- Typed canonical event structs ----

type SystemInitEvent struct {
	subtypedEnvelope
	Model      string   `json:"model"`
	Cwd        string   `json:"cwd"`
	Tools      []string `json:"tools"`
	MCPServers []string `json:"mcp_servers"`
}

// SystemPermissionRequestEvent: the provider holds generation until a response
// arrives via /api/permission. Claude-only today.
type SystemPermissionRequestEvent struct {
	subtypedEnvelope
	PermissionID string          `json:"permission_id"`
	ToolName     string          `json:"tool_name"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	ToolInput    json.RawMessage `json:"tool_input,omitempty"`
}

type SystemQuestionEvent struct {
	subtypedEnvelope
	Prompt   string          `json:"prompt"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

type SystemStatusEvent struct {
	subtypedEnvelope
	Message string `json:"message"`
}

type SystemAPIErrorEvent struct {
	subtypedEnvelope
	Message  string `json:"message"`
	Retrying bool   `json:"retrying,omitempty"`
}

type SystemBridgeStatusEvent struct {
	subtypedEnvelope
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type SystemStopHookSummaryEvent struct {
	subtypedEnvelope
	Summary string `json:"summary"`
	IsError bool   `json:"is_error,omitempty"`
}

type AssistantMessage struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type AssistantMessageStartEvent struct {
	assistantEnvelope
	Message AssistantMessage `json:"message"`
}

type ContentBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type AssistantBlockStartEvent struct {
	assistantEnvelope
	Index        int          `json:"index"`
	ContentBlock ContentBlock `json:"content_block"`
}

type Delta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type AssistantBlockDeltaEvent struct {
	assistantEnvelope
	Index int   `json:"index"`
	Delta Delta `json:"delta"`
}

// AssistantBlockStopEvent's tail ContentBlock is non-nil only for tool_use
// blocks (resolved input echo).
type AssistantBlockStopEvent struct {
	assistantEnvelope
	Index            int           `json:"index"`
	ContentBlockStop bool          `json:"content_block_stop"`
	ContentBlock     *ContentBlock `json:"content_block,omitempty"`
}

// ResultToolResultEvent: clients pair by ToolUseID. ToolName is metadata for
// UI labelling only.
type ResultToolResultEvent struct {
	subtypedEnvelope
	ToolUseID string `json:"tool_use_id"`
	ToolName  string `json:"tool_name,omitempty"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

type ResultToolProgressEvent struct {
	subtypedEnvelope
	ToolUseID string `json:"tool_use_id"`
	ToolName  string `json:"tool_name,omitempty"`
	Message   string `json:"message"`
}

// ResultErrorEvent is a streamed model/tool error, distinct from
// system.api_error (recoverable upstream context) and the top-level error
// envelope (session-level).
type ResultErrorEvent struct {
	subtypedEnvelope
	Error string `json:"error"`
}

// ---- EventEmitter ----

// EventEmitter is a typed wrapper around an EventHandler. Providers construct
// one and call its methods rather than building JSON inline — the methods are
// the single point that enforces the wire contract.
type EventEmitter struct {
	handler EventHandler
}

func NewEventEmitter(handler EventHandler) *EventEmitter {
	return &EventEmitter{handler: handler}
}

func (e *EventEmitter) SystemInit(model, cwd string, tools, mcpServers []string) {
	if tools == nil {
		tools = []string{}
	}
	if mcpServers == nil {
		mcpServers = []string{}
	}
	e.emit(SystemInitEvent{
		subtypedEnvelope: newSystemEnv(SystemInitSubtype),
		Model:            model,
		Cwd:              cwd,
		Tools:            tools,
		MCPServers:       mcpServers,
	})
}

func (e *EventEmitter) PermissionRequest(permissionID, toolName, toolUseID string, toolInput json.RawMessage) {
	e.emit(SystemPermissionRequestEvent{
		subtypedEnvelope: newSystemEnv(SystemPermissionRequestSubtype),
		PermissionID:     permissionID,
		ToolName:         toolName,
		ToolUseID:        toolUseID,
		ToolInput:        toolInput,
	})
}

func (e *EventEmitter) SystemQuestion(prompt string, metadata json.RawMessage) {
	e.emit(SystemQuestionEvent{
		subtypedEnvelope: newSystemEnv(SystemQuestionSubtype),
		Prompt:           prompt,
		Metadata:         metadata,
	})
}

func (e *EventEmitter) SystemStatus(message string) {
	e.emit(SystemStatusEvent{
		subtypedEnvelope: newSystemEnv(SystemStatusSubtype),
		Message:          message,
	})
}

func (e *EventEmitter) SystemAPIError(message string, retrying bool) {
	e.emit(SystemAPIErrorEvent{
		subtypedEnvelope: newSystemEnv(SystemAPIErrorSubtype),
		Message:          message,
		Retrying:         retrying,
	})
}

func (e *EventEmitter) SystemBridgeStatus(status, detail string) {
	e.emit(SystemBridgeStatusEvent{
		subtypedEnvelope: newSystemEnv(SystemBridgeStatusSubtype),
		Status:           status,
		Detail:           detail,
	})
}

func (e *EventEmitter) SystemStopHookSummary(summary string, isError bool) {
	e.emit(SystemStopHookSummaryEvent{
		subtypedEnvelope: newSystemEnv(SystemStopHookSummarySubtype),
		Summary:          summary,
		IsError:          isError,
	})
}

func (e *EventEmitter) MessageStart(id string) {
	e.emit(AssistantMessageStartEvent{
		assistantEnvelope: newAssistantEnv(),
		Message: AssistantMessage{
			ID:      id,
			Role:    "assistant",
			Content: []any{},
		},
	})
}

func (e *EventEmitter) TextBlockStart(index int) {
	e.emit(AssistantBlockStartEvent{
		assistantEnvelope: newAssistantEnv(),
		Index:             index,
		ContentBlock:      ContentBlock{Type: BlockText},
	})
}

func (e *EventEmitter) ThinkingBlockStart(index int) {
	e.emit(AssistantBlockStartEvent{
		assistantEnvelope: newAssistantEnv(),
		Index:             index,
		ContentBlock:      ContentBlock{Type: BlockThinking},
	})
}

// ToolUseBlockStart: id must be unique within the session. Providers without
// native ids should call SynthesizeToolUseID.
func (e *EventEmitter) ToolUseBlockStart(index int, id, name string) {
	e.emit(AssistantBlockStartEvent{
		assistantEnvelope: newAssistantEnv(),
		Index:             index,
		ContentBlock: ContentBlock{
			Type:  BlockToolUse,
			ID:    id,
			Name:  name,
			Input: json.RawMessage(`{}`),
		},
	})
}

func (e *EventEmitter) TextDelta(index int, text string) {
	e.emit(AssistantBlockDeltaEvent{
		assistantEnvelope: newAssistantEnv(),
		Index:             index,
		Delta:             Delta{Type: DeltaText, Text: text},
	})
}

func (e *EventEmitter) ThinkingDelta(index int, text string) {
	e.emit(AssistantBlockDeltaEvent{
		assistantEnvelope: newAssistantEnv(),
		Index:             index,
		Delta:             Delta{Type: DeltaThinking, Thinking: text},
	})
}

func (e *EventEmitter) InputJsonDelta(index int, partial string) {
	e.emit(AssistantBlockDeltaEvent{
		assistantEnvelope: newAssistantEnv(),
		Index:             index,
		Delta:             Delta{Type: DeltaInputJSON, PartialJSON: partial},
	})
}

func (e *EventEmitter) BlockStop(index int) {
	e.emit(AssistantBlockStopEvent{
		assistantEnvelope: newAssistantEnv(),
		Index:             index,
		ContentBlockStop:  true,
	})
}

// ToolUseBlockStop echoes the resolved final input so clients that ignored
// the streaming deltas still get the full call.
func (e *EventEmitter) ToolUseBlockStop(index int, id, name string, input json.RawMessage) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	cb := ContentBlock{Type: BlockToolUse, ID: id, Name: name, Input: input}
	e.emit(AssistantBlockStopEvent{
		assistantEnvelope: newAssistantEnv(),
		Index:             index,
		ContentBlockStop:  true,
		ContentBlock:      &cb,
	})
}

func (e *EventEmitter) ToolResult(toolUseID, toolName, content string, isError bool) {
	e.emit(ResultToolResultEvent{
		subtypedEnvelope: newResultEnv(ResultToolResultSubtype),
		ToolUseID:        toolUseID,
		ToolName:         toolName,
		Content:          content,
		IsError:          isError,
	})
}

func (e *EventEmitter) ToolProgress(toolUseID, toolName, message string) {
	e.emit(ResultToolProgressEvent{
		subtypedEnvelope: newResultEnv(ResultToolProgressSubtype),
		ToolUseID:        toolUseID,
		ToolName:         toolName,
		Message:          message,
	})
}

func (e *EventEmitter) ResultError(msg string) {
	e.emit(ResultErrorEvent{
		subtypedEnvelope: newResultEnv(ResultErrorSubtype),
		Error:            msg,
	})
}

// emit marshals a typed event and forwards it. Marshal failures here mean a
// programming bug (an event struct with a non-serializable field), so panic —
// production paths never trip this.
func (e *EventEmitter) emit(event any) {
	if e == nil || e.handler == nil {
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		panic(fmt.Sprintf("event emitter: marshal failed: %v", err))
	}
	e.handler("llm_event", data)
}

// EmitVersionedRaw forwards a pre-shaped event after stamping the protocol
// version on it. The escape hatch for translator-side events whose full
// schema isn't yet typed (Claude-CLI-specific top-level events like
// permission-mode, ai-title, custom-title). Prefer the typed methods for
// any event that's part of the documented contract.
//
// Malformed input (non-object JSON, marshal failure) is logged and dropped
// rather than panicking — third-party wire formats can ship surprises and we
// don't want one bad line to take down the provider goroutine.
func (e *EventEmitter) EmitVersionedRaw(raw json.RawMessage) {
	if e == nil || e.handler == nil || len(raw) == 0 {
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		slog.Warn("event emitter: dropping non-object raw event", "error", err, "raw", string(raw))
		return
	}
	obj["v"] = versionField
	out, err := json.Marshal(obj)
	if err != nil {
		slog.Warn("event emitter: re-marshal failed; dropping", "error", err)
		return
	}
	e.handler("llm_event", out)
}

// SynthesizeToolUseID produces a deterministic id for tool_use blocks emitted
// by providers without native ids (Ollama). Format: tool_<index>_<name>.
func SynthesizeToolUseID(index int, name string) string {
	return fmt.Sprintf("tool_%d_%s", index, name)
}
