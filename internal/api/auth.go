package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
)

// GenerateBearerToken returns a 64-char (32-byte) random hex token. Used
// when relayLLM auto-provisions its own listener auth (no flag/env override).
func GenerateBearerToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// bearerAuth wraps an http.Handler with bearer-token authentication. It
// guards the Unix socket only — app.go's --http-port TCP front is served
// anonymously (see startMainTCPListener's call site), protected by
// --http-bind rather than a token, deliberately matching --router-port's
// existing unauthenticated-by-bind-address posture. The TCP front's
// anonymity is why it is additionally wrapped in TCPDiagnosticsOnly
// (tcp_diagnostics.go): with no credential at all, the route surface itself
// has to be the read-only diagnostics allowlist rather than the full table
// this middleware guards. There is therefore no browser-facing credential
// path here anymore (no cookie, no ?token= bootstrap) — every caller of the
// socket is a machine client (relay's dispatcher, the permission hook, a
// direct socket client) that can just send a header.
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

// HookTokenValidator validates a per-session permission-hook credential
// (minted by permission.PermissionManager.MintHookToken) and reports the
// session it is bound to. Declared here rather than imported from
// internal/permission so this file's only dependency stays net/http +
// stdlib crypto — the same shape as tokenMatches above.
type HookTokenValidator func(token string) (sessionID string, ok bool)

type hookSessionContextKey struct{}

// HookSessionFromContext returns the session id a request's hook token
// authenticated as. False means the request came in on the full internal
// bearer (or auth is disabled) and carries no per-session restriction —
// RegisterPermissionRoutes' handler treats that exactly as it did before
// this credential existed.
func HookSessionFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(hookSessionContextKey{}).(string)
	return id, ok
}

// HookScopedBearerAuth is BearerAuth plus one narrow exception: a POST to
// /api/permission may authenticate with a per-session hook token in place
// of the full internal bearer. That credential is deliberately weaker — it
// is minted per Claude Code session (internal/permission.MintHookToken) and
// is meant to grant only the permission-decision exchange for the one
// session it was minted for — so validate is consulted for no other path,
// method, or the /ws upgrade: those all still require the exact internal
// bearer, identical to BearerAuth alone. A validated hook token still only
// proves "bound to some session"; the handler is responsible for checking
// that session against the one named in the request body (HookSessionFromContext)
// so a token for session A can't authorize a decision for session B.
//
// This is deliberate rather than a generalization of BearerAuth: widening
// the path/method check would turn the hook token into a second internal
// bearer, which is the exact exposure this credential exists to close.
func HookScopedBearerAuth(token string, validate HookTokenValidator, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expected := []byte(token)
	deny := func(w http.ResponseWriter, r *http.Request, reason string) {
		slog.Warn("rejecting request: "+reason,
			"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if header == "" || !strings.HasPrefix(header, prefix) {
			slog.Debug("rejecting request: no credentials presented",
				"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		presented := strings.TrimSpace(header[len(prefix):])
		if tokenMatches(presented, expected) {
			next.ServeHTTP(w, r)
			return
		}
		if validate != nil && r.Method == http.MethodPost && r.URL.Path == "/api/permission" {
			if sessionID, ok := validate(presented); ok {
				ctx := context.WithValue(r.Context(), hookSessionContextKey{}, sessionID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		deny(w, r, "bad or malformed bearer header")
	})
}
