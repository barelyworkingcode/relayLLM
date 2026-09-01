package main

// Unit coverage for virtualAffinityStore and applyAffinity (virtual_affinity.go,
// relay_router.go). Full request-path coverage (pinning surviving a
// reachability flip, independent conversations, fallback when a pin's target
// is gone, re-pin after a mid-conversation failure) lives in
// relay_router_test.go alongside the rest of the virtual-model routing tests.

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// applyAffinity — pure
// ---------------------------------------------------------------------------

func TestApplyAffinity_NoPinLeavesOrderUnchanged(t *testing.T) {
	candidates := []resolvedVirtualTarget{{alias: "a", manager: &ServerManager{}}, {alias: "b", manager: &ServerManager{}}}
	got := applyAffinity(candidates, "")
	if len(got) != 2 || got[0].alias != "a" || got[1].alias != "b" {
		t.Errorf("got %+v, want unchanged order", got)
	}
}

func TestApplyAffinity_MovesPinnedTargetToFront(t *testing.T) {
	candidates := []resolvedVirtualTarget{
		{endpoint: OpenAIEndpoint{Name: "x"}, upstreamID: "m"},
		{endpoint: OpenAIEndpoint{Name: "y"}, upstreamID: "m"},
		{alias: "z", manager: &ServerManager{}},
	}
	got := applyAffinity(candidates, "alias:z")
	if len(got) != 3 || got[0].alias != "z" {
		t.Fatalf("pinned target not moved to front: %+v", got)
	}
	// Everything else keeps its relative order behind the pin.
	if got[1].endpoint.Name != "x" || got[2].endpoint.Name != "y" {
		t.Errorf("remaining order disturbed: %+v", got)
	}
}

func TestApplyAffinity_UnknownPinIsIgnored(t *testing.T) {
	candidates := []resolvedVirtualTarget{{alias: "a", manager: &ServerManager{}}, {alias: "b", manager: &ServerManager{}}}
	// "alias:removed" names a target that was dropped from config — the pin
	// is stale and must not synthesize a target that isn't there, nor
	// disturb the existing order.
	got := applyAffinity(candidates, "alias:removed")
	if len(got) != 2 || got[0].alias != "a" || got[1].alias != "b" {
		t.Errorf("got %+v, want unchanged order for an unresolvable pin", got)
	}
}

func TestApplyAffinity_AlreadyFirstIsNoop(t *testing.T) {
	candidates := []resolvedVirtualTarget{{alias: "a", manager: &ServerManager{}}, {alias: "b", manager: &ServerManager{}}}
	got := applyAffinity(candidates, "alias:a")
	if len(got) != 2 || got[0].alias != "a" || got[1].alias != "b" {
		t.Errorf("got %+v, want unchanged order when the pin is already first", got)
	}
}

// resolvedVirtualTarget.identity distinguishes an endpoint from a managed
// alias of the same name — the two namespaces must never collide inside the
// affinity store any more than they do anywhere else in the router.
func TestResolvedVirtualTarget_IdentityDistinguishesEndpointFromAlias(t *testing.T) {
	endpointTarget := resolvedVirtualTarget{endpoint: OpenAIEndpoint{Name: "shared"}, upstreamID: "m"}
	aliasTarget := resolvedVirtualTarget{alias: "shared", manager: &ServerManager{}}
	if endpointTarget.identity() == aliasTarget.identity() {
		t.Errorf("endpoint and alias with the same name must have distinct identities, both got %q", endpointTarget.identity())
	}
}

// ---------------------------------------------------------------------------
// virtualAffinityStore — TTL + LRU cap bounds, driven with a FakeClock so
// nothing here sleeps.
// ---------------------------------------------------------------------------

func TestVirtualAffinityStore_RecordThenLookupRoundTrips(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	store := newVirtualAffinityStore(clock)

	store.record("vCode", "conv-1", "endpoint:remote")
	if got := store.lookup("vCode", "conv-1"); got != "endpoint:remote" {
		t.Errorf("lookup = %q, want endpoint:remote", got)
	}
}

func TestVirtualAffinityStore_LookupMissWhenNoKeyEverRecorded(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	store := newVirtualAffinityStore(clock)
	if got := store.lookup("vCode", "conv-1"); got != "" {
		t.Errorf("lookup = %q, want empty (nothing recorded)", got)
	}
	if store.size() != 0 {
		t.Errorf("size = %d, want 0", store.size())
	}
}

// An empty conversation key means "no affinity key present in the request" —
// record must never store anything for it, and lookup must always miss.
func TestVirtualAffinityStore_EmptyConversationKeyIsNeverStored(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	store := newVirtualAffinityStore(clock)

	store.record("vCode", "", "endpoint:remote")
	if store.size() != 0 {
		t.Errorf("size = %d, want 0 — an empty conversation key must never be persisted", store.size())
	}
	if got := store.lookup("vCode", ""); got != "" {
		t.Errorf("lookup(\"\") = %q, want empty", got)
	}
}

