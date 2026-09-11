package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// RelayRouter aggregates managed-server aliases (llama.cpp, MLX, …) and
// reachable OpenAI endpoint models behind one OpenAI-compatible listener.
// Dispatch is by the request body's `model` field: bare alias → first
// matching manager; virtual name → ordered candidate targets, attempted in
// turn until one works (see routeVirtual); `endpoint.Name/id` → matching
// OpenAI upstream.
// Endpoints that fail their last probe drop out of /v1/models and refuse
// direct (`endpoint.Name/id`) routing until the next 15s TTL cycle — a
// virtual model may still route through an offline-believed endpoint as a
// last resort (see candidatesForVirtual).
type RelayRouter struct {
	managers []*ServerManager
	registry *ProxyRegistry
	virtual  *VirtualLLMConfig
	affinity *virtualAffinityStore
	server   *http.Server

	// listeners holds every bound listener from Listen, one per configured
	// bind address. Serve fans the shared server out across all of them;
	// Close (via p.server.Close) tears down every one at once — the stdlib
	// tracks a server's listeners internally regardless of how many Serve
	// calls registered them, so this field exists only for Listen to hand
	// listeners to Serve, not for shutdown bookkeeping.
	listeners []net.Listener

	// metrics instruments the proxy path for GET /api/status/detailed
	// (status_metrics.go). Always non-nil after NewRelayRouter; the
	// ProxyConn methods it hands out are all nil-safe anyway so a hand-built
	// router in a test needs no wiring.
	metrics *ProxyMetrics

	// reasoningEffortMap rewrites a top-level "reasoning_effort" string field
	// on every proxied body before it reaches a backend — see RouterConfig
	// and rewriteProxyBody. nil/empty (the zero value, and what every
	// constructor leaves it at) means no rewriting at all; wired in from
	// settings.json via StartRelayRouter's trailing *RouterConfig parameter
	// (see setReasoningEffortMap), applied before the serving goroutine is
	// spawned rather than a NewRelayRouter constructor parameter, to keep
	// this opt-in feature from touching NewRelayRouter's much larger set of
	// call sites. reasoningEffortTemplateKwargs, below, is its sibling knob.
	reasoningEffortMap map[string]string

	// reasoningEffortTemplateKwargs merges an object into the proxied body's
	// top-level "chat_template_kwargs" field — see RouterConfig for the
	// oMLX/llama.cpp measurements this exists to satisfy, and
	// rewriteProxyBody / applyReasoningEffortTemplateKwargs for the merge
	// semantics. Same nil/empty-means-off shape, wired in the same way
	// (setReasoningEffortTemplateKwargs, called before serving starts), as
	// reasoningEffortMap above.
	reasoningEffortTemplateKwargs map[string]map[string]any

	// tlsCert/tlsKey, when both set, make Serve serve every listener over
	// TLS instead of plain http — this is the router's own listener(s)
	// (the relayLLM-to-upstream hop is Part A, above; this is a client
	// dialing INTO the router). Wired in the same before-the-serving-goroutine
	// way as the reasoningEffort* fields above, via setTLS.
	tlsCert string
	tlsKey  string

	// anthropic holds the Anthropic Messages API compatibility state
	// (settings.json's router.anthropic) — see relay_router_anthropic.go.
	// nil means the feature is off: /v1/messages and friends 404. Wired in
	// the same pre-serve-setter way as the reasoningEffort* fields above, via
	// setAnthropic.
	anthropic *anthropicRouterState
}

