package main

// Anthropic Messages API compatibility for the relay-router. Lets Claude
// Code (which only ever speaks Anthropic's dialect and takes a base URL, not
// a provider list) point at relayLLM via ANTHROPIC_BASE_URL and get real
// Claude models transparently proxied through to api.anthropic.com, plus an
// optional config-driven redirect of specific model ids to a local
// managed/virtual/endpoint model. See relay_router_anthropic_translate.go's
// header for what's explicitly out of scope.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// AnthropicRouterConfig is settings.json's optional router.anthropic
// section. Absent entirely -> the /v1/messages routes 404 (RelayRouter.anthropic
// stays nil) — zero behavior change from before this feature existed, the
// same off-by-default shape as RouterConfig.ReasoningEffortMap.
type AnthropicRouterConfig struct {
	// Upstream is the real Anthropic API base URL passthrough forwards to.
	// Defaults to "https://api.anthropic.com" when empty.
	Upstream string `json:"upstream,omitempty"`

	// ModelMap redirects specific Anthropic model ids (exact, case-sensitive
	// match against the request body's top-level "model" field — same
	// free-form-string convention as RouterConfig.ReasoningEffortMap) to a
	// router-dispatchable target: a managed-server alias, a configured
	// virtual model name, or an "endpoint/model" id. A key absent from this
	// map (the common case, and the only case when ModelMap is empty or
	// absent) passes straight through to Upstream untouched.
	ModelMap map[string]string `json:"modelMap,omitempty"`

	// PingIntervalSeconds sets how often a "ping" SSE event is sent during a
	// redirected streaming response to keep Claude Code's byte-idle watchdog
	// from firing while a local model is still processing a long prompt.
	// Defaults to 15 when zero or negative.
	PingIntervalSeconds int `json:"pingIntervalSeconds,omitempty"`
}

const (
	defaultAnthropicUpstream                  = "https://api.anthropic.com"
	defaultAnthropicPingIntervalSeconds       = 15
	maxAnthropicBodyBytes               int64 = 64 << 20
)

// anthropicRouterState is the validated, ready-to-use form of
// AnthropicRouterConfig, built once by setAnthropic.
type anthropicRouterState struct {
	upstream     *url.URL
	modelMap     map[string]string
	pingInterval time.Duration
	transport    http.RoundTripper
}

// setAnthropic installs the router's Anthropic-compat state (settings.json's
// router.anthropic). Subject to the same pre-serve ordering constraint as
// setReasoningEffortMap: StartRelayRouter calls this before spawning any of
// Serve's per-listener goroutines. nil cfg leaves p.anthropic nil (routes 404); an
// invalid upstream URL disables the feature with a startup log rather than
// failing the whole process, matching this codebase's "additive feature,
// fails safe" convention for router config.
func (p *RelayRouter) setAnthropic(cfg *AnthropicRouterConfig) {
	if cfg == nil {
		return
	}
	upstreamRaw := cfg.Upstream
	if upstreamRaw == "" {
		upstreamRaw = defaultAnthropicUpstream
	}
	u, err := url.Parse(upstreamRaw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		slog.Error("relay router: invalid router.anthropic.upstream, anthropic compatibility disabled", "upstream", upstreamRaw, "error", err)
		return
	}

	pingSeconds := cfg.PingIntervalSeconds
	if pingSeconds <= 0 {
		pingSeconds = defaultAnthropicPingIntervalSeconds
	}

	// DisableCompression: Go's default transport otherwise injects
	// "Accept-Encoding: gzip" when the client didn't send one and
	// transparently decompresses the response — semantically fine, but not
	// the byte-for-byte passthrough this feature promises.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true

	p.anthropic = &anthropicRouterState{
		upstream:     u,
		modelMap:     cfg.ModelMap,
		pingInterval: time.Duration(pingSeconds) * time.Second,
		transport:    t,
	}
}

// ---------------------------------------------------------------------------
// Route handlers
// ---------------------------------------------------------------------------