// Two virtual models sharing a conversation key must not collide — the
// affinity namespace is (virtual, conversation), not conversation alone.
func TestVirtualAffinityStore_ScopedPerVirtualModel(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	store := newVirtualAffinityStore(clock)

	store.record("vCode", "conv-1", "endpoint:remote")
	store.record("vChat", "conv-1", "alias:local")

	if got := store.lookup("vCode", "conv-1"); got != "endpoint:remote" {
		t.Errorf("vCode pin = %q, want endpoint:remote", got)
	}
	if got := store.lookup("vChat", "conv-1"); got != "alias:local" {
		t.Errorf("vChat pin = %q, want alias:local", got)
	}
}

func TestVirtualAffinityStore_TTLExpiry(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	store := newVirtualAffinityStore(clock)

	store.record("vCode", "conv-1", "endpoint:remote")

	clock.Advance(virtualAffinityTTL) // exactly at the boundary — still valid
	if got := store.lookup("vCode", "conv-1"); got != "endpoint:remote" {
		t.Errorf("lookup at exactly the TTL = %q, want still valid", got)
	}

	clock.Advance(time.Nanosecond) // one tick past — now expired
	if got := store.lookup("vCode", "conv-1"); got != "" {
		t.Errorf("lookup past the TTL = %q, want empty", got)
	}
}

// A lookup that refreshes activity (via a subsequent record) must not expire
// on the original TTL window.
func TestVirtualAffinityStore_RecordRefreshesTTL(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	store := newVirtualAffinityStore(clock)

	store.record("vCode", "conv-1", "endpoint:remote")
	clock.Advance(virtualAffinityTTL - time.Second)
	store.record("vCode", "conv-1", "endpoint:remote") // refresh, well before expiry

	clock.Advance(virtualAffinityTTL - time.Second) // would be expired from t=0, not from the refresh
	if got := store.lookup("vCode", "conv-1"); got != "endpoint:remote" {
		t.Errorf("lookup after refresh = %q, want still valid", got)
	}
}

// A record() call sweeps expired entries as a side effect (lazy, write-path
// only — see the store's doc comment) — this proves an expired entry doesn't
// silently take up permanent room once another key is written.
func TestVirtualAffinityStore_ExpiredEntrySweptOnNextWrite(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	store := newVirtualAffinityStore(clock)

	store.record("vCode", "conv-1", "endpoint:remote")
	clock.Advance(virtualAffinityTTL + time.Second)
	store.record("vCode", "conv-2", "endpoint:other") // triggers the sweep

	if store.size() != 1 {
		t.Errorf("size = %d, want 1 (conv-1 expired and swept)", store.size())
	}
	if got := store.lookup("vCode", "conv-1"); got != "" {
		t.Errorf("conv-1 lookup = %q, want empty (expired)", got)
	}
}

func TestVirtualAffinityStore_LRUCapEvictsLeastRecentlyUsed(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	store := newVirtualAffinityStore(clock)
	store.cap = 3 // shrink for the test rather than writing 1024 entries

	store.record("v", "c1", "t1")
	clock.Advance(time.Second)
	store.record("v", "c2", "t2")
	clock.Advance(time.Second)
	store.record("v", "c3", "t3")
	clock.Advance(time.Second)

	// Store is at cap. Adding a 4th distinct key must evict c1 (oldest
	// lastUsed), not c2 or c3.
	store.record("v", "c4", "t4")

	if store.size() != 3 {
		t.Fatalf("size = %d, want 3 (cap enforced)", store.size())
	}
	if got := store.lookup("v", "c1"); got != "" {
		t.Errorf("c1 = %q, want evicted (least recently used)", got)
	}
	for key, want := range map[string]string{"c2": "t2", "c3": "t3", "c4": "t4"} {
		if got := store.lookup("v", key); got != want {
			t.Errorf("%s = %q, want %q (must survive eviction)", key, got, want)
		}
	}
}

// Refreshing an existing key must never itself trigger LRU eviction — only
// inserting a genuinely new key can push the store over its cap.
func TestVirtualAffinityStore_RefreshDoesNotTriggerEviction(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	store := newVirtualAffinityStore(clock)
	store.cap = 2

	store.record("v", "c1", "t1")
	clock.Advance(time.Second)
	store.record("v", "c2", "t2")
	clock.Advance(time.Second)
	// Refresh c1 — store stays at 2 entries, nothing should be evicted.
	store.record("v", "c1", "t1-refreshed")

	if store.size() != 2 {
		t.Fatalf("size = %d, want 2", store.size())
	}
	if got := store.lookup("v", "c1"); got != "t1-refreshed" {
		t.Errorf("c1 = %q, want t1-refreshed", got)
	}
	if got := store.lookup("v", "c2"); got != "t2" {
		t.Errorf("c2 = %q, want t2 (must not be evicted by a refresh)", got)
	}
}

// ---------------------------------------------------------------------------
// affinityKeyFromBody — pure
// ---------------------------------------------------------------------------

func TestAffinityKeyFromBody_PrefersPromptCacheKey(t *testing.T) {
	if got := affinityKeyFromBody("cache-key", "user-1"); got != "cache-key" {
		t.Errorf("got %q, want prompt_cache_key to win", got)
	}
}

func TestAffinityKeyFromBody_FallsBackToUser(t *testing.T) {
	if got := affinityKeyFromBody("", "user-1"); got != "user-1" {
		t.Errorf("got %q, want user-1", got)
	}
}

func TestAffinityKeyFromBody_EmptyWhenNeitherPresent(t *testing.T) {
	if got := affinityKeyFromBody("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
