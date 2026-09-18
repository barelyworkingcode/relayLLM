package router

// Credential passthrough routes for the relay-router (settings.json's
// router.passthrough). Each entry mounts /<name>/ and forwards everything
// under it byte-for-byte to a fixed upstream, including the client's own
// credential. relayLLM never holds that credential, which is the same model
// as router.anthropic's passthrough.
//
// Built for OpenAI, which has two upstreams. Oh My Pi's openai-codex provider
// and the Codex CLI both sign in with a ChatGPT subscription OAuth token
// against https://chatgpt.com/backend-api. API-key clients talk to
// https://api.openai.com.
//
// Routing is by path prefix, not by the body's model field. OpenAI's routes
// (/v1/chat/completions, /v1/responses, /v1/models) are the same paths the
// router serves for local models. Sharing that namespace would send a
// mistyped local model name, prompt and all, to a cloud provider. A prefix
// also carries requests with no model field: GETs, /responses/{id}, and
// WebSocket upgrades. OMP's codex transport prefers WebSocket
// (wss://…/codex/responses) and falls back to SSE.
//
// There is no modelMap. OMP and Codex already reach local models through
// their own provider config. A redirect would need a Responses API to Chat
// Completions translator plus WebSocket framing, and router.anthropic's
// translator covers neither.

import (
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"relayllm/internal/config"
	"relayllm/internal/netutil"
)

// passthroughReservedNames are first path segments the router's own mux
// already routes. A passthrough mounted there would be shadowed, and a
// duplicate "/api/" pattern would panic ServeMux.
var passthroughReservedNames = map[string]bool{"v1": true, "api": true, "models": true, "health": true}

var passthroughNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// setPassthrough mounts one /<name>/ route per valid entry. It has the same
// pre-serve constraint as setReasoningEffortMap: StartRelayRouter calls it
// before Serve. An invalid entry is logged and skipped rather than failing
// startup, the same fail-safe convention setAnthropic follows.
func (p *RelayRouter) setPassthrough(cfg map[string]config.PassthroughConfig) {
	for _, name := range slices.Sorted(maps.Keys(cfg)) {
		proxy, upstream, err := newPassthroughProxy(name, cfg[name])
		if err != nil {
			slog.Error("relay router: router.passthrough entry disabled", "name", name, "error", err)
			continue
		}
		// Recorded here, not derived from cfg directly, so the router-key
		// middleware (auth.go) covers exactly the set of names that actually
		// mounted — an invalid entry never got a route, so it falls to
		// handleProxy like any other path. router.sock serves p.mux itself
		// (router_socket.go), so it gains this route with no registration of
		// its own.
		p.passthroughNames = append(p.passthroughNames, name)
		p.mux.HandleFunc("/"+name+"/", func(w http.ResponseWriter, r *http.Request) {
			// The body is never read on this path, so the connection is
			// labeled with no model, like handleAnthropicPassthrough.
			conn, mw := p.metrics.begin(w, r, "", false, r.ContentLength)
			conn.setTarget("passthrough", name)
			defer p.metrics.end(conn)
			proxy.ServeHTTP(mw, r)
		})
		slog.Info("relay router: passthrough mounted", "path", "/"+name+"/", "upstream", upstream.String())
	}
}

// newPassthroughProxy builds the reverse proxy for one entry. It is
// deliberately not newUpstreamProxy, which replaces Authorization. Here the
// client's credential must reach the upstream untouched. Rewrite (not
// Director) keeps X-Forwarded-* off the outbound request, so the hop stays
// invisible. ReverseProxy carries WebSocket upgrades on its own; see
// meteredResponseWriter.Hijack for how they are counted.
func newPassthroughProxy(name string, cfg config.PassthroughConfig) (*httputil.ReverseProxy, *url.URL, error) {
	if !passthroughNamePattern.MatchString(name) {
		return nil, nil, fmt.Errorf("name must be one path segment of letters, digits, '-' or '_'")
	}
	if passthroughReservedNames[name] {
		return nil, nil, fmt.Errorf("/%s/ is already routed by the router itself", name)
	}
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil || upstream.Host == "" || (upstream.Scheme != "https" && upstream.Scheme != "http") {
		return nil, nil, fmt.Errorf("upstream %q must be an absolute http(s) URL", cfg.Upstream)
	}
	// The client's real credential rides every request. Sending it in
	// plaintext off the box would undo the TLS the client used before it was
	// pointed here.
	if upstream.Scheme == "http" && !netutil.IsLoopbackHost(upstream.Hostname()) {
		return nil, nil, fmt.Errorf("upstream %q must use https (only a loopback upstream may be plain http)", cfg.Upstream)
	}

	// DisableCompression keeps Accept-Encoding as the client sent it. Go's
	// default transport would otherwise add gzip and decompress the reply,
	// which breaks the byte-for-byte promise.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true

	prefix := "/" + name
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.RawPath = strings.TrimPrefix(pr.In.URL.RawPath, prefix)
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			// C10: X-Relay-Router-Key authenticates the caller to relayLLM
			// ITSELF on this route (auth.go) — Authorization/X-Api-Key are
			// what's forwarded byte-for-byte, on purpose, to the real
			// upstream above, so relayLLM's own local secret needs a header
			// name the upstream has no other reason to see and must never
			// receive.
			pr.Out.Header.Del("X-Relay-Router-Key")
		},
		Transport:     transport,
		FlushInterval: -1, // flush immediately for SSE streaming
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("relay router: passthrough backend error", "name", name, "error", err)
			writeRouterError(w, http.StatusBadGateway, fmt.Sprintf("passthrough %q: backend error: %v", name, err))
		},
	}
	return proxy, upstream, nil
}
