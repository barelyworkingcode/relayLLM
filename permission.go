package main

import (
	"strings"
	"sync"

	"github.com/google/uuid"
)

// PermissionPolicy is the per-session Claude permission policy. Sourced from
// session.Settings.permissionPolicy at session creation. relayLLM uses it for
// (a) Claude CLI flags at spawn time and (b) short-circuit evaluation in
// RegisterPermissionRoutes (matched rules skip the WS roundtrip to Eve).
type PermissionPolicy struct {
	DefaultMode  string   `json:"defaultMode,omitempty"`
	AllowedTools []string `json:"allowedTools,omitempty"`
	DeniedTools  []string `json:"deniedTools,omitempty"`
}

// MatchToolRule reports whether toolName/toolInput matches any of the given
// patterns. A bare pattern like "Read" matches any use of Read. A pattern
// of the form "ToolName:argPrefix" matches uses where the serialized
// toolInput starts with argPrefix (after a leading "{" and any whitespace).
//
// This intentionally accepts a small subset of Claude CLI's grammar — enough
// for safe-tool allowlisting without re-implementing arg parsing.
func MatchToolRule(toolName, toolInput string, patterns []string) bool {
	for _, pat := range patterns {
		colon := strings.IndexByte(pat, ':')
		if colon < 0 {
			if pat == toolName {
				return true
			}
			continue
		}
		if pat[:colon] != toolName {
			continue
		}
		prefix := pat[colon+1:]
		// Hook payload toolInput is the raw JSON (e.g. {"command":"ls -la"}).
		// Strip the outer braces and whitespace, then string-match on the prefix.
		trimmed := strings.TrimLeft(strings.TrimPrefix(toolInput, "{"), " \t\n")
		if strings.Contains(trimmed, prefix) {
			return true
		}
	}
	return false
}

// PermissionDecision represents a user's decision on a tool permission request.
type PermissionDecision struct {
	Decision string `json:"decision"` // "allow" or "deny"
	Reason   string `json:"reason"`
}

// PermissionRequest represents a pending permission request from the hook binary.
// ToolUseID lets clients (Eve) correlate the request back to the specific
// tool_use block in the rendered chat history.
type PermissionRequest struct {
	ID        string `json:"permissionId"`
	SessionID string `json:"sessionId"`
	ToolName  string `json:"toolName"`
	ToolInput string `json:"toolInput"`
	ToolUseID string `json:"toolUseId"`
}

// PermissionManager tracks pending permission requests.
type PermissionManager struct {
	mu      sync.Mutex
	pending map[string]chan PermissionDecision
	sink    EventSink
}

func NewPermissionManager() *PermissionManager {
	return &PermissionManager{
		pending: make(map[string]chan PermissionDecision),
	}
}

func (m *PermissionManager) SetEventSink(sink EventSink) {
	m.sink = sink
}

// CreateRequest creates a pending permission request and returns the request
// and a channel that will receive the decision. toolUseID may be empty for
// callers that don't track it.
func (m *PermissionManager) CreateRequest(sessionID, toolName, toolInput, toolUseID string) (PermissionRequest, chan PermissionDecision) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := uuid.New().String()
	ch := make(chan PermissionDecision, 1)
	m.pending[id] = ch

	return PermissionRequest{
		ID:        id,
		SessionID: sessionID,
		ToolName:  toolName,
		ToolInput: toolInput,
		ToolUseID: toolUseID,
	}, ch
}

// Resolve resolves a pending permission request with the given decision.
func (m *PermissionManager) Resolve(permissionID string, decision PermissionDecision) bool {
	m.mu.Lock()
	ch, ok := m.pending[permissionID]
	if ok {
		delete(m.pending, permissionID)
	}
	m.mu.Unlock()

	if !ok {
		return false
	}

	ch <- decision
	return true
}

// Cleanup removes a pending request (e.g., on timeout).
func (m *PermissionManager) Cleanup(permissionID string) {
	m.mu.Lock()
	delete(m.pending, permissionID)
	m.mu.Unlock()
}
