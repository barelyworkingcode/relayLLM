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
// including /health, requires a valid router key. Passthrough routes
// (router.passthrough, router_passthrough.go — forwards Authorization/
// x-api-key byte-for-byte to a real third-party upstream on purpose) must
// not have those same headers repurposed as the local router-key credential,
// since that would send relayLLM's own local secret to that upstream; they
// authenticate with their own header, X-Relay-Router-Key, instead.
func routerKeyAuthMiddleware(store *RouterKeyStore, passthroughNames []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var presented string
			if isPassthroughPath(r.URL.Path, passthroughNames) {
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

// isPassthroughPath reports whether path is served by one of the /<name>/
// routes setPassthrough actually mounted — the same set SocketHandler reads
// to refuse passthrough paths outright on router.sock (C9).
func isPassthroughPath(path string, passthroughNames []string) bool {
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