func (p *RelayRouter) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if p.anthropic == nil {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "anthropic compatibility is not configured on this router")
		return
	}
	body, err := readAnthropicBody(w, r)
	if err != nil {
		return // readAnthropicBody already wrote the error response
	}

	model := anthropicRequestModel(body)
	if target, ok := p.anthropic.modelMap[model]; ok && target != "" {
		p.handleAnthropicRedirect(w, r, body, model, target)
		return
	}

	p.anthropicPassthroughBody(w, r, body)
}

// handleAnthropicCountTokens answers /v1/messages/count_tokens. For a
// redirected model there is no real backend token counter to ask (OpenAI
// compat servers don't standardize one), so this returns the same crude
// byte-based estimate used elsewhere in the translator; a passthrough
// request reaches the real Anthropic API, which counts exactly.
func (p *RelayRouter) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if p.anthropic == nil {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "anthropic compatibility is not configured on this router")
		return
	}
	body, err := readAnthropicBody(w, r)
	if err != nil {
		return
	}

	model := anthropicRequestModel(body)
	if target, ok := p.anthropic.modelMap[model]; ok && target != "" {
		_ = target
		writeRouterJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateAnthropicInputTokens(body)})
		return
	}

	p.anthropicPassthroughBody(w, r, body)
}

// handleAnthropicPassthrough is the catch-all for everything else Claude
// Code's first-party (OAuth) mode talks to under a custom base URL — notably
// GET /api/claude_cli/bootstrap and HEAD /api/hello. Not buffered: the body,
// if any, streams straight through.
func (p *RelayRouter) handleAnthropicPassthrough(w http.ResponseWriter, r *http.Request) {
	if p.anthropic == nil {
		http.NotFound(w, r)
		return
	}
	// Body is unread on this path (true byte-for-byte passthrough — reading
	// it here to sniff "model"/"stream" would mean buffering and
	// re-installing it, defeating the point), so the connection is labeled
	// with what's known for free: no model, and r.ContentLength as the
	// upfront byte count.
	conn, mw := p.metrics.begin(w, r, "", false, r.ContentLength)
	conn.setTarget("anthropic-passthrough", p.anthropic.upstream.Host)
	defer p.metrics.end(conn)
	p.newAnthropicPassthroughProxy().ServeHTTP(mw, r)
}

func readAnthropicBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAnthropicBodyBytes))
	r.Body.Close()
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("request body exceeds %d bytes", maxAnthropicBodyBytes))
			return nil, err
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return nil, err
	}
	return body, nil
}

func anthropicRequestModel(body []byte) string {
	var envelope struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.Model
}

func anthropicRequestStream(body []byte) bool {
	var envelope struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.Stream
}

// anthropicPassthroughBody re-installs body onto r (it was already drained by
// readAnthropicBody to inspect "model") and forwards byte-for-byte to
// Upstream. Unlike handleAnthropicPassthrough, body is already decoded here
// (the caller read it to check the modelMap), so the connection is labeled
// with the real model/stream instead of the "" placeholder the raw
// catch-all path uses.
func (p *RelayRouter) anthropicPassthroughBody(w http.ResponseWriter, r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	conn, mw := p.metrics.begin(w, r, anthropicRequestModel(body), anthropicRequestStream(body), int64(len(body)))
	conn.setTarget("anthropic-passthrough", p.anthropic.upstream.Host)
	defer p.metrics.end(conn)
	p.newAnthropicPassthroughProxy().ServeHTTP(mw, r)
}

// newAnthropicPassthroughProxy builds a byte-for-byte reverse proxy to
// Upstream. Deliberately not newUpstreamProxy: that one replaces/strips
// Authorization, which is exactly what passthrough must never do — the
// client's real Anthropic credential (OAuth bearer or API key) has to reach
// api.anthropic.com untouched. Uses Rewrite (not the older Director hook) so
// no X-Forwarded-* headers are injected — this hop must stay invisible.
func (p *RelayRouter) newAnthropicPassthroughProxy() *httputil.ReverseProxy {
	upstream := p.anthropic.upstream
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
		},
		Transport:     p.anthropic.transport,
		FlushInterval: -1, // flush immediately for SSE streaming
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("relay router: anthropic passthrough backend error", "error", err)
			writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("backend error: %v", err))
		},
	}
}

