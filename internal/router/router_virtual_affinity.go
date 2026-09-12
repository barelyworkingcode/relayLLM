package router

import (
	clk "relayllm/internal/clock"
	"sync"
	"time"
)

// virtualAffinityTTL and virtualAffinityCap bound virtualAffinityStore's
// growth. Every other piece of the router — reachability, catalog rows,
// dispatch — is derived fresh from config and the 15s probe cache on every
// request; the router itself holds nothing about any individual
// conversation. This is the one deliberate exception, and it stays narrow:
// an in-memory, per-process map of opaque target identities, no
// persistence, no cross-process sharing, bounded and self-pruning.
const (
	virtualAffinityTTL = time.Hour
	virtualAffinityCap = 1024
)

// virtualAffinityKey scopes a conversation key to the virtual model it was
// pinned under. Two virtual models sharing the same conversation key (e.g.
// the same client-generated prompt_cache_key sent under two different
// virtual names) must not collide, so the map key is the pair, not the bare
// conversation key.
type virtualAffinityKey struct {
	virtual      string
	conversation string
}

type virtualAffinityEntry struct {
	target   string // ResolvedVirtualTarget.Identity()
	lastUsed time.Time
}

// virtualAffinityStore pins a virtual model's chosen target per conversation
// once some target has actually served it, because two backends cannot
// safely share a reasoning transcript. A production incident is why this
// exists: a single conversation had 97 turns served by a llama.cpp endpoint
// and 8 by an oMLX endpoint, interleaved, after a spurious "endpoint
// offline" reading caused a mid-conversation failover and then a failback.
// llama.cpp emits reasoning as a `content` array plus `encrypted_content`;
// oMLX emits `summary` only with `content: null`. The client (Oh My Pi)
// replays the full reasoning history on every turn, so once that history
// contained an oMLX-shaped item, routing back to llama.cpp 400'd on every
// subsequent retry with `item['content'] is not an array` — permanently, not
// just once, since every retry replayed the same poisoned history.
//
// The incompatibility is one-directional, measured against both backends:
//
//	reasoning item shape          llama.cpp                          oMLX
//	summary only (oMLX's shape)   reject: content is not an array    accept
//	content array (llama.cpp's)   accept                             accept
//	both summary and content      accept                             accept
//	content: []                   reject: content is empty           —
//
// llama.cpp's transcript replays into oMLX fine; only the reverse breaks.
// And there is nothing to translate: the raw reasoning was never sent, and
// `encrypted_content` is an opaque token valid only for the model that
// produced it, so a `content` array can't be reconstructed from an oMLX
// `summary` after the fact. A conversation that has taken even one oMLX turn
// can never be replayed to llama.cpp — the only fix is to stop mixing.
//
// An alternative was considered and rejected: instead of pinning, the router
// could inspect each request's reasoning items and rewrite or drop the ones
// that don't match the selected target's expected shape before forwarding.
// Rejected for two reasons. First, it requires the router to know the
// reasoning-item shape of every backend it might ever proxy to —
// format-specific knowledge with no other reason to live in a generic
// OpenAI-compatible proxy. Second, and worse, it silently discards reasoning
// the model already paid for: reshaping a reasoning item changes what the
// model "remembers" about its own prior turn without telling the caller,
// which is a stranger failure mode than a request simply going to the same
// backend it always has.
//
// Bounded the same way registry.ProxyRegistry bounds its probe cache: no background
// goroutine. Expiry and the LRU cap are both enforced lazily, only on the
// write path (record), matching registry.ProxyRegistry's natural-expiry style —
// nothing sweeps this map on a timer.
type virtualAffinityStore struct {
	clock clk.Clock
	ttl   time.Duration
	cap   int

	mu      sync.Mutex
	entries map[virtualAffinityKey]*virtualAffinityEntry
}

// newVirtualAffinityStore returns a store using clock for TTL bookkeeping.
// clock defaults to DefaultClock when nil, so production call sites don't
// need to know about the seam.
func newVirtualAffinityStore(clock clk.Clock) *virtualAffinityStore {
	if clock == nil {
		clock = clk.DefaultClock
	}
	return &virtualAffinityStore{
		clock:   clock,
		ttl:     virtualAffinityTTL,
		cap:     virtualAffinityCap,
		entries: make(map[virtualAffinityKey]*virtualAffinityEntry),
	}
}

// lookup returns the pinned target identity for (virtual, conversation), or
// "" when there is no pin, or it has aged past the TTL. A conversation key of
// "" means "no affinity key was present in the request" — always a miss,
// never stored.
func (s *virtualAffinityStore) lookup(virtual, conversation string) string {
	if s == nil || conversation == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[virtualAffinityKey{virtual: virtual, conversation: conversation}]
	if !ok || s.clock.Since(entry.lastUsed) > s.ttl {
		return ""
	}
	return entry.target
}

// record pins (or refreshes) conversation's target for virtual. Call only
// after an attempt has actually succeeded — routeVirtual never calls this on
// a failed attempt, so a conversation is never pinned to a target that
// hasn't proven it can serve it. This also covers the flap case: if the
// previously pinned target failed and a later candidate served instead, that
// candidate becomes the new pin — the conversation is already contaminated
// by the switch, so there is nothing to gain from trying to flap back.
func (s *virtualAffinityStore) record(virtual, conversation, target string) {
	if s == nil || conversation == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()

	key := virtualAffinityKey{virtual: virtual, conversation: conversation}
	now := s.clock.Now()
	if entry, ok := s.entries[key]; ok {
		entry.target = target
		entry.lastUsed = now
		return
	}
	if len(s.entries) >= s.cap {
		s.evictLRULocked()
	}
	s.entries[key] = &virtualAffinityEntry{target: target, lastUsed: now}
}

// size reports the current entry count. Test-only introspection (mutex-
// guarded rather than a bare field read, so it's race-detector-clean when
// called from a test goroutine after concurrent request handling).
func (s *virtualAffinityStore) size() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// pinCounts returns, for each virtual model name, the number of live pins
// (not yet past ttl) grouped by target identity — GET /api/status/detailed
// surfaces this as each virtual candidate's pinnedConversations so the
// dashboard can show where conversations are actually sticking, without
// exposing any conversation key itself (the map's value is a count, never
// the key that produced it).
func (s *virtualAffinityStore) pinCounts() map[string]map[string]int {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	out := make(map[string]map[string]int)
	for k, e := range s.entries {
		if now.Sub(e.lastUsed) > s.ttl {
			continue
		}
		byTarget, ok := out[k.virtual]
		if !ok {
			byTarget = make(map[string]int)
			out[k.virtual] = byTarget
		}
		byTarget[e.target]++
	}
	return out
}

// expireLocked drops every entry past the TTL. Called with mu held, from the
// write path only.
func (s *virtualAffinityStore) expireLocked() {
	now := s.clock.Now()
	for k, e := range s.entries {
		if now.Sub(e.lastUsed) > s.ttl {
			delete(s.entries, k)
		}
	}
}

// evictLRULocked drops the single least-recently-used entry. Called with mu
// held, only when about to insert a new key that would push the store over
// its cap — refreshing an existing key never grows the map, so it never
// triggers eviction.
func (s *virtualAffinityStore) evictLRULocked() {
	var oldestKey virtualAffinityKey
	var oldest time.Time
	found := false
	for k, e := range s.entries {
		if !found || e.lastUsed.Before(oldest) {
			oldestKey, oldest = k, e.lastUsed
			found = true
		}
	}
	if found {
		delete(s.entries, oldestKey)
	}
}