// RouterConfig holds relay-router behavior that doesn't belong to any one
// backend — the reasoning-effort rewrite table and its sibling
// chat_template_kwargs merge table. Maps to settings.json's optional
// top-level "router" section.
type RouterConfig struct {
	// ReasoningEffortMap rewrites (or, mapped to "", removes) a top-level
	// string "reasoning_effort" field on every proxied request body. Absent
	// or empty disables rewriting entirely — the default, so behavior is
	// byte-identical to a settings.json with no "router" section at all.
	//
	// Keys and values are free-form strings on purpose, not a fixed
	// vocabulary: backends disagree about what they accept. Measured against
	// a real llama.cpp server (Qwen3.8-27B): "none" is accepted and turns
	// reasoning off; "low"/"medium"/"high"/"xhigh" are accepted and produce
	// reasoning; "minimal" 500s ("Unexpected reasoning effort minimal.
	// Supported types are xhigh (default), medium, and low."). Oh My Pi has
	// no wire value that means "off" — its `--thinking off` clamps to the
	// lowest entry in the model's configured `efforts` list and sends that
	// verbatim, so "minimal" is the lowest value such a client can be made
	// to send. Mapping {"minimal": "none"} turns that into the off signal
	// the backend actually understands. See CLAUDE.md's Relay-router section
	// for the full story.
	ReasoningEffortMap map[string]string `json:"reasoningEffortMap,omitempty"`

	// ReasoningEffortTemplateKwargs merges an object into a proxied body's
	// top-level "chat_template_kwargs" field when the request's ORIGINAL
	// "reasoning_effort" string value — matched the same way, and BEFORE,
	// ReasoningEffortMap rewrites it (see rewriteProxyBody) — matches a
	// configured key. Absent or empty disables this entirely, the default,
	// so behavior is unchanged from before this field existed.
	//
	// ReasoningEffortMap's value swap only fixes backends that interpret
	// reasoning_effort server-side (llama.cpp does). It does not fix oMLX:
	// measured at omlx/server.py:3594, oMLX merges request.reasoning_effort
	// verbatim into chat_template_kwargs and hands it to the model's Jinja
	// template — there is no server-side meaning to rewrite. The MLX build
	// of the measured model (CodeFast) uses the older Qwen convention
	// (enable_thinking), not reasoning_effort, so no VALUE swap of
	// reasoning_effort can reach it — the template never reads that field at
	// all. It needs a field-SHAPE rewrite: inject a different field.
	// Measured reasoning output length against oMLX CodeFast:
	//
	//	request                                            reasoning returned
	//	baseline                                            101 chars
	//	reasoning_effort: "none"                             94 chars — no effect
	//	chat_template_kwargs: {"enable_thinking": false}      0 chars — off
	//
	// The same chat_template_kwargs also turns reasoning off against
	// llama.cpp (0 chars, measured on "europa"), so each backend tolerates
	// the other's mechanism harmlessly — llama.cpp ignores an
	// unrecognized chat_template_kwargs key, oMLX ignores reasoning_effort
	// once nothing reads it. Configuring both knobs together (this field and
	// ReasoningEffortMap) is what makes "turn reasoning off" portable across
	// both backends from one client-side value.
	//
	// Matched against the value BEFORE ReasoningEffortMap's rewrite, not
	// after, because the two knobs describe ONE inbound client value
	// triggering TWO independent rewrites: configuring
	// {"minimal": "none"} (ReasoningEffortMap) alongside
	// {"minimal": {"enable_thinking": false}} (this field) must both fire
	// off the client's original "minimal". Matching post-rewrite would
	// require this field's keys to track whatever ReasoningEffortMap
	// happens to rewrite "minimal" INTO ("none") rather than what the client
	// actually sent — coupling the two maps together for no reason, and
	// breaking silently if either is reconfigured independently.
	//
	// Values are arbitrary JSON (bool, string, number, …), not just bools:
	// oMLX forwards chat_template_kwargs to the Jinja template verbatim, so
	// this passes values through untyped exactly like ReasoningEffortMap's
	// free-form string values do.
	//
	// The merge never overwrites a key the client's own body already sets
	// under chat_template_kwargs — mirroring oMLX's own
	// merged.setdefault(...) server-side, so the client's explicit choice
	// always wins over ours. See applyReasoningEffortTemplateKwargs.
	ReasoningEffortTemplateKwargs map[string]map[string]any `json:"reasoningEffortTemplateKwargs,omitempty"`

	// Anthropic enables Anthropic Messages API compatibility (/v1/messages,
	// /v1/messages/count_tokens, and an /api/ passthrough) on this same
	// listener — see relay_router_anthropic.go. Absent (nil, the default)
	// means those routes 404; behavior is otherwise unchanged.
	Anthropic *AnthropicRouterConfig `json:"anthropic,omitempty"`
}

