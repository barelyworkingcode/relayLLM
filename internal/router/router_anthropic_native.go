package router

// Native Anthropic pass-through for modelMap targets that speak the Anthropic
// Messages API themselves (settings.json `api: ["anthropic"]`, see
// config.SpeaksAnthropic). Instead of translating to OpenAI chat and back
// (router_anthropic_translate.go), the request goes to {baseURL}/messages with
// only the top-level "model" rewritten (anthropic_body.go), and the response
// streams back untouched. That keeps the request prefix byte-stable, so a
// backend's prompt cache survives across a Claude Code conversation's turns.
//
// A target that has not declared the dialect never gets here: the resolvers
// return false and handleAnthropicRedirect translates exactly as before.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"relayllm/internal/config"
)

const maxNativeOverflowBody = 1 << 20

// nativeResolution is how a modelMap target resolves for the native path:
// either one managed alias / endpoint model (single), or a virtual model's
// ordered candidates.
type nativeResolution struct {
	single      *ResolvedVirtualTarget
	virtualName string
	candidates  []ResolvedVirtualTarget
}

func (t ResolvedVirtualTarget) speaksAnthropic() bool {
	if t.manager != nil {
		return t.manager.SpeaksAnthropic()
	}
	return t.endpoint.SpeaksAnthropic()
}

// nativeUpstreamModel is the model id the backend knows the target by.
func (t ResolvedVirtualTarget) nativeUpstreamModel() string {
	if t.manager != nil {
		return t.alias
	}
	return t.upstreamID
}

// resolveAnthropicNative resolves target in handleProxy's order (managed
// alias, virtual, endpoint) and reports whether the native path applies.
//
// A virtual is native only when EVERY candidate declares the dialect. A
// native-then-translated failover would replay one conversation's history
// through two different encodings mid-stream, which is the wedge conversation
// affinity exists to prevent; all-or-nothing keeps each conversation on one
// dialect.
func (p *RelayRouter) resolveAnthropicNative(ctx context.Context, target string) (nativeResolution, bool) {
	for _, mgr := range p.managers {
		if mgr.HasAlias(target) {
			if !mgr.SpeaksAnthropic() {
				return nativeResolution{}, false
			}
			t := ResolvedVirtualTarget{manager: mgr, alias: target}
			return nativeResolution{single: &t}, true
		}
	}
	if p.virtual != nil {
		if p.virtual.Find(target) != nil {
			candidates := p.virtualCandidates(ctx, target)
			if len(candidates) == 0 {
				return nativeResolution{}, false
			}
			for _, c := range candidates {
				if !c.speaksAnthropic() {
					return nativeResolution{}, false
				}
			}
			return nativeResolution{virtualName: target, candidates: candidates}, true
		}
	}
	if p.registry != nil {
		if ep, upstreamID, ok := p.registry.LookupModel(ctx, target); ok {
			if !ep.SpeaksAnthropic() {
				return nativeResolution{}, false
			}
			t := ResolvedVirtualTarget{endpoint: ep, upstreamID: upstreamID}
			return nativeResolution{single: &t}, true
		}
	}
	return nativeResolution{}, false
}

// serveAnthropicNative serves a modelMap redirect natively and reports true,
// or reports false having written nothing so the caller translates.
func (p *RelayRouter) serveAnthropicNative(w http.ResponseWriter, r *http.Request, body []byte, requestedModel, target string) bool {
	res, ok := p.resolveAnthropicNative(r.Context(), target)
	if !ok {
		return false
	}
	// Validate once up front: a body the splice rejects would otherwise fail
	// identically on every virtual candidate and surface as a misleading 503.
	if _, err := mutateAnthropicBody(body, anthropicMutation{Model: target}); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return true
	}

	var envelope struct {
		Metadata *anthropicMetadata `json:"metadata"`
		Stream   bool               `json:"stream"`
	}
	_ = json.Unmarshal(body, &envelope)

	// One ProxyConn for the exchange; the translated path gets its own from
	// the re-entrant handleProxy instead, so exactly one exists either way.
	conn, mw := p.metrics.begin(w, r, target, envelope.Stream, int64(len(body)))
	defer p.metrics.end(conn)
	opts := nativeOpts{tap: true, rewriteOverflow: true}

	if res.single != nil {
		t := *res.single
		wrote, _, err := p.attemptNative(mw, r, t, body, conn, opts, nil, false, true)
		if err != nil && !wrote {
			m := mapAnthropicBackendError(http.StatusBadGateway, []byte(err.Error()))
			writeAnthropicError(mw, m.status, m.errType, m.message)
		}
		return true
	}

	affinityKey := anthropicAffinityKey(envelope.Metadata)
	candidates := applyAffinity(res.candidates, p.affinity.lookup(res.virtualName, affinityKey))
	p.routeVirtualWith(mw, r, res.virtualName, candidates, affinityKey, conn,
		func(w http.ResponseWriter, status int, msg string) {
			writeAnthropicError(w, status, "overloaded_error", msg)
		},
		func(t ResolvedVirtualTarget) (bool, int, error) {
			var backendErr error
			wrote, status, err := p.attemptNative(mw, r, t, body, conn, opts, func(e error) bool {
				backendErr = e
				return true
			}, true, false)
			if err == nil {
				err = backendErr
			}
			return wrote, status, err
		})
	return true
}

