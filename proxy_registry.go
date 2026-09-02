package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// proxyRegistryTTL is how long a probe result is trusted before the next
// inbound request triggers a fresh probe. Picked to balance freshness
// against load — upstreams (LM Studio, OMLX, etc.) are local processes that
// can take a beat to spin up and we don't want to hammer them.
const proxyRegistryTTL = 15 * time.Second

// EndpointStatus is one entry in the registry's snapshot. Models holds the
// upstream models (IDs carry no endpoint prefix); the router prefixes them
// when emitting the aggregated /v1/models list.
type EndpointStatus struct {
	Endpoint    OpenAIEndpoint
	Online      bool
	Models      []UpstreamModel
	LastChecked time.Time
	Err         string // last probe error, for diagnostics; empty when Online
}

// ProxyRegistry tracks reachability + model lists for the configured OpenAI
// endpoints. The cache is natural-expiry (15s TTL) — no background goroutine.
// The first Snapshot() call after expiry triggers parallel probes; per-endpoint
// single-flight keeps concurrent misses from stampeding the upstream.
type ProxyRegistry struct {
	cfg *OpenAIConfig
	ttl time.Duration

	mu       sync.Mutex
	status   map[string]*EndpointStatus
	inflight map[string]chan struct{}
}

// NewProxyRegistry returns a registry seeded with the given endpoint config.
// Nil cfg or empty Endpoints yields a registry whose Snapshot returns nil and
// LookupModel always misses — safe to instantiate unconditionally.
func NewProxyRegistry(cfg *OpenAIConfig) *ProxyRegistry {
	return &ProxyRegistry{
		cfg:      cfg,
		ttl:      proxyRegistryTTL,
		status:   make(map[string]*EndpointStatus),
		inflight: make(map[string]chan struct{}),
	}
}

// Snapshot returns the current per-endpoint status. For any entry whose
// LastChecked is older than the TTL (or missing), it triggers a probe and
// blocks for the result. Probes for distinct endpoints run in parallel.
func (r *ProxyRegistry) Snapshot(ctx context.Context) []EndpointStatus {
	if r == nil || r.cfg == nil || len(r.cfg.Endpoints) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	for i := range r.cfg.Endpoints {
		ep := r.cfg.Endpoints[i]
		if r.isFresh(ep.Name) {
			continue
		}
		wg.Add(1)
		go func(ep OpenAIEndpoint) {
			defer wg.Done()
			r.probe(ctx, ep)
		}(ep)
	}
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EndpointStatus, 0, len(r.cfg.Endpoints))
	for _, ep := range r.cfg.Endpoints {
		if s, ok := r.status[ep.Name]; ok {
			dup := *s
			dup.Models = append([]UpstreamModel(nil), s.Models...)
			out = append(out, dup)
		}
	}
	return out
}

// SnapshotModels returns ModelInfo entries for every currently-online
// endpoint, upstream ids prefixed with the endpoint name (matching the
// /v1/models aggregation). Offline endpoints contribute nothing — same
// drop-on-offline policy as the reverse proxy. Results come from the 15s
// probe cache, so /api/models callers (e.g. Eve's model-list poll) share that
// throttle instead of firing a live /models fetch per request.
func (r *ProxyRegistry) SnapshotModels(ctx context.Context) []ModelInfo {
	var out []ModelInfo
	for _, status := range r.Snapshot(ctx) {
		if !status.Online {
			continue
		}
		for _, m := range status.Models {
			value := status.Endpoint.Name + "/" + m.ID
			out = append(out, ModelInfo{
				Label:    value,
				Value:    value,
				Group:    status.Endpoint.Group,
				Provider: "openai",
			})
		}
	}
	return out
}

// LookupModel parses "endpoint.Name/upstreamID" and resolves the endpoint
// from the registry, returning the upstream id with the prefix stripped. An
// unconfigured endpoint name is an immediate miss with no network call —
// an unrecognized model id must never be able to trigger a probe. For a
// configured endpoint whose cached status is missing or stale (past the
// TTL), it probes before answering: on a freshly started process nothing
// has called Snapshot yet, and without this the first chat request naming a
// live endpoint would misreport it as unknown. A believed-offline entry that
// is still fresh is a miss without probing — deliberate, so a request isn't
// forwarded into a known-down upstream, and the TTL bounds how long that
// lasts. Reuses the same single-flighted probe as Snapshot, so concurrent
// callers for the same cold endpoint share one network round trip.
func (r *ProxyRegistry) LookupModel(ctx context.Context, modelID string) (OpenAIEndpoint, string, bool) {
	if r == nil {
		return OpenAIEndpoint{}, "", false
	}
	name, upstreamID, ok := strings.Cut(modelID, "/")
	if !ok || name == "" || upstreamID == "" {
		return OpenAIEndpoint{}, "", false
	}
	ep := r.cfg.Find(name)
	if ep == nil {
		return OpenAIEndpoint{}, "", false
	}
	if !r.isFresh(name) {
		r.probe(ctx, *ep)
	}
	r.mu.Lock()
	s, known := r.status[name]
	r.mu.Unlock()
	if !known || !s.Online {
		return OpenAIEndpoint{}, "", false
	}
	return *ep, upstreamID, true
}

func (r *ProxyRegistry) isFresh(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.status[name]
	if !ok || s.LastChecked.IsZero() {
		return false
	}
	return time.Since(s.LastChecked) < r.ttl
}

// probe runs (or waits for) a single network probe of one endpoint and
// writes the result back into the registry. Single-flight per endpoint.
func (r *ProxyRegistry) probe(ctx context.Context, ep OpenAIEndpoint) {
	r.mu.Lock()
	if ch, ok := r.inflight[ep.Name]; ok {
		r.mu.Unlock()
		// A waiter may still give up early if its own caller hangs up — that's
		// fine, it just returns without a result to read. It says nothing
		// about whether the in-flight probe itself succeeds, so it must not
		// affect what gets recorded (see the WithoutCancel use below).
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return
	}
	ch := make(chan struct{})
	r.inflight[ep.Name] = ch
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.inflight, ep.Name)
		r.mu.Unlock()
		close(ch)
	}()

	// Detach the network call from the triggering request's context. A probe
	// is shared across every concurrent Snapshot() caller (single-flighted
	// above), and the caller that happened to trigger it can disconnect for
	// reasons that say nothing about the upstream (client gave up, browser
	// tab closed). Cancelling on that basis would record the endpoint
	// offline with a fresh LastChecked, poisoning the 15s cache for what may
	// be a perfectly healthy endpoint. FetchOpenAIModels has its own 3s
	// client timeout, so this can't hang indefinitely even detached.
	models, err := FetchOpenAIModels(context.WithoutCancel(ctx), ep)
	status := &EndpointStatus{
		Endpoint:    ep,
		LastChecked: time.Now(),
	}
	if err != nil {
		status.Online = false
		status.Err = err.Error()
		slog.Warn("proxy registry: endpoint offline", "endpoint", ep.Name, "error", err)
	} else {
		status.Online = true
		status.Models = models
	}

	r.mu.Lock()
	r.status[ep.Name] = status
	r.mu.Unlock()
}
