package main

import (
	"sync"
	"time"
)

// virtualAffinityTTL and virtualAffinityCap bound virtualAffinityStore's
// growth. See ADR-010 for why a deliberately stateless router now carries
// this bit of state at all.
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
	target   string // resolvedVirtualTarget.identity()
	lastUsed time.Time
}

// virtualAffinityStore pins a virtual model's chosen target per conversation
// once some target has actually served it. Two backends cannot safely share
// a reasoning transcript — see ADR-010 — so once a conversation has been
// answered, every later turn must return to the same target regardless of
// what the reachability cache currently prefers.
//
// Bounded the same way ProxyRegistry bounds its probe cache: no background
// goroutine. Expiry and the LRU cap are both enforced lazily, only on the
// write path (record), matching ProxyRegistry's natural-expiry style —
// nothing sweeps this map on a timer.
type virtualAffinityStore struct {
	clock Clock
	ttl   time.Duration
	cap   int

	mu      sync.Mutex
	entries map[virtualAffinityKey]*virtualAffinityEntry
}

// newVirtualAffinityStore returns a store using clock for TTL bookkeeping.
// clock defaults to DefaultClock when nil, so production call sites don't
// need to know about the seam.
func newVirtualAffinityStore(clock Clock) *virtualAffinityStore {
	if clock == nil {
		clock = DefaultClock
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