type nativeOpts struct {
	tap             bool // count usage from the response
	rewriteOverflow bool // reword a context-overflow 400 for Claude Code's auto-compact
}

// attemptNative runs one native attempt against w, tracking whether anything
// reached the client (see virtualResponseRecorder). onError, when set,
// suppresses the proxy's own error response so a virtual candidate can fail
// over cleanly. direct is a single (non-virtual) target: it labels the conn
// and sets X-Relay-Model-Target itself, which routeVirtualWith does per
// candidate otherwise. The lease, if any, is held until the exchange ends.
func (p *RelayRouter) attemptNative(w http.ResponseWriter, r *http.Request, t ResolvedVirtualTarget, body []byte, conn *ProxyConn, o nativeOpts, onError func(error) bool, virtual, direct bool) (wrote bool, status int, err error) {
	proxy, release, err := p.buildNativeProxy(r.Context(), t, body, conn, o, onError, virtual)
	if err != nil {
		return false, 0, err
	}
	defer release()
	if direct {
		if t.manager != nil {
			conn.setTarget("managed", t.manager.Profile().Kind+":"+t.alias)
		} else {
			conn.setTarget("endpoint", t.endpoint.Name+"/"+t.upstreamID)
		}
		setModelTargetHeader(w, t.TargetHeaderValue())
	}
	rec := &virtualResponseRecorder{ResponseWriter: w}
	proxy.ServeHTTP(rec, r)
	return rec.wrote, rec.statusCode, nil
}

// buildNativeProxy wraps newUpstreamProxy (which already drops the client's
// Authorization / X-Api-Key / X-Relay-* and strips upstream X-Relay-* from the
// response) rather than editing it: this adds the X-Api-Key form a native
// Anthropic server may expect, Anthropic-shaped errors, and the response taps.
func (p *RelayRouter) buildNativeProxy(ctx context.Context, t ResolvedVirtualTarget, body []byte, conn *ProxyConn, o nativeOpts, onError func(error) bool, virtual bool) (*httputil.ReverseProxy, func(), error) {
	release := func() {}
	var baseURL, apiKey, branch, label string
	var transport http.RoundTripper
	if t.manager != nil {
		endpoint, rel, err := t.manager.Acquire(ctx, t.alias)
		if err != nil {
			slog.Warn("relay router: failed to launch managed server", "kind", t.manager.Profile().Kind, "model", t.alias, "error", err)
			return nil, nil, err
		}
		release = rel
		baseURL, apiKey = endpoint.BaseURL, endpoint.APIKey
		branch, label = t.manager.Profile().Kind, t.alias
		if virtual {
			transport = config.VirtualDialTransport
		} else {
			transport = p.anthropic.transport
		}
	} else {
		baseURL, apiKey = t.endpoint.BaseURL, t.endpoint.APIKey
		branch, label = "anthropic-native", t.endpoint.Name
		if virtual {
			transport = t.endpoint.VirtualTransport()
		} else {
			transport = t.endpoint.Transport()
		}
	}

	mutated, err := mutateAnthropicBody(body, anthropicMutation{Model: t.nativeUpstreamModel()})
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("rewrite request body: %w", err)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("invalid backend endpoint: %w", err)
	}

	proxy := newUpstreamProxy(u, mutated, apiKey, branch, label, onError)
	proxy.Transport = transport

	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		if apiKey != "" {
			req.Header.Set("X-Api-Key", apiKey)
		}
	}

	modify := proxy.ModifyResponse
	proxy.ModifyResponse = func(resp *http.Response) error {
		if err := modify(resp); err != nil {
			return err
		}
		if resp.Header.Get("Content-Encoding") != "" {
			return nil // can't read a compressed body; pass it through untouched
		}
		switch {
		case o.rewriteOverflow && resp.StatusCode == http.StatusBadRequest:
			rewriteNativeOverflow(resp)
		case o.tap && resp.StatusCode == http.StatusOK:
			mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
			switch {
			case mt == "text/event-stream":
				resp.Body = newAnthropicUsageTap(resp.Body, true, conn.noteUsage)
			case strings.Contains(mt, "json"):
				resp.Body = newAnthropicUsageTap(resp.Body, false, conn.noteUsage)
			}
		}
		return nil
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("relay router: backend error", "branch", branch, "target", label, "error", err)
		if onError != nil && onError(err) {
			return
		}
		m := mapAnthropicBackendError(http.StatusBadGateway, []byte(fmt.Sprintf("backend error: %v", err)))
		writeAnthropicError(w, m.status, m.errType, m.message)
	}
	return proxy, release, nil
}

