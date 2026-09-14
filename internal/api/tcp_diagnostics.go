package api

import (
	"net/http"
	"path"
	"strings"
)

// tcpDiagnosticsAllowlist is the entire read-only diagnostics surface the
// anonymous TCP front (--http-port) may reach. It is the one place that
// decides TCP reachability — see TCPDiagnosticsOnly's doc comment for why
// entries must be exact method+path pairs, never a prefix or pattern.
//
// Derived from what the dashboard actually does: internal/api/status/
// status.js's only network call is GET /api/status/detailed; status.html
// links exactly two static assets (status.css, status.js); GET /api/status
// is the relay Service Inspector's read of the same runtime counters,
// equally safe to expose (api_status.go's RegisterStatusRoutes carries no
// session or terminal content — TerminalSummary has no directory field).
// The dashboard has no action buttons — nothing in its JS ever issues a
// non-GET request — so no mutating route, no /ws upgrade, no llama/mlx
// instance listing (the dashboard reads instances via /api/status/detailed,
// never GET /api/{llama,mlx}/instances directly), and no terminal/session
// route (GET /api/terminals/{id}/log included) earns a place here. Any of
// those now requires the bearer-authenticated Unix socket.
var tcpDiagnosticsAllowlist = map[string]bool{
	"GET /status":              true,
	"GET /status/status.css":   true,
	"GET /status/status.js":    true,
	"GET /api/status":          true,
	"GET /api/status/detailed": true,
}

// TCPDiagnosticsOnly wraps the same handler value the Unix socket serves so
// the anonymous TCP front (--http-port) only ever reaches the routes in
// tcpDiagnosticsAllowlist. Everything else — every mutating route, /ws,
// terminal/session content, instance listings — answers 404, identical to
// an unregistered path, so a probe against the TCP port can't distinguish
// "exists but blocked" from "never existed". Applied only at the TCP
// listener's call site (see app.go's startMainTCPListener call); the Unix
// socket's own handler is untouched and keeps serving the full route table
// behind bearerAuth.
//
// The match is exact method + exact r.URL.Path, checked only after
// isCleanDiagnosticsPath confirms the path is already in the one canonical
// form the allowlist's keys are written in. This is deliberate: normalizing
// a path before matching (collapsing "..", doubled slashes, a
// percent-encoded "/") is exactly the mechanism a traversal probe relies on
// to make an unclean path resolve to an allowed one, so an unclean path is
// rejected outright rather than cleaned and retried.
func TCPDiagnosticsOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isCleanDiagnosticsPath(r) || !tcpDiagnosticsAllowlist[r.Method+" "+r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isCleanDiagnosticsPath reports whether r.URL.Path is already in the exact
// canonical form tcpDiagnosticsAllowlist's keys are written in — no "..",
// no doubled slash, no trailing slash the allowlist doesn't itself have.
// r.URL.Path is already percent-decoded by net/http, so a request that hid
// a literal "/" as "%2F" would otherwise decode into a Path that passes the
// Clean check while never having been the literal path the client's HTTP
// request line named; checking RawPath (only populated when the escaped
// form differs from Path) catches that case before the decoded form is
// trusted.
func isCleanDiagnosticsPath(r *http.Request) bool {
	p := r.URL.Path
	if p == "" || p[0] != '/' {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	if strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		return false
	}
	return true
}
