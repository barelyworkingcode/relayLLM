package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
)

// generateBearerToken returns a 64-char (32-byte) random hex token. Used
// when relayLLM auto-provisions its own listener auth (standalone, no
// flag/env override). Same shape as relay's own service tokens.
func GenerateBearerToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// bearerAuth wraps an http.Handler with bearer-token authentication. It
// guards the Unix socket only — main.go's --http-port TCP front is served
// anonymously (see startMainTCPListener's call site), protected by
// --http-bind rather than a token, deliberately matching --router-port's
// existing unauthenticated-by-bind-address posture. There is therefore no
// browser-facing credential path here anymore (no cookie, no ?token=
// bootstrap) — every caller of the socket is a machine client (relay's
// dispatcher, the permission hook, a direct socket client) that can just
// send a header.
//
// If `token` is empty, the middleware is a no-op pass-through. This is the
// dev-mode default — relayLLM running without orchestrator credentials.
//
// When a token is configured, every request — including the /ws WebSocket
// upgrade — must present `Authorization: Bearer <token>` or it is rejected
// with HTTP 401 before any handler runs (so the WS upgrade never allocates a
// session for an unauthenticated client).
//
// Token comparison uses crypto/subtle.ConstantTimeCompare to prevent timing
// attacks.
func BearerAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expected := []byte(token)
	deny := func(w http.ResponseWriter, r *http.Request, reason string) {
		slog.Warn("rejecting request: "+reason,
			"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if header == "" {
			slog.Debug("rejecting request: no credentials presented",
				"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(header, prefix) ||
			!tokenMatches(strings.TrimSpace(header[len(prefix):]), expected) {
			deny(w, r, "bad or malformed bearer header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenMatches compares a presented credential against the configured one.
// ConstantTimeCompare returns 0 on any mismatch including a length
// difference, so the comparison stays constant-time for inputs of any size.
func tokenMatches(got string, expected []byte) bool {
	return subtle.ConstantTimeCompare([]byte(got), expected) == 1
}