// ---------------------------------------------------------------------------
// Redirect (translation) path
// ---------------------------------------------------------------------------

// handleAnthropicRedirect translates the Anthropic request to OpenAI, then
// re-enters the router's existing dispatch (p.handleProxy) exactly as if an
// OpenAI client had asked for target directly — getting managed-server
// leases, virtual-model failover/affinity, and the reasoning_effort rewrite
// knobs for free. tw wraps the real ResponseWriter and never forwards the
// client's Anthropic credential: innerReq is built fresh with only a
// Content-Type header, never copied from r.
func (p *RelayRouter) handleAnthropicRedirect(w http.ResponseWriter, r *http.Request, body []byte, requestedModel, target string) {
	supportsImages := p.targetSupportsImages(r.Context(), target)
	openaiBody, wantStream, aerr := anthropicToOpenAIRequest(body, target, anthropicTranslateOpts{SupportsImages: supportsImages})
	if aerr != nil {
		writeAnthropicError(w, aerr.status, aerr.errType, aerr.message)
		return
	}

	inputEstimate := estimateAnthropicInputTokens(body)
	translator := newAnthropicStreamTranslator(wantStream, requestedModel, inputEstimate)
	tw := &translatingResponseWriter{
		real:         w,
		wantStream:   wantStream,
		translator:   translator,
		pingInterval: p.anthropic.pingInterval,
	}

	innerReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://relay-router.internal/v1/chat/completions", bytes.NewReader(openaiBody))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to build backend request: "+err.Error())
		return
	}
	innerReq.Header.Set("Content-Type", "application/json")
	// Tag the re-entrant request so status_metrics.go's ProxyMetrics.begin can
	// mark its ProxyConn viaAnthropic: true — the dashboard's bytesOut for
	// this connection is the backend's OpenAI-format byte count, not what the
	// client actually received (tw translates it), and that needs to be
	// labeled rather than papered over. A context value is used rather than a
	// header because a header set on innerReq would be forwarded to the
	// upstream by newUpstreamProxy's Director.
	innerReq = innerReq.WithContext(context.WithValue(innerReq.Context(), proxyViaAnthropicKey{}, true))

	// defer, not a bare follow-up call: a client disconnecting mid-stream
	// makes httputil.ReverseProxy's body copy fail, and the stdlib's
	// documented response to that is panic(http.ErrAbortHandler) — recovered
	// higher up by net/http's own per-connection serve loop, not by anything
	// in this file. A bare `tw.finish()` after handleProxy would never run on
	// that path, leaking the ping goroutine startPing spawned in beginReal:
	// it would keep ticking past this request's lifetime and eventually fire
	// against a *http.response Go has already reset and returned to its
	// sync.Pool for a later request on the same keep-alive connection —
	// observed live as a nil-pointer panic in bufio.Writer.Write from inside
	// that goroutine, which (unlike ErrAbortHandler in the handler goroutine
	// itself) net/http does NOT recover, crashing the whole process. defer
	// guarantees finish()'s stopPing() runs during unwind, before the panic
	// continues past this frame.
	defer tw.finish()
	p.handleProxy(tw, innerReq)
}

