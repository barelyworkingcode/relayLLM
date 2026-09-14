package permission

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	clk "relayllm/internal/clock"
	"relayllm/internal/types"
)

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

// pendingPermission pairs a decision channel with the session it belongs to,
// so a session-wide event (stop, kill) can resolve every request that
// session owns without the caller having to track ids itself.
type pendingPermission struct {
	sessionID string
	ch        chan PermissionDecision
}

// PermissionManager tracks pending permission requests. Both the hook binary
// (HTTP POST /api/permission) and a host session's control_request stream
// (provider_claude.go) register through the same CreateRequest/Resolve pair,
// so a permission_response from Eve resolves either path transparently.
type PermissionManager struct {
	mu      sync.Mutex
	pending map[string]pendingPermission
	sink    types.EventSink
	clock   clk.Clock

	// hookTokens tracks live per-session permission-hook credentials
	// (MintHookToken/ValidateHookToken/RevokeHookToken). Keyed by the
	// SHA-256 hash of the plaintext, never the plaintext itself, so a copy
	// of this map (a heap dump, a future debug endpoint) can't be replayed
	// as a bearer.
	hookTokens     map[[32]byte]string // hash -> sessionID
	hookTokenOwner map[string][32]byte // sessionID -> hash, for revocation
}

func NewPermissionManager() *PermissionManager {
	return &PermissionManager{
		pending:        make(map[string]pendingPermission),
		clock:          clk.DefaultClock,
		hookTokens:     make(map[[32]byte]string),
		hookTokenOwner: make(map[string][32]byte),
	}
}

// SetClock swaps the clock used for permission timeouts. Tests use this to
// drive deterministic timeouts; production never calls it.
func (m *PermissionManager) SetClock(c clk.Clock) {
	m.clock = c
}

// Clock returns the clock used for permission timeouts, for callers (e.g.
// the HTTP hook handler) that need to select on it directly alongside their
// own response-writing logic instead of going through WaitForDecision.
func (m *PermissionManager) Clock() clk.Clock {
	return m.clock
}

func (m *PermissionManager) SetEventSink(sink types.EventSink) {
	m.sink = sink
}

// NotifySession forwards msg to sessionID via the configured event sink, if
// any. Returns false when no sink is configured (e.g. a headless session or
// a manager under test) so callers can distinguish "not sent" without
// reaching into the unexported sink field themselves.
func (m *PermissionManager) NotifySession(sessionID string, msg map[string]any) bool {
	if m.sink == nil {
		return false
	}
	m.sink.SendToSession(sessionID, msg)
	return true
}

// CreateRequest creates a pending permission request and returns the request
// and a channel that will receive the decision. toolUseID may be empty for
// callers that don't track it.
func (m *PermissionManager) CreateRequest(sessionID, toolName, toolInput, toolUseID string) (PermissionRequest, chan PermissionDecision) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := uuid.New().String()
	ch := make(chan PermissionDecision, 1)
	m.pending[id] = pendingPermission{sessionID: sessionID, ch: ch}

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
	p, ok := m.pending[permissionID]
	if ok {
		delete(m.pending, permissionID)
	}
	m.mu.Unlock()

	if !ok {
		return false
	}

	p.ch <- decision
	return true
}

// PendingCount returns the number of currently-outstanding requests. Test
// seam for asserting registration/cleanup without reaching into the
// unexported pending map.
func (m *PermissionManager) PendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}

// PendingIDs returns the ids of every currently-outstanding request. Test
// seam; production code never needs to enumerate pending ids.
func (m *PermissionManager) PendingIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.pending))
	for id := range m.pending {
		ids = append(ids, id)
	}
	return ids
}

// Cleanup removes a pending request (e.g., on timeout).
func (m *PermissionManager) Cleanup(permissionID string) {
	m.mu.Lock()
	delete(m.pending, permissionID)
	m.mu.Unlock()
}

// WaitForDecision blocks until the pending request identified by id resolves
// or 60s elapses, in which case it is cleaned up and denied with reason
// "No response" — the control_request path's timeout (../relay/docs/ssh-hosts.md).
// Shared timeout plumbing for any caller that isn't the HTTP hook handler
// (which has its own inline select because it also needs to write the HTTP
// response, not just a decision value).
func (m *PermissionManager) WaitForDecision(id string, ch chan PermissionDecision) PermissionDecision {
	select {
	case d := <-ch:
		return d
	case <-m.clock.After(60 * time.Second):
		m.Cleanup(id)
		return PermissionDecision{Decision: "deny", Reason: "No response"}
	}
}

// DenyAllForSession resolves every pending request belonging to sessionID
// with a deny decision. Used when a session's provider stops or is killed:
// the process that would have read the control_response is gone, so there is
// no reason to wait out the remaining timeout.
func (m *PermissionManager) DenyAllForSession(sessionID, reason string) {
	m.mu.Lock()
	var chans []chan PermissionDecision
	for id, p := range m.pending {
		if p.sessionID == sessionID {
			chans = append(chans, p.ch)
			delete(m.pending, id)
		}
	}
	m.mu.Unlock()

	for _, ch := range chans {
		ch <- PermissionDecision{Decision: "deny", Reason: reason}
	}
}

// MintHookToken generates a fresh per-session credential for the Claude Code
// PreToolUse hook's callback into /api/permission and returns its plaintext
// (32 random bytes, hex-encoded). Only its SHA-256 hash is retained, bound
// to sessionID; the plaintext is handed to the one process that needs it
// (the hook, via its env) and never persisted. Minting again for a sessionID
// that already has a token drops the old one first, so at most one token is
// ever live per session and a stale one from a crashed/replaced provider
// can't out-live its replacement.
func (m *PermissionManager) MintHookToken(sessionID string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	token := hex.EncodeToString(b)
	hash := sha256.Sum256([]byte(token))

	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.hookTokenOwner[sessionID]; ok {
		delete(m.hookTokens, old)
	}
	m.hookTokens[hash] = sessionID
	m.hookTokenOwner[sessionID] = hash
	return token
}

// ValidateHookToken reports the sessionID a hook token was minted for. A
// false result covers every failure mode identically (unknown, revoked,
// wrong session already ended) — callers must not distinguish them, since
// doing so would tell a caller with a guessed/leaked hash more than "no".
func (m *PermissionManager) ValidateHookToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	hash := sha256.Sum256([]byte(token))
	m.mu.Lock()
	defer m.mu.Unlock()
	sessionID, ok := m.hookTokens[hash]
	return sessionID, ok
}

// RevokeHookToken invalidates sessionID's hook token, if any. Called when
// the session's provider is killed (end, delete, or replaced by a fresh
// provider on resume) so a token from a session that no longer exists can
// never authenticate a permission decision for anything.
func (m *PermissionManager) RevokeHookToken(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if hash, ok := m.hookTokenOwner[sessionID]; ok {
		delete(m.hookTokens, hash)
		delete(m.hookTokenOwner, sessionID)
	}
}
