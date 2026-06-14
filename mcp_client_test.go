package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Hermetic coverage for the real MCPManager (previously 0% — only the
// FakeMCPClient was exercised). The subprocess Start() path stays in the
// //go:build live tier (mcp_integration_test.go); here we drive the actual
// CallTool / progress-routing / tool-def-shaping logic against an in-process
// MCP server over the SDK's in-memory transport, so a regression in tool
// dispatch or result extraction fails the default `go test ./...`.

// startInMemoryMCP wires a real in-process MCP server (tools added by register)
// into a fresh MCPManager via the SDK's in-memory transport. Crucially it uses
// the manager's OWN newMCPClient(), so the production progress-notification
// handler is the one under test. Tool discovery mirrors Start(). Cleanup closes
// both sessions.
func startInMemoryMCP(t *testing.T, serverName string, register func(*mcp.Server)) *MCPManager {
	t.Helper()
	m := NewMCPManager(nil)

	srv := mcp.NewServer(&mcp.Implementation{Name: "test-srv", Version: "v0"}, nil)
	register(srv)

	ct, st := mcp.NewInMemoryTransports()
	ctx := t.Context()

	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := m.newMCPClient().Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Wait()
	})

	m.servers[serverName] = &mcpServerConn{session: cs}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range res.Tools {
		m.tools = append(m.tools, MCPTool{ServerName: serverName, Name: tool.Name, Tool: tool})
		m.toolMap[tool.Name] = serverName
	}
	return m
}

func registerEcho(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "echo tool"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
		})
}

// ---------------------------------------------------------------------------
// Real round-trip: dispatch + result extraction
// ---------------------------------------------------------------------------

func TestMCPManager_CallTool_RoundTrip(t *testing.T) {
	m := startInMemoryMCP(t, "srv", registerEcho)

	out, err := m.CallTool(t.Context(), "echo", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "pong" {
		t.Errorf("CallTool result = %q; want pong", out)
	}
}

// Discovery state populated by a real ListTools.
func TestMCPManager_DiscoveryState(t *testing.T) {
	m := startInMemoryMCP(t, "srv", registerEcho)

	if !m.HasTools() {
		t.Error("HasTools = false; want true")
	}
	if n := m.ToolCount(); n != 1 {
		t.Errorf("ToolCount = %d; want 1", n)
	}
	if names := m.ToolNames(); len(names) != 1 || names[0] != "echo" {
		t.Errorf("ToolNames = %v; want [echo]", names)
	}
	if servers := m.ServerNames(); len(servers) != 1 || servers[0] != "srv" {
		t.Errorf("ServerNames = %v; want [srv]", servers)
	}
}

// ---------------------------------------------------------------------------
// Progress streaming through the production handler + token correlation
// ---------------------------------------------------------------------------

func TestMCPManager_CallTool_StreamsProgress(t *testing.T) {
	// Progress notifications are delivered asynchronously and CallTool deletes
	// the per-call sink when it returns, so "assert after CallTool returns" is
	// inherently racy (a late notification is dropped). Instead the in-process
	// tool sends both notifications then BLOCKS on release until the test has
	// observed them, and only then returns its result — making delivery
	// deterministic without sleeps. (This race is a real property of the
	// progress mechanism, not a test artifact.)
	release := make(chan struct{})
	register := func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: "work", Description: "emits progress"},
			func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
				if token := req.Params.GetProgressToken(); token != nil {
					_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{Message: "step 1", ProgressToken: token})
					_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{Message: "step 2", ProgressToken: token})
				}
				<-release // hold the result until the client has the progress
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil, nil
			})
	}
	m := startInMemoryMCP(t, "srv", register)

	received := make(chan string, 8)
	type result struct {
		out string
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		out, err := m.CallTool(t.Context(), "work", nil, func(msg string) { received <- msg })
		resCh <- result{out, err}
	}()

	// Observe both progress messages (order on a single connection is stable).
	got := []string{<-received, <-received}
	close(release) // let the tool return its result
	r := <-resCh

	if r.err != nil {
		t.Fatalf("CallTool: %v", r.err)
	}
	if r.out != "done" {
		t.Errorf("result = %q; want done", r.out)
	}
	if got[0] != "step 1" || got[1] != "step 2" {
		t.Errorf("progress messages = %v; want [step 1, step 2]", got)
	}

	// The per-call progress sink must be unregistered once CallTool returns —
	// otherwise the progress map leaks an entry per call.
	m.progressMu.Lock()
	n := len(m.progress)
	m.progressMu.Unlock()
	if n != 0 {
		t.Errorf("progress map has %d leftover entries; want 0 (defer cleanup)", n)
	}
}

