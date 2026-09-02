package main

// Coverage for ProxyRegistry.LookupModel's on-demand probing (the
// registry-lookup-cold-start fix). Before this fix, LookupModel only ever
// read the cache — Snapshot was the only thing that probed — so a freshly
// started process whose first request named an endpoint model 400'd with
// "unknown model" even though the endpoint was up. See CLAUDE.md's
// Relay-router section for the full story.
//
// Style matches relay_router_test.go / relay_router_reasoning_effort_test.go:
// httptest fakes standing in for upstreams, no sleeping on real time.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// countingOpenAIUpstream is a fake OpenAI-compatible upstream whose /models
// behavior flips on a shared atomic switch (healthy true → 200 with one
// model; false → 500), and which counts every request it receives. Used to
// prove exactly how many times (if any) the registry actually dialed out.
func countingOpenAIUpstream(t *testing.T, modelID string, healthy *atomic.Bool, reqCount *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": modelID}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Requirement 1 (regression pin): a registry that has never been probed
// resolves an online endpoint's model on the very first LookupModel call —
// no preceding Snapshot. This is the exact bug: LookupModel used to only
// read the cache, so a cold registry always missed here.
func TestProxyRegistry_LookupModel_ColdRegistryProbesAndResolves(t *testing.T) {
	var reqCount atomic.Int64
	healthy := &atomic.Bool{}
	healthy.Store(true)
	upstream := countingOpenAIUpstream(t, "CodeFast", healthy, &reqCount)

	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "omlx", BaseURL: upstream.URL + "/v1"}},
	})

	ep, upstreamID, ok := registry.LookupModel(context.Background(), "omlx/CodeFast")
	if !ok {
		t.Fatal("LookupModel on a never-probed registry returned a miss for an online endpoint")
	}
	if ep.Name != "omlx" {
		t.Errorf("endpoint = %q, want %q", ep.Name, "omlx")
	}
	if upstreamID != "CodeFast" {
		t.Errorf("upstreamID = %q, want %q", upstreamID, "CodeFast")
	}
	if reqCount.Load() != 1 {
		t.Errorf("upstream request count = %d, want 1 (the on-demand probe)", reqCount.Load())
	}
}

// Requirement 3: an endpoint name that isn't in config at all is a miss, and
// must never trigger a probe — an unrecognized model id must not be able to
// cause network work. Proven against a *configured* upstream so a bug that
// probed indiscriminately (e.g. probing every endpoint on any miss) would be
// caught, not just a bug that probed the literal unconfigured name.
func TestProxyRegistry_LookupModel_UnconfiguredEndpoint_NeverProbes(t *testing.T) {
	var reqCount atomic.Int64
	healthy := &atomic.Bool{}
	healthy.Store(true)
	upstream := countingOpenAIUpstream(t, "m", healthy, &reqCount)

	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "known", BaseURL: upstream.URL + "/v1"}},
	})

	_, _, ok := registry.LookupModel(context.Background(), "unknown/model")
	if ok {
		t.Error("LookupModel on an unconfigured endpoint name returned a hit, want miss")
	}
	if reqCount.Load() != 0 {
		t.Errorf("upstream request count = %d, want 0 — an unconfigured name must never probe", reqCount.Load())
	}
}

// Requirement 4: an endpoint believed offline within the TTL stays a miss on
// a second call, and is not re-probed — the deliberate "known-down, don't
// hammer it" behavior the TTL exists to bound.
func TestProxyRegistry_LookupModel_OfflineWithinTTL_NotReprobed(t *testing.T) {
	var reqCount atomic.Int64
	healthy := &atomic.Bool{}
	healthy.Store(false) // upstream starts unhealthy
	upstream := countingOpenAIUpstream(t, "m", healthy, &reqCount)

	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "ep", BaseURL: upstream.URL + "/v1"}},
	})

	if _, _, ok := registry.LookupModel(context.Background(), "ep/m"); ok {
		t.Fatal("first call: expected miss against an offline upstream")
	}
	if reqCount.Load() != 1 {
		t.Fatalf("after first call: upstream request count = %d, want 1", reqCount.Load())
	}

	// Upstream is now healthy, but the cached "offline" status is still
	// fresh (well within the 15s TTL) — the registry must trust the cache,
	// not re-dial, and still report a miss.
	healthy.Store(true)
	if _, _, ok := registry.LookupModel(context.Background(), "ep/m"); ok {
		t.Error("second call within TTL: expected miss (cache still says offline)")
	}
	if reqCount.Load() != 1 {
		t.Errorf("after second call within TTL: upstream request count = %d, want 1 (no re-probe)", reqCount.Load())
	}
}

// Requirement 5: once the cached status goes stale (TTL expired), a
// previously-offline endpoint that has since come back online is probed
// again and resolves. Drives "time passing" by setting the registry's ttl
// field directly (no FakeClock seam on ProxyRegistry) rather than sleeping.
func TestProxyRegistry_LookupModel_ReprobesAfterTTLExpiry(t *testing.T) {
	var reqCount atomic.Int64
	healthy := &atomic.Bool{}
	healthy.Store(false)
	upstream := countingOpenAIUpstream(t, "m", healthy, &reqCount)

	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "ep", BaseURL: upstream.URL + "/v1"}},
	})

	if _, _, ok := registry.LookupModel(context.Background(), "ep/m"); ok {
		t.Fatal("first call: expected miss against an offline upstream")
	}
	if reqCount.Load() != 1 {
		t.Fatalf("after first call: upstream request count = %d, want 1", reqCount.Load())
	}

	// Force the cached entry stale without waiting out the real 15s TTL.
	registry.ttl = 0
	healthy.Store(true)

	ep, upstreamID, ok := registry.LookupModel(context.Background(), "ep/m")
	if !ok {
		t.Fatal("call after TTL expiry: expected a hit — endpoint is back online and the cache is stale")
	}
	if ep.Name != "ep" || upstreamID != "m" {
		t.Errorf("got (%q, %q), want (\"ep\", \"m\")", ep.Name, upstreamID)
	}
	if reqCount.Load() != 2 {
		t.Errorf("upstream request count = %d, want 2 (re-probed after TTL expiry)", reqCount.Load())
	}
}

// Requirement 6: several concurrent LookupModel calls for the same cold
// endpoint collapse into exactly one upstream probe — the per-endpoint
// single-flight in probe() still holds when triggered from LookupModel
// instead of only from Snapshot.
func TestProxyRegistry_LookupModel_ConcurrentCallsSingleFlight(t *testing.T) {
	var reqCount atomic.Int64
	healthy := &atomic.Bool{}
	healthy.Store(true)
	upstream := countingOpenAIUpstream(t, "m", healthy, &reqCount)

	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "ep", BaseURL: upstream.URL + "/v1"}},
	})

	const n = 20
	var wg sync.WaitGroup
	var misses atomic.Int64
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			if _, _, ok := registry.LookupModel(context.Background(), "ep/m"); !ok {
				misses.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if reqCount.Load() != 1 {
		t.Errorf("upstream request count = %d, want 1 (single-flighted probe)", reqCount.Load())
	}
	if misses.Load() != 0 {
		t.Errorf("%d/%d concurrent LookupModel calls missed, want 0 (all should see the endpoint online)", misses.Load(), n)
	}
}
