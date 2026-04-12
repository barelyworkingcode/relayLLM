package main

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// bearerAuth wraps an http.Handler with bearer-token authentication.
//
// If `token` is empty, the middleware is a no-op pass-through. This is the
// dev-mode default — relayLLM running on loopback without orchestrator
// credentials. Operators get a loud startup warning in main.go in that case.
//
// When a token is configured, every request — including the /ws WebSocket
// upgrade — must carry `Authorization: Bearer <token>` or it is rejected
// with HTTP 401 before any handler runs (so the WS upgrade never allocates
// a session for an unauthenticated client).
//
// Token comparison uses crypto/subtle.ConstantTimeCompare to prevent timing
// attacks. The full design lives in eve/plans/cozy-honking-toast.md Section B.
func bearerAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			slog.Debug("rejecting request: missing bearer header",
				"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := []byte(strings.TrimSpace(header[len(prefix):]))
		// Compare lengths via subtle to keep the comparison constant-time
		// regardless of input length. ConstantTimeCompare returns 0 on
		// mismatch (including length mismatch).
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			slog.Warn("rejecting request: bad bearer token",
				"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