// ---------------------------------------------------------------------------
// Dispatch guard rails (no connection needed)
// ---------------------------------------------------------------------------

func TestMCPManager_CallTool_UnknownTool(t *testing.T) {
	m := NewMCPManager(nil)
	_, err := m.CallTool(t.Context(), "nope", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("CallTool unknown tool err = %v; want 'unknown tool'", err)
	}
}

func TestMCPManager_CallTool_ServerNotConnected(t *testing.T) {
	m := NewMCPManager(nil)
	m.toolMap["ghost"] = "dead-server" // routed to a server that isn't in m.servers
	_, err := m.CallTool(t.Context(), "ghost", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("CallTool disconnected server err = %v; want 'not connected'", err)
	}
}

// ---------------------------------------------------------------------------
// ChatToolDefs shaping — the function-def contract sent to Ollama/OpenAI
// ---------------------------------------------------------------------------

func TestMCPManager_ChatToolDefs_Shape(t *testing.T) {
	m := startInMemoryMCP(t, "srv", registerEcho)

	defs := m.ChatToolDefs()
	if len(defs) != 1 {
		t.Fatalf("ChatToolDefs len = %d; want 1", len(defs))
	}
	if defs[0]["type"] != "function" {
		t.Errorf("def type = %v; want function", defs[0]["type"])
	}
	fn, ok := defs[0]["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("def.function not a map: %T", defs[0]["function"])
	}
	if fn["name"] != "echo" {
		t.Errorf("function.name = %v; want echo", fn["name"])
	}
	if fn["description"] != "echo tool" {
		t.Errorf("function.description = %v; want 'echo tool'", fn["description"])
	}
	// Production surfaces parameters iff the discovered tool carries a schema.
	if m.tools[0].Tool.InputSchema != nil {
		if _, present := fn["parameters"]; !present {
			t.Error("tool has InputSchema but function.parameters is absent")
		}
	}
}

func TestMCPManager_ChatToolDefs_EmptyIsNil(t *testing.T) {
	if defs := NewMCPManager(nil).ChatToolDefs(); defs != nil {
		t.Errorf("ChatToolDefs on empty manager = %v; want nil", defs)
	}
}

// ---------------------------------------------------------------------------
// Close resets routing state
// ---------------------------------------------------------------------------

func TestMCPManager_Close_ResetsState(t *testing.T) {
	// Populate routing state without a live session so Close exercises the
	// map-reset path independently of session teardown (covered by the
	// in-memory tests' cleanup).
	m := NewMCPManager(nil)
	m.tools = []MCPTool{{ServerName: "s", Name: "t", Tool: &mcp.Tool{Name: "t"}}}
	m.toolMap["t"] = "s"

	m.Close()

	if m.HasTools() {
		t.Error("HasTools = true after Close; want false")
	}
	if n := m.ToolCount(); n != 0 {
		t.Errorf("ToolCount = %d after Close; want 0", n)
	}
	if names := m.ToolNames(); len(names) != 0 {
		t.Errorf("ToolNames = %v after Close; want empty", names)
	}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestExtractToolResultText(t *testing.T) {
	if got := extractToolResultText(nil); got != "" {
		t.Errorf("nil result = %q; want empty", got)
	}
	if got := extractToolResultText(&mcp.CallToolResult{}); got != "" {
		t.Errorf("empty content = %q; want empty", got)
	}

	single := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}}
	if got := extractToolResultText(single); got != "hello" {
		t.Errorf("single text = %q; want hello", got)
	}

	multi := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "foo"},
		&mcp.TextContent{Text: "bar"},
	}}
	if got := extractToolResultText(multi); got != "foobar" {
		t.Errorf("multi text = %q; want foobar", got)
	}

	// Non-text content falls back to JSON marshalling.
	img := &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: []byte("x"), MIMEType: "image/png"}}}
	if got := extractToolResultText(img); !strings.Contains(got, "image/png") {
		t.Errorf("image content = %q; want JSON containing image/png", got)
	}
}

func TestProgressTokenString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"abc123", "abc123"},   // string passthrough
		{float64(5), "5"},      // JSON numbers decode to float64
		{42, "42"},             // int via %v
		{nil, "<nil>"},         // defensive
	}
	for _, c := range cases {
		if got := progressTokenString(c.in); got != c.want {
			t.Errorf("progressTokenString(%v) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestNewMCPManager_InitializesMaps(t *testing.T) {
	m := NewMCPManager(nil)
	if m.servers == nil || m.toolMap == nil || m.progress == nil {
		t.Errorf("NewMCPManager left a nil map: servers=%v toolMap=%v progress=%v",
			m.servers == nil, m.toolMap == nil, m.progress == nil)
	}
}
