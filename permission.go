package main

import (
	"sync"

	"github.com/google/uuid"
)

// PermissionDecision represents a user's decision on a tool permission request.
type PermissionDecision struct {
	Decision string `json:"decision"` // "allow" or "deny"
	Reason   string `json:"reason"`
}

// PermissionRequest represents a pending permission request from the hook binary.
type PermissionRequest struct {
	ID        string `json:"permissionId"`
	SessionID string `json:"sessionId"`
	ToolName  string `json:"toolName"`
	ToolInput string `json:"toolInput"`
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
// and a channel that will receive the decision.
func (m *PermissionManager) CreateRequest(sessionID, toolName, toolInput string) (PermissionRequest, chan PermissionDecision) {
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