// targetSupportsImages reports whether target's catalog entry advertises
// vision support — checked in dispatch-priority order (managed alias,
// virtual model's first candidate, endpoint model), matching handleProxy's
// own resolution order. Never claim support that can't be backed: offering
// images to a server that can't take them fails mid-turn (see CLAUDE.md).
func (p *RelayRouter) targetSupportsImages(ctx context.Context, target string) bool {
	for _, mgr := range p.managers {
		for _, entry := range mgr.ModelCatalog() {
			if entry.Alias == target {
				return entry.SupportsImages
			}
		}
	}
	if p.virtual != nil {
		if candidates := p.virtualCandidates(ctx, target); len(candidates) > 0 {
			var statuses []EndpointStatus
			if p.registry != nil {
				statuses = p.registry.Snapshot(ctx)
			}
			modalities, _, _, _ := virtualRowMetadata(candidates[0], statuses)
			for _, m := range modalities {
				if m == "image" {
					return true
				}
			}
			return false
		}
	}
	if p.registry != nil {
		if ep, upstreamID, ok := p.registry.LookupModel(ctx, target); ok {
			for _, status := range p.registry.Snapshot(ctx) {
				if status.Endpoint.Name != ep.Name {
					continue
				}
				for _, m := range status.Models {
					if m.ID == upstreamID {
						return m.SupportsImages
					}
				}
			}
		}
	}
	return false
}

// translatingResponseWriter is the http.ResponseWriter p.handleProxy writes
// the OpenAI-shaped backend response into. It defers ever touching the real
// ResponseWriter until it knows the backend's status: a 200 begins the
// Anthropic-shaped response (streaming or not); anything else is buffered
// and remapped to an Anthropic error envelope in finish(). This is what lets
// a managed-server Acquire failure, or a dead backend, surface as a real
// Anthropic error instead of a half-written SSE stream.
type translatingResponseWriter struct {
	real       http.ResponseWriter
	wantStream bool
	translator *anthropicStreamTranslator

	pingInterval time.Duration

	mu            sync.Mutex
	hdr           http.Header
	backendStatus int
	isError       bool
	errBuf        bytes.Buffer
	realStarted   bool // true once anything has actually been written to `real`

	pingStop chan struct{}
	pingWG   sync.WaitGroup
}

func (tw *translatingResponseWriter) Header() http.Header {
	if tw.hdr == nil {
		tw.hdr = make(http.Header)
	}
	return tw.hdr
}

func (tw *translatingResponseWriter) WriteHeader(status int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.backendStatus != 0 {
		return
	}
	tw.backendStatus = status
	if status != http.StatusOK {
		tw.isError = true
		return // don't touch the real ResponseWriter yet — buffered in Write, mapped in finish()
	}
	tw.beginReal()
}

// beginReal starts the real Anthropic-shaped response. Called with tw.mu held.
func (tw *translatingResponseWriter) beginReal() {
	if tw.wantStream {
		tw.realStarted = true
		tw.real.Header().Set("Content-Type", "text/event-stream")
		tw.real.Header().Set("Cache-Control", "no-cache")
		tw.real.Header().Set("Connection", "keep-alive")
		tw.real.WriteHeader(http.StatusOK)
		tw.translator.Start(tw.real)
		tw.flushReal()
		tw.startPing()
	}
	// Non-streaming: nothing is written until finish() has the full message.
}

func (tw *translatingResponseWriter) Write(b []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.backendStatus == 0 {
		// A Write without a prior WriteHeader implies 200, same as the
		// standard library's own ResponseWriter contract.
		tw.backendStatus = http.StatusOK
		tw.beginReal()
	}
	if tw.isError {
		tw.errBuf.Write(b)
		return len(b), nil
	}
	if tw.wantStream {
		tw.translator.Feed(b, tw.real)
		tw.flushReal()
	} else {
		tw.translator.Feed(b, io.Discard)
	}
	return len(b), nil
}

// Flush satisfies http.Flusher — required for httputil.ReverseProxy's
// FlushInterval: -1 (used by every backend p.handleProxy dispatches to) to
// flush through this writer at all. Gated on wantStream: in non-streaming
// mode nothing has been written to the real ResponseWriter yet (finish()
// writes the whole aggregated Message in one shot), and calling the real
// http.Flusher before its first WriteHeader implicitly sends a premature
// 200 — this is called on every proxied write regardless of whether Write
// itself did anything, so the guard has to live here too, not just in Write.
func (tw *translatingResponseWriter) Flush() {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.wantStream {
		tw.flushReal()
	}
}

// flushReal flushes the real ResponseWriter. Called with tw.mu held.
func (tw *translatingResponseWriter) flushReal() {
	if f, ok := tw.real.(http.Flusher); ok {
		f.Flush()
	}
}

