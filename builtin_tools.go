package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// BuiltinToolHandler executes a built-in tool. files contains the user's
// message attachments (may be nil). emitter is the typed event channel for
// progress updates (may be nil for non-streaming callers); toolUseID lets
// progress events pair back to the originating tool_use block in the UI.
//
// NOTE: image generation no longer ships as a built-in — it is the
// relay-comfyui MCP tool, reached through relay like any other MCP tool (see
// ADR-006 in the relay repo). This registry is retained as the generic
// mechanism for any future in-process tool that needs a progress emitter MCP
// tools can't provide.
type BuiltinToolHandler func(ctx context.Context, args json.RawMessage,
	files []FileAttachment,
	toolUseID string, emitter *EventEmitter) (string, error)

// BuiltinToolDef is the static definition of a built-in tool, used both for
// ChatToolDefs() export and for the handler registry.
type BuiltinToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
	parsedParams any            // cached parse of Parameters, set at registration
}

// BuiltinToolRegistry holds built-in tools that run alongside MCP tools in
// the BaseChatProvider tool loop. Built-in tools get an emit callback for
// progress events, which MCP tools cannot provide.
type BuiltinToolRegistry struct {
	tools    []BuiltinToolDef
	handlers map[string]BuiltinToolHandler
}

func NewBuiltinToolRegistry() *BuiltinToolRegistry {
	return &BuiltinToolRegistry{
		handlers: make(map[string]BuiltinToolHandler),
	}
}

// Register adds a tool to the registry.
func (r *BuiltinToolRegistry) Register(def BuiltinToolDef, handler BuiltinToolHandler) {
	if len(def.Parameters) > 0 {
		_ = json.Unmarshal(def.Parameters, &def.parsedParams)
	}
	r.tools = append(r.tools, def)
	r.handlers[def.Name] = handler
}

// Has returns true if the named tool is built-in.
func (r *BuiltinToolRegistry) Has(name string) bool {
	_, ok := r.handlers[name]
	return ok
}

// Call executes a built-in tool by name. files are the user's message
// attachments from the current conversation turn.
func (r *BuiltinToolRegistry) Call(ctx context.Context, name string, args json.RawMessage,
	files []FileAttachment, toolUseID string, emitter *EventEmitter) (string, error) {
	handler, ok := r.handlers[name]
	if !ok {
		return "", fmt.Errorf("unknown built-in tool: %s", name)
	}
	return handler(ctx, args, files, toolUseID, emitter)
}

// ChatToolDefs returns tool definitions in the OpenAI/Ollama compatible
// shape: [{type: "function", function: {name, description, parameters}}].
func (r *BuiltinToolRegistry) ChatToolDefs() []map[string]any {
	defs := make([]map[string]any, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.parsedParams,
			},
		})
	}
	return defs
}
