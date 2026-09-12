package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// FakeMCPClient is an MCPClient driven by scripted responses. Tests register
// tool definitions and a handler for each tool name; the chat tool loop calls
// CallTool exactly as it would against a real MCPManager.
type FakeMCPClient struct {
	mu      sync.Mutex
	tools   []FakeTool
	calls   []FakeMCPCall
	started bool
}

// FakeTool is a single scripted tool entry.
type FakeTool struct {
	Name        string
	Description string
	Schema      map[string]interface{} // JSON schema; nil if no params
	Handler     func(args json.RawMessage) (string, error)
}

// FakeMCPCall records one CallTool invocation for assertions.
type FakeMCPCall struct {
	Name string
	Args json.RawMessage
}

func NewFakeMCPClient(tools ...FakeTool) *FakeMCPClient {
	return &FakeMCPClient{tools: tools}
}

func (f *FakeMCPClient) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}

func (f *FakeMCPClient) HasTools() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tools) > 0
}

func (f *FakeMCPClient) ToolCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tools)
}

func (f *FakeMCPClient) ChatToolDefs() []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tools) == 0 {
		return nil
	}
	defs := make([]map[string]interface{}, 0, len(f.tools))
	for _, t := range f.tools {
		fn := map[string]interface{}{
			"name":        t.Name,
			"description": t.Description,
		}
		if t.Schema != nil {
			fn["parameters"] = t.Schema
		}
		defs = append(defs, map[string]interface{}{
			"type":     "function",
			"function": fn,
		})
	}
	return defs
}

func (f *FakeMCPClient) CallTool(ctx context.Context, name string, args json.RawMessage, onProgress func(message string)) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, FakeMCPCall{Name: name, Args: append(json.RawMessage(nil), args...)})
	var handler func(json.RawMessage) (string, error)
	for _, t := range f.tools {
		if t.Name == name {
			handler = t.Handler
			break
		}
	}
	f.mu.Unlock()
	if handler == nil {
		return "", fmt.Errorf("fake mcp: no handler for tool %q", name)
	}
	return handler(args)
}

func (f *FakeMCPClient) ToolNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.tools))
	for _, t := range f.tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

func (f *FakeMCPClient) ServerNames() []string {
	if f.HasTools() {
		return []string{"fake"}
	}
	return nil
}

func (f *FakeMCPClient) Close() {}

// Calls returns a snapshot of all CallTool invocations.
func (f *FakeMCPClient) Calls() []FakeMCPCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeMCPCall, len(f.calls))
	copy(out, f.calls)
	return out
}