func (tw *translatingResponseWriter) startPing() {
	if tw.pingInterval <= 0 {
		return
	}
	tw.pingStop = make(chan struct{})
	tw.pingWG.Add(1)
	go func() {
		defer tw.pingWG.Done()
		// Defense in depth: this goroutine's whole reason to exist is
		// handleAnthropicRedirect's `defer tw.finish()` reliably stopping it
		// before the request's *http.response can be recycled (see that
		// comment for the crash this guards against) — but a panic here is
		// a whole-process crash regardless of cause, on a goroutine relayLLM
		// doesn't otherwise get a chance to log or recover from. A ping
		// event is disposable; losing one to a swallowed panic is a far
		// better failure mode than taking down every other in-flight
		// request on this process.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("relay router: anthropic ping goroutine recovered from panic", "panic", r)
			}
		}()
		ticker := time.NewTicker(tw.pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				tw.pingTick()
			case <-tw.pingStop:
				return
			}
		}
	}()
}

// pingTick writes one ping event under tw.mu. A defer (not explicit
// Lock/Unlock) is required here specifically: startPing's goroutine recovers
// a panic from this call, and recover only runs after every defer in the
// panicking frame has already unwound — an explicit Unlock placed after the
// write, instead, would never execute on a panicking write and leave tw.mu
// permanently locked, deadlocking finish() (called from the request's own
// goroutine) forever instead of just losing one ping.
func (tw *translatingResponseWriter) pingTick() {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if !tw.isError && tw.backendStatus == http.StatusOK {
		io.WriteString(tw.real, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		tw.flushReal()
	}
}

// stopPing must be called WITHOUT tw.mu held — it waits for the ping
// goroutine to exit, and that goroutine itself needs to acquire tw.mu to
// finish its current tick. Calling this while holding tw.mu deadlocks
// whenever a tick is in flight at the same moment.
func (tw *translatingResponseWriter) stopPing() {
	if tw.pingStop != nil {
		close(tw.pingStop)
		tw.pingWG.Wait()
	}
}

// finish is called once, after p.handleProxy(tw, innerReq) returns, to
// deliver whatever the exchange produced: a mapped error envelope, the
// streaming Finish() events, or one aggregated non-streaming Message.
func (tw *translatingResponseWriter) finish() {
	tw.stopPing()

	tw.mu.Lock()
	defer tw.mu.Unlock()

	if tw.isError {
		// realStarted can only be true here if something already streamed a
		// 200 to the client before the backend (or a re-entrant dispatch
		// inside p.handleProxy) surfaced an error — e.g. a mid-stream
		// failure on a code path that still routes through the default
		// newUpstreamProxy ErrorHandler. There is nothing to send a fresh
		// status line for at that point; end the stream with an in-band
		// error event instead of attempting a second WriteHeader (which
		// net/http would silently drop anyway, but the client would still
		// be left with a stream that never reached message_stop).
		mapped := mapAnthropicBackendError(tw.backendStatus, tw.errBuf.Bytes())
		if tw.realStarted {
			writeSSEEvent(tw.real, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": mapped.errType, "message": mapped.message},
			})
			tw.flushReal()
			return
		}
		writeAnthropicError(tw.real, mapped.status, mapped.errType, mapped.message)
		return
	}
	if tw.backendStatus == 0 {
		if tw.realStarted {
			writeSSEEvent(tw.real, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": "no response from backend"},
			})
			tw.flushReal()
			return
		}
		writeAnthropicError(tw.real, http.StatusBadGateway, "api_error", "no response from backend")
		return
	}
	if tw.wantStream {
		tw.translator.Finish(tw.real)
		tw.flushReal()
		return
	}
	tw.real.Header().Set("Content-Type", "application/json")
	tw.real.WriteHeader(http.StatusOK)
	json.NewEncoder(tw.real).Encode(tw.translator.BuildMessage())
}

// ---------------------------------------------------------------------------
// Error envelope
// ---------------------------------------------------------------------------

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}
