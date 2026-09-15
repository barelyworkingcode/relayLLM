package router

// Request-time router-key auth (C10), standalone TCP router only. This is a
// completely separate mechanism from router.sock's kernel-peer-token
// admission (C9, router_socket.go): a launched relayLLM never wires this in
// — see RelayRouter.EnableStandaloneRouterKeys's doc comment — so the two
// never interact and a bug in one cannot silently widen the other.

import (
	"net/http"
	"strings"
)

// routerKeyUnauthorizedBody matches router.sock's own refusal shape
// (admitRelayOnly in router_socket.go) so a client sees the same error
// envelope regardless of which of relayLLM's two auth mechanisms rejected
// it.
const routerKeyUnauthorizedBody = `{"error":{"message":"unauthorized","type":"authentication_error"}}`

// EnableStandaloneRouterKeys wraps the TCP router's handler with C10's
// router-key authentication. Standalone mode ONLY: a launched relayLLM's
// authentication story is entirely router.sock's kernel-peer-token
// admission, wired on a completely separate *http.Server this method never
// touches (ListenSocket's socketSrv, not p.server) — app.go must never call
// this when relay.Launched() is true.
//
// Must be called after every setPassthrough call (BuildRelayRouter already
// does this before returning) so p.passthroughNames is complete, and before
// Listen/Serve — the same pre-serve ordering constraint every other setX in
// router.go documents, since this replaces p.server.Handler outright rather
// than mutating something Serve already captured a reference to.
func (p *RelayRouter) EnableStandaloneRouterKeys(routerKeysPath string) {
	store := NewRouterKeyStore(routerKeysPath)
	p.server.Handler = routerKeyAuthMiddleware(store, p.passthroughNames)(p.mux)
}

// routerKeyAuthMiddleware is C10's gate in front of next: every route,
// including /health, requires a valid router key. Passthrough-shaped routes
// — router.passthrough's /<name>/ mounts (router_passthrough.go), and the
// three Anthropic routes that can forward a caller's own Authorization
// byte-for-byte to api.anthropic.com when the request isn't a modelMap hit
// (/api/, POST /v1/messages, POST /v1/messages/count_tokens —
// router_anthropic.go's newAnthropicPassthroughProxy) — must not have those
// same headers repurposed as the local router-key credential, since that
// would send relayLLM's own local secret to that upstream. A security
// review caught exactly this: /v1/messages can't be told apart from a
// modelMap-routed call (safe; dispatches through newUpstreamProxy, which
// already strips Authorization/X-Api-Key/X-Relay-*) until the handler reads
// the request body, so it and the other two get the X-Relay-Router-Key
// carve-out unconditionally rather than only when router.anthropic happens
// to be configured — the routes 404 without it either way, and requiring
// the same header on both outcomes keeps this middleware's decision
// independent of the handler's own routing logic.
func routerKeyAuthMiddleware(store *RouterKeyStore, passthroughNames []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var presented string
			if requiresRouterKeyHeader(r.URL.Path, passthroughNames) {
				presented = r.Header.Get("X-Relay-Router-Key")
			} else {
				presented = routerKeyCredentialFromHeaders(r.Header)
			}
			if _, ok := store.Authenticate(presented); !ok {
				writeRouterKeyUnauthorized(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// anthropicPassthroughPaths are the exact route patterns router.go registers
// for router_anthropic.go's handlers — mirrored here rather than imported
// from a shared slice because they're mux patterns (some exact, one prefix),
// not names, and this is the only other place that needs to enumerate them.
var anthropicPassthroughPaths = []string{"/v1/messages", "/v1/messages/count_tokens"}

// requiresRouterKeyHeader reports whether path must present the local
// router key via X-Relay-Router-Key rather than Authorization/X-Api-Key —
// every route that can forward those headers to a real third-party upstream
// (see routerKeyAuthMiddleware's doc comment for why /v1/messages* is in
// this set unconditionally).
func requiresRouterKeyHeader(path string, passthroughNames []string) bool {
	if strings.HasPrefix(path, "/api/") {
		return true
	}
	for _, p := range anthropicPassthroughPaths {
		if path == p {
			return true
		}
	}
	for _, name := range passthroughNames {
		if strings.HasPrefix(path, "/"+name+"/") {
			return true
		}
	}
	return false
}

// routerKeyCredentialFromHeaders reads the router key off Authorization:
// Bearer or x-api-key — either header name is accepted (C10); this repo's
// existing convention elsewhere (see router_passthrough.go's own upstream
// Authorization handling) is also case-insensitive canonical header lookup,
// which http.Header.Get already does.
func routerKeyCredentialFromHeaders(h http.Header) string {
	if v := h.Get("Authorization"); v != "" {
		if rest, ok := strings.CutPrefix(v, "Bearer "); ok {
			return rest
		}
	}
	return h.Get("X-Api-Key")
}

func writeRouterKeyUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(routerKeyUnauthorizedBody))
}