// setReasoningEffortMap installs the router-level reasoning_effort rewrite
// table (settings.json's router.reasoningEffortMap). nil or empty disables
// rewriting, which is also this field's zero value, so a router this is
// never called on behaves exactly as it did before the feature existed.
//
// MUST be called before the router starts serving — StartRelayRouter is the
// only production call site, and it calls this before spawning any of
// Serve's per-listener goroutines. Go's memory model guarantees a
// goroutine's creation happens-before its execution, so every
// request-handling goroutine transitively spawned from one of those is
// guaranteed to observe the write; a call made after they're already
// running (the previous shape: main called the exported
// SetReasoningEffortMap after StartRelayRouter had already returned) races
// the first accepted connection under -race. Kept unexported, rather than
// removed, so tests that drive a router's handler directly without ever
// calling Listen/Serve (no goroutine, so no race) can still configure it
// post-construction.
func (p *RelayRouter) setReasoningEffortMap(m map[string]string) {
	p.reasoningEffortMap = m
}

// setReasoningEffortTemplateKwargs installs the router-level
// chat_template_kwargs merge table (settings.json's
// router.reasoningEffortTemplateKwargs). nil or empty disables it — also
// this field's zero value — so a router this is never called on behaves
// exactly as it did before the feature existed. Subject to the same
// pre-serve constraint as setReasoningEffortMap above (see its comment):
// StartRelayRouter calls this before spawning any of Serve's per-listener
// goroutines.
func (p *RelayRouter) setReasoningEffortTemplateKwargs(m map[string]map[string]any) {
	p.reasoningEffortTemplateKwargs = m
}

// setTLS installs the router listener's TLS cert/key pair (settings.json has
// no section for this — it comes from --router-tls-cert/--router-tls-key,
// validated as a matched pair in main). Subject to the same pre-serve
// ordering constraint as setReasoningEffortMap above: StartRelayRouter calls
// this before spawning any of Serve's per-listener goroutines.
func (p *RelayRouter) setTLS(cert, key string) {
	p.tlsCert = cert
	p.tlsKey = key
}