// rewriteNativeOverflow rewords a backend's context-overflow 400 to
// "prompt is too long: <original>". Claude Code's auto-compact keys on that
// wording, not on the status, and local engines each phrase overflow their
// own way; without this a native target would dead-end a long session where
// the translated path compacts. Every other response, error or not, is left
// exactly as the backend sent it, and so is a message already worded that way
// (a real Anthropic-style backend). Bodies over 1 MiB are not error messages
// and pass through unread.
func rewriteNativeOverflow(resp *http.Response) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxNativeOverflowBody+1))
	restore := func() {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(data), resp.Body), resp.Body}
	}
	if err != nil || len(data) > maxNativeOverflowBody {
		restore()
		return
	}
	msg := extractBackendErrorMessage(data)
	if !isContextOverflowMessage(msg) || strings.HasPrefix(strings.ToLower(msg), "prompt is too long") {
		restore()
		return
	}
	resp.Body.Close()
	out, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "invalid_request_error", "message": "prompt is too long: " + msg},
	})
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Set("Content-Type", "application/json")
}

// ---------------------------------------------------------------------------
// count_tokens
// ---------------------------------------------------------------------------

// serveAnthropicCountTokensNative forwards count_tokens to a native backend's
// {baseURL}/messages/count_tokens (managed alias or endpoint only; a virtual
// keeps the estimate). It reports false, having written nothing, whenever the
// caller should answer with the byte estimate instead: not native, nothing
// reachable, or the backend has no such route (404/405), which is common
// among local engines.
func (p *RelayRouter) serveAnthropicCountTokensNative(w http.ResponseWriter, r *http.Request, body []byte, target string) bool {
	res, ok := p.resolveAnthropicNative(r.Context(), target)
	if !ok || res.single == nil {
		return false
	}
	t := *res.single
	var backendErr error
	proxy, release, err := p.buildNativeProxy(r.Context(), t, body, nil, nativeOpts{}, func(e error) bool {
		backendErr = e
		return true
	}, false)
	if err != nil {
		return false
	}
	defer release()

	probe := &countTokensProbe{real: w, target: t.TargetHeaderValue()}
	proxy.ServeHTTP(probe, r)
	return backendErr == nil && !probe.fallback
}

// countTokensProbe holds a count_tokens response back until its status is
// known: 404/405 is swallowed (the caller falls back to the estimate), any
// other status is relayed verbatim.
type countTokensProbe struct {
	real     http.ResponseWriter
	target   string
	hdr      http.Header
	decided  bool
	fallback bool
}

func (c *countTokensProbe) Header() http.Header {
	if c.hdr == nil {
		c.hdr = make(http.Header)
	}
	return c.hdr
}

func (c *countTokensProbe) WriteHeader(status int) {
	if c.decided {
		return
	}
	c.decided = true
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		c.fallback = true
		return
	}
	for k, v := range c.hdr {
		c.real.Header()[k] = v
	}
	setModelTargetHeader(c.real, c.target)
	c.real.WriteHeader(status)
}

func (c *countTokensProbe) Write(b []byte) (int, error) {
	if !c.decided {
		c.WriteHeader(http.StatusOK)
	}
	if c.fallback {
		return len(b), nil
	}
	return c.real.Write(b)
}

func (c *countTokensProbe) Flush() {
	if c.decided && !c.fallback {
		if f, ok := c.real.(http.Flusher); ok {
			f.Flush()
		}
	}
}
