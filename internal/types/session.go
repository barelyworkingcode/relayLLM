package types

import (
	"encoding/json"
	"sync"
)

// Session represents an active LLM conversation.
type Session struct {
	ID            string          `json:"sessionId"`
	ProjectID     string          `json:"projectId"`
	Name          string          `json:"name"`
	Folder        string          `json:"folder,omitempty"` // UI-only grouping label within a project; empty = ungrouped
	Directory     string          `json:"directory"`
	Model         string          `json:"model"`
	ProviderType  string          `json:"providerType"`
	Settings      json.RawMessage `json:"settings,omitempty"`
	CreatedAt     string          `json:"createdAt"`
	Messages      []Message       `json:"messages"`
	Stats         SessionStats    `json:"stats"`
	ProviderState json.RawMessage `json:"providerState,omitempty"`

	SystemPrompt  string `json:"systemPrompt,omitempty"`
	Headless      bool   `json:"headless,omitempty"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"` // pi-only: off/minimal/low/medium/high/xhigh
	// Project-scoped MCP tokens are NOT stored on the session. Relay is the
	// sole token authority: providers resolve the token just-in-time from
	// relay's bridge by ProjectID at spawn time (see spawn.ResolveProjectToken).

	// Per-session Claude permission policy (parsed from Settings at create
	// time). PermissionMode is the live mode — may be mutated by
	// SetPermissionMode for mid-session toggle. Policy is the per-project
	// allow/deny rule set forwarded by Eve.
	PermissionMode string            `json:"permissionMode,omitempty"`
	Policy         *PermissionPolicy `json:"policy,omitempty"`

	// Host is non-nil when this session's project lives on an SSH host
	// (../relay/docs/ssh-hosts.md) rather than the console. Resolved via
	// relay's bridge at create time and re-resolved at each provider spawn;
	// the stored value is the fallback when the bridge is unavailable, so a
	// persisted host session survives a relayLLM restart.
	Host *HostSpec `json:"host,omitempty"`

	provider   Provider
	processing bool
	mu         sync.Mutex
}

// Lock/Unlock expose Session's mutex directly to callers that need to read or
// mutate several fields (Messages, Stats, PermissionMode, …) as one atomic
// unit — e.g. a status snapshot copying multiple fields together. Prefer the
// narrower accessors below when only one field is involved.
func (s *Session) Lock()   { s.mu.Lock() }
func (s *Session) Unlock() { s.mu.Unlock() }

// IsProcessing reports whether this session currently has a generation in
// flight. Used by GET /api/status/detailed to render a chat row's state
// (processing/idle) — see CLAUDE.md's Relay-router section, judgment call J2,
// for why this is the only signal a session row gets: there is no
// per-session last-event timestamp, so a session can never be flagged
// "stalled" the way a proxy connection can.
func (s *Session) IsProcessing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processing
}

// SetProcessing sets the in-flight-generation flag, safe for concurrent use.
func (s *Session) SetProcessing(v bool) {
	s.mu.Lock()
	s.processing = v
	s.mu.Unlock()
}

// GetHost returns the session's HostSpec, safe for concurrent use (Host is
// refreshed from a provider's spawn goroutine while other goroutines — WS
// join, ListSessions — may read it concurrently). Named GetHost rather than
// Host because the exported Host field already owns that identifier.
func (s *Session) GetHost() *HostSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Host
}

// SetHost sets Host, safe for concurrent use.
func (s *Session) SetHost(h *HostSpec) {
	s.mu.Lock()
	s.Host = h
	s.mu.Unlock()
}

// Provider returns the current provider, safe for concurrent use.
func (s *Session) Provider() Provider {
	s.mu.Lock()
	p := s.provider
	s.mu.Unlock()
	return p
}

// SetProvider sets the provider, safe for concurrent use.
func (s *Session) SetProvider(p Provider) {
	s.mu.Lock()
	s.provider = p
	s.mu.Unlock()
}

// SwapProvider atomically replaces the provider and returns the previous
// one, so a caller tearing down the old provider never races a concurrent
// SetProvider/Provider call observing a half-updated value.
func (s *Session) SwapProvider(p Provider) Provider {
	s.mu.Lock()
	old := s.provider
	s.provider = p
	s.mu.Unlock()
	return old
}

// TryStartProcessing atomically checks-and-sets the in-flight-generation
// flag, returning false if a generation was already running. Used to
// serialize concurrent SendMessage calls on the same session without a
// separate check-then-set race window.
func (s *Session) TryStartProcessing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.processing {
		return false
	}
	s.processing = true
	return true
}

// WithLockIfNotProcessing calls fn while holding the session lock, but only
// if no generation is currently in flight; returns false without calling fn
// otherwise. Lets a caller change session state guarded by the same
// processing check SendMessage uses to serialize itself, without a separate
// check-then-set race window.
func (s *Session) WithLockIfNotProcessing(fn func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.processing {
		return false
	}
	fn()
	return true
}

// EventSink receives events from sessions and routes them to clients.
type EventSink interface {
	SendToSession(sessionID string, msg map[string]interface{})
}