// NewRelayRouter creates a router reporting addr as its canonical address
// (Addr()) — it does not bind anything itself; call Listen for that. A
// single addr rather than a list keeps this constructor's ~60 test call
// sites (which never call Listen) unchanged by multi-bind support; addr is
// overwritten with the first real bound address once Listen runs. Nil
// entries in managers are dropped; registry may be nil to disable the
// endpoint branch; virtual may be nil to disable the virtual-model branch. A
// router with no live backends 400s every request — StartRelayRouter guards
// against starting one.
func NewRelayRouter(addr string, managers []*ServerManager, registry *ProxyRegistry, virtual *VirtualLLMConfig) *RelayRouter {
	live := make([]*ServerManager, 0, len(managers))
	for _, m := range managers {
		if m != nil {
			live = append(live, m)
		}
	}
	p := &RelayRouter{managers: live, registry: registry, virtual: virtual, affinity: newVirtualAffinityStore(nil), metrics: NewProxyMetrics(nil)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", p.handleModels)
	mux.HandleFunc("GET /models", p.handleModels)
	mux.HandleFunc("POST /models/load", p.handleModelLoad)
	mux.HandleFunc("POST /models/unload", p.handleModelUnload)
	mux.HandleFunc("GET /health", p.handleHealth)
	mux.HandleFunc("POST /v1/audio/transcriptions", p.handleAudioTranscription)
	// Anthropic Messages API compatibility (relay_router_anthropic.go).
	// Handlers 404 at request time when p.anthropic is nil (feature off) —
	// registered unconditionally here since setAnthropic runs after
	// construction, same ordering as the reasoningEffort* setters.
	mux.HandleFunc("POST /v1/messages", p.handleAnthropicMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", p.handleAnthropicCountTokens)
	mux.HandleFunc("/api/", p.handleAnthropicPassthrough)
	mux.HandleFunc("/", p.handleProxy)

	p.server = &http.Server{
		Addr:    addr,
		Handler: mux,
		// ReadHeaderTimeout bounds how long a client may trickle in request
		// headers (the classic slow-loris shape) before the connection is
		// dropped. IdleTimeout bounds how long a keep-alive connection may
		// sit idle between requests. Neither touches an in-flight
		// request — a legitimate long generation is read/written well
		// after headers complete, so WriteTimeout/ReadTimeout stay unset on
		// purpose: this service's whole point is serving responses that can
		// run for minutes.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return p
}

// Listen binds every address in addrs on a best-effort basis (see
// listenAll's doc comment) and records whichever listeners actually bound
// for Serve. Must be called before Serve; StartRelayRouter is the one
// production caller and does so in that order. Returns an error only if
// NOT ONE address could be bound.
//
// p.server.Addr is overwritten with the first bound listener's real address
// so a port-0 caller (the whole test suite passes ":0") sees the port it
// actually got, not the literal string NewRelayRouter was constructed with.
func (p *RelayRouter) Listen(addrs []string) error {
	lns, err := listenAll(addrs, "relay router")
	if err != nil {
		return err
	}
	p.listeners = lns
	p.server.Addr = lns[0].Addr().String()
	return nil
}

// Serve starts one goroutine per listener bound by Listen, all serving the
// same *http.Server — so the same handler, same TLS config, same
// keep-alive/timeout settings on every bound interface. Each goroutine exits
// when its listener closes (Close, via p.server.Close, closes every listener
// the server is tracking at once) and logs only if that wasn't the expected
// shutdown signal.
func (p *RelayRouter) Serve() {
	scheme := "http"
	if p.tlsCert != "" {
		scheme = "https"
		// Pinned rather than left at the stdlib default, matching the main
		// mux's TCP front: a future toolchain lowering that default must not
		// silently loosen this listener.
		p.server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	for _, ln := range p.listeners {
		slog.Info("relay router listening", "addr", ln.Addr().String(), "scheme", scheme)
	}
	for _, ln := range p.listeners {
		go func() {
			var err error
			if p.tlsCert != "" {
				err = p.server.ServeTLS(ln, p.tlsCert, p.tlsKey)
			} else {
				err = p.server.Serve(ln)
			}
			if err != nil && err != http.ErrServerClosed {
				slog.Error("relay router error", "addr", ln.Addr().String(), "error", err)
			}
		}()
	}
}

func (p *RelayRouter) Close() error {
	return p.server.Close()
}

// Metrics exposes the router's proxy instrumentation to the main mux's
// GET /api/status/detailed handler. Both listeners live in the same
// process, so this is a direct in-process read of mutex/atomic-guarded
// state — the status handler never dials the router's own TCP port (which
// may be disabled, bound elsewhere, or TLS-only). Nil receiver matters:
// main.go's `relayRouter := StartRelayRouter(...)` is nil when
// --router-port is unset.
func (p *RelayRouter) Metrics() *ProxyMetrics {
	if p == nil {
		return nil
	}
	return p.metrics
}

// Addr returns the router's primary listener address (e.g.
// "127.0.0.1:8180") — the first of possibly several bound by Listen, or ""
// for a nil router. See Addrs for the full set.
func (p *RelayRouter) Addr() string {
	if p == nil {
		return ""
	}
	return p.server.Addr
}

// Addrs returns every address Listen actually bound, in bind order. Nil for
// a nil router or one Listen was never called on (e.g. a test that drives
// the handler directly without ever serving).
func (p *RelayRouter) Addrs() []string {
	if p == nil || len(p.listeners) == 0 {
		return nil
	}
	addrs := make([]string, len(p.listeners))
	for i, ln := range p.listeners {
		addrs[i] = ln.Addr().String()
	}
	return addrs
}

// TLSEnabled reports whether the router listener serves TLS.
func (p *RelayRouter) TLSEnabled() bool {
	if p == nil {
		return false
	}
	return p.tlsCert != ""
}

// AffinityPinCounts exposes virtualAffinityStore.pinCounts for the status
// dashboard — see that method for the shape.
func (p *RelayRouter) AffinityPinCounts() map[string]map[string]int {
	if p == nil {
		return nil
	}
	return p.affinity.pinCounts()
}

func (p *RelayRouter) handleModelLoad(w http.ResponseWriter, r *http.Request) {
	mgr, model, ok := p.managedModelFromBody(w, r)
	if !ok {
		return
	}
	if err := mgr.StartLoad(model); err != nil {
		writeRouterError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeRouterJSON(w, http.StatusOK, map[string]any{"success": true, "model": model})
}

// handleModelUnload stops a managed model. Unloading something that is not
// running is a no-op rather than an error: callers use this to reach a state,
// not to perform a transition.
func (p *RelayRouter) handleModelUnload(w http.ResponseWriter, r *http.Request) {
	mgr, model, ok := p.managedModelFromBody(w, r)
	if !ok {
		return
	}
	if err := mgr.StopInstance(model); err != nil {
		slog.Debug("relay router: unload of a model that was not running", "model", model, "error", err)
	}
	writeRouterJSON(w, http.StatusOK, map[string]any{"success": true, "model": model})
}

// managedModelFromBody decodes {"model": "..."} and resolves it to the manager
// that owns it, writing the error response itself when it cannot.
func (p *RelayRouter) managedModelFromBody(w http.ResponseWriter, r *http.Request) (*ServerManager, string, bool) {
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
		writeRouterError(w, http.StatusBadRequest, "missing or invalid model field")
		return nil, "", false
	}
	for _, mgr := range p.managers {
		if mgr.HasAlias(body.Model) {
			return mgr, body.Model, true
		}
	}

	writeRouterError(w, http.StatusBadRequest,
		fmt.Sprintf("model %q is not a managed server; only llama-server and mlx-serve models can be loaded or unloaded", body.Model))
	return nil, "", false
}

// resolvedVirtualTarget is one candidate the router will actually try for a
// virtual model. manager set means an alias target; otherwise it's an
// endpoint target (endpoint + upstreamID).

func writeRouterJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

// writeRouterError emits the {"error":{"message":...}} envelope that
// OpenAI-compatible and llama.cpp clients both parse for a human-readable
// reason.
func writeRouterError(w http.ResponseWriter, status int, msg string) {
	writeRouterJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg},
	})
}

func (p *RelayRouter) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// maxProxyBodyBytes caps the primary /v1/chat/completions-shaped route.
// handleAudioTranscription (maxTranscriptionBytes) and the Anthropic routes
// (maxAnthropicBodyBytes) already cap theirs; this one read the whole body
// with no ceiling at all. Sized the same as the Anthropic cap — this is the
// general-purpose chat route, so base64 image attachments and long tool
// results are the realistic upper end, not a special case.
var maxProxyBodyBytes int64 = 64 << 20

func (p *RelayRouter) handleProxy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyBodyBytes))
	r.Body.Close()
	if err != nil {
		// MaxBytesReader's error is the overflow case and deserves its own
		// status — everything else is a mundane read failure.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf(`{"error":"request body exceeds %d bytes"}`, maxProxyBodyBytes), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}

	var envelope struct {
		Model          string `json:"model"`
		PromptCacheKey string `json:"prompt_cache_key"`
		User           string `json:"user"`
		Stream         bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Model == "" {
		http.Error(w, `{"error":"missing or invalid model field"}`, http.StatusBadRequest)
		return
	}

	// An Anthropic-compat modelMap key (see relay_router_anthropic.go) is
	// resolved before every other dispatch check below, so a redirect
	// configured under router.anthropic.modelMap doubles as a router-wide
	// alias reachable from a plain OpenAI client too — not just
	// /v1/messages. Rewritten in place so managed/virtual/endpoint dispatch
	// below sees the target exactly as if the client had asked for it
	// directly. warnAnthropicModelMap (main.go) flags a key that collides
	// with an existing managed alias or virtual name at startup, since this
	// ordering means such a key would silently shadow it.
	if p.anthropic != nil {
		if target, ok := p.anthropic.modelMap[envelope.Model]; ok && target != "" {
			if rewritten, err := rewriteProxyBody(body, target, nil, nil); err == nil {
				body = rewritten
				envelope.Model = target
			}
		}
	}

	// Register this request with the proxy-metrics registry (status_metrics.go)
	// now that envelope.Model reflects any anthropic modelMap rewrite above —
	// the dashboard's `model` field should read what dispatch actually used.
	// begin's returned writer SHADOWS the outer `w` deliberately: every branch
	// below (routeManaged, routeVirtual, routeOpenAI, the unknown-model 400)
	// must write through the metered writer, and shadowing makes that
	// unmissable rather than relying on each call site remembering to use a
	// differently-named variable. Registering after the body has already been
	// read (above) means a slow/oversized upload never shows up as a tracked
	// connection — maxProxyBodyBytes already bounds it, and the diagnostic
	// question this dashboard answers is about upstream behavior, not client
	// uploads.
	conn, w := p.metrics.begin(w, r, envelope.Model, envelope.Stream, int64(len(body)))
	// defer, not a call at the end of the function: a mid-stream backend
	// failure panics with http.ErrAbortHandler (recovered by net/http one
	// frame up), and a bare end-of-function call would never run on that
	// path, leaking the connection into `active` forever — exactly the
	// "ghost entry" the ring-buffered recentRequests exists to never produce.
	defer p.metrics.end(conn)

	// Managed servers checked in priority order (llama first, then mlx).
	// First HasAlias match wins — llama wins on collision.
	for _, mgr := range p.managers {
		if mgr.HasAlias(envelope.Model) {
			p.routeManaged(w, r, mgr, envelope.Model, body, conn)
			return
		}
	}

	if p.virtual != nil {
		if virtual := p.virtual.Find(envelope.Model); virtual != nil {
			candidates := p.virtualCandidates(r.Context(), envelope.Model)
			if len(candidates) == 0 {
				// A configured virtual name is never "unknown" — that error
				// sends whoever's debugging after the wrong problem. Every
				// target here is misconfigured (bad endpoint/alias
				// reference), not missing; warnVirtualModelConfig already
				// flagged this at startup.
				writeRouterError(w, http.StatusServiceUnavailable,
					fmt.Sprintf("virtual model %q: no usable target configured", envelope.Model))
				return
			}
			// A pinned target (see virtualAffinityStore) outranks reachability ordering —
			// it goes to the front even if candidatesForVirtual currently
			// believes something else is more reachable. A pin naming a
			// target dropped from candidates (removed from config) is a
			// no-op inside applyAffinity, so routing falls back to the
			// normal order.
			affinityKey := affinityKeyFromBody(envelope.PromptCacheKey, envelope.User)
			candidates = applyAffinity(candidates, p.affinity.lookup(envelope.Model, affinityKey))
			p.routeVirtual(w, r, envelope.Model, candidates, body, affinityKey, conn)
			return
		}
	}

	if p.registry != nil {
		if ep, upstreamID, ok := p.registry.LookupModel(r.Context(), envelope.Model); ok {
			p.routeOpenAI(w, r, ep, upstreamID, body, conn)
			return
		}
	}

	conn.setTarget("unknown", "")
	slog.Warn("relay router: unknown model", "model", envelope.Model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("unknown model %q", envelope.Model)})
}

func (p *RelayRouter) routeManaged(w http.ResponseWriter, r *http.Request, mgr *ServerManager, alias string, body []byte, conn *ProxyConn) {
	// The lease is held for the whole proxied exchange, including the SSE
	// stream, so the budget cannot evict this instance mid-response.
	endpoint, release, err := mgr.Acquire(r.Context(), alias)
	if err != nil {
		slog.Warn("relay router: failed to launch managed server", "kind", mgr.profile.Kind, "model", alias, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	defer release()
	// Set after Acquire returns, not before: a request queued on admission
	// has not chosen an instance yet, and the dashboard's target should mean
	// "is being served by", not "wants".
	conn.setTarget("managed", mgr.profile.Kind+":"+alias)

	// No model swap needed here — the client already sent the bare alias the
	// managed server expects — but the reasoning_effort rewrite still applies
	// (see RouterConfig): a client hitting a managed alias has exactly the
	// same backend-vocabulary problem as one hitting an endpoint.
	rewritten, err := rewriteProxyBody(body, "", p.reasoningEffortMap, p.reasoningEffortTemplateKwargs)
	if err != nil {
		slog.Warn("relay router: body rewrite failed", "alias", alias, "error", err)
		writeRouterError(w, http.StatusBadRequest, "failed to rewrite request body")
		return
	}

	target, err := url.Parse(endpoint.BaseURL)
	if err != nil {
		// endpoint.BaseURL is normally built internally (e.g.
		// "http://127.0.0.1:<port>/v1") and always valid, but this error must
		// still be handled here rather than ignored: newUpstreamProxy's
		// Director dereferences target.Scheme unconditionally, so passing a
		// nil target would panic inside the handler instead of failing the
		// request cleanly.
		slog.Warn("relay router: bad managed server endpoint", "kind", mgr.profile.Kind, "alias", alias, "error", err)
		writeRouterError(w, http.StatusBadGateway, fmt.Sprintf("invalid managed server endpoint: %v", err))
		return
	}
	newUpstreamProxy(target, rewritten, endpoint.APIKey, mgr.profile.Kind, alias, nil).ServeHTTP(w, r)
}

// routeOpenAI rewrites the body's `model` to the bare upstream id (so OMLX
// et al. see their own name, not "omlx/X") and forwards to the endpoint.
func (p *RelayRouter) routeOpenAI(w http.ResponseWriter, r *http.Request, ep OpenAIEndpoint, upstreamID string, body []byte, conn *ProxyConn) {
	conn.setTarget("endpoint", ep.Name+"/"+upstreamID)
	rewritten, err := rewriteProxyBody(body, upstreamID, p.reasoningEffortMap, p.reasoningEffortTemplateKwargs)
	if err != nil {
		slog.Warn("relay router: body rewrite failed", "endpoint", ep.Name, "error", err)
		http.Error(w, `{"error":"failed to rewrite model field"}`, http.StatusBadRequest)
		return
	}
	target, err := url.Parse(ep.BaseURL)
	if err != nil {
		slog.Warn("relay router: bad endpoint baseURL", "endpoint", ep.Name, "baseURL", ep.BaseURL, "error", err)
		http.Error(w, `{"error":"invalid endpoint configuration"}`, http.StatusInternalServerError)
		return
	}
	proxy := newUpstreamProxy(target, rewritten, ep.APIKey, "openai", ep.Name, nil)
	proxy.Transport = ep.Transport()
	proxy.ServeHTTP(w, r)
}

// newUpstreamProxy builds the reverse proxy shared by every branch. The
// Director replaces (or clears) Authorization so the inbound bearer token —
// which is relayLLM's internal token, meaningless to upstreams — never
// leaks across the trust boundary.
//
// onError, when non-nil, is consulted before the default 502 is written on a
// backend failure. httputil.ReverseProxy only invokes ErrorHandler on a
// RoundTrip failure, which always happens before any response byte is
// written — a body-copy failure after headers are sent is not routed through
// here. Returning true means the caller is handling the failure itself (the
// virtual-model retry path: a failed attempt must emit nothing so the next
// candidate gets a clean response to write into); false or nil falls through
// to the normal 502 body.
func newUpstreamProxy(target *url.URL, body []byte, apiKey, branch, label string, onError func(error) bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			if apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+apiKey)
			} else {
				req.Header.Del("Authorization")
			}
		},
		FlushInterval: -1, // flush immediately for SSE streaming
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("relay router: backend error", "branch", branch, "target", label, "error", err)
			if onError != nil && onError(err) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("backend error: %v", err)})
		},
	}
}

// StartRelayRouter binds every address in addrs it can (best-effort — see
// listenAll) and starts serving each bound one in its own background
// goroutine (see Listen/Serve). Returns (nil, nil) — a clean no-op, not an
// error — when addrs is empty or no live backend remains after dropping nil
// managers. Returns a non-nil error only when NOT ONE address in addrs could
// be bound, which is fatal: the caller is expected to log it and exit rather
// than run with the router silently absent, matching the main HTTP front's
// startMainTCPListener. A partial failure (some addresses bound, at least
// one didn't) is not an error here at all — listenAll already logged the
// failure and Serve simply runs on whichever listeners exist.
//
// router (may be nil) carries RouterConfig-level behavior — the
// reasoning_effort rewrite map and its sibling chat_template_kwargs merge
// table — and both are applied via their setters before any of Serve's
// per-listener goroutines are spawned, not after StartRelayRouter returns.
// That ordering is load-bearing, not stylistic: Go's memory model guarantees
// a goroutine's creation happens-before its execution, so setting the fields
// first means every connection-handling goroutine transitively spawned from
// Serve is guaranteed to observe them without synchronization. Setting them
// after a listener is already serving would let an accepted request read
// the field concurrently with the write — a real, race-detector-visible
// data race under live traffic. StartRelayRouter is the one production call
// site for this, chosen over adding the parameters to NewRelayRouter
// because it has far fewer call sites to touch.
//
// tlsCert/tlsKey are the router listener's own cert/key pair (empty strings
// mean plain http); main validates they're either both set or both empty
// before calling in, so this never has to fail startup on a mismatched pair
// itself. Applied via setTLS under the same pre-serve ordering rule as the
// reasoningEffort* fields. One cert/key pair covers every bound address —
// there's no per-bind TLS config — so the cert must be valid for all of
// them if more than one is configured.
func StartRelayRouter(addrs []string, managers []*ServerManager, registry *ProxyRegistry, virtual *VirtualLLMConfig, router *RouterConfig, tlsCert, tlsKey string) (*RelayRouter, error) {
	if len(addrs) == 0 {
		return nil, nil
	}
	p := NewRelayRouter(addrs[0], managers, registry, virtual)
	// A router with no managed servers and no OpenAI endpoints would
	// otherwise dispatch nothing — except router.anthropic's passthrough is
	// a real destination in its own right (api.anthropic.com), needing
	// neither. Without this check, a deployment using relayLLM purely as a
	// Claude Code proxy (router.anthropic configured, nothing else) got no
	// router at all: /v1/messages, the /api/* bootstrap passthrough,
	// everything 404'd with no indication why.
	hasAnthropic := router != nil && router.Anthropic != nil
	if len(p.managers) == 0 && p.registry == nil && !hasAnthropic {
		return nil, nil
	}
	if router != nil {
		p.setReasoningEffortMap(router.ReasoningEffortMap)
		p.setReasoningEffortTemplateKwargs(router.ReasoningEffortTemplateKwargs)
		p.setAnthropic(router.Anthropic)
	}
	p.setTLS(tlsCert, tlsKey)
	if err := p.Listen(addrs); err != nil {
		return nil, err
	}
	p.Serve()
	return p, nil
}
