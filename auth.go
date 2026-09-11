package main

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
func generateBearerToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// authCookieName is the cookie bearerAuth accepts in place of an
// Authorization header, and authQueryParam is the one-shot URL parameter
// that mints it. See bearerAuth's doc comment for why a browser needs both.
const (
	authCookieName = "relayllm_auth"
	authQueryParam = "token"
)

// bearerAuth wraps an http.Handler with bearer-token authentication.
//
// If `token` is empty, the middleware is a no-op pass-through. This is the
// dev-mode default — relayLLM running on loopback without orchestrator
// credentials. Operators get a loud startup warning in main.go in that case.
//
// When a token is configured, every request — including the /ws WebSocket
// upgrade — must present the token or it is rejected with HTTP 401 before
// any handler runs (so the WS upgrade never allocates a session for an
// unauthenticated client).
//
// Three credential carriers are accepted, all of them the same secret
// compared the same constant-time way:
//
//  1. `Authorization: Bearer <token>` — the machine path (relay's
//     dispatcher, the permission hook, direct socket clients). Checked
//     first; a present-but-wrong header is a rejection, never a fall-through
//     to the weaker carriers, so a broken client can't be silently upgraded
//     to a cookie it doesn't have.
//  2. `Cookie: relayllm_auth=<token>` — the browser path.
//  3. `?token=<token>` — a bootstrap for (2) only. A browser navigating to a
//     URL cannot attach a header, and the /status dashboard's own
//     `fetch('/api/status/detailed')` inherits nothing from the address bar
//     but cookies (it sends `credentials: 'same-origin'`), so accepting the
//     parameter alone would render a shell whose every poll 401s. A GET/HEAD
//     carrying a valid parameter is therefore answered with a Set-Cookie and
//     a 302 back to the same URL minus the parameter: the secret leaves the
//     address bar, the history entry, and any Referer the page's subresources
//     would otherwise carry, and every subsequent request on that origin —
//     document, CSS, JS, XHR — authenticates by cookie.
//
// The cookie is not a weaker credential than the header: it is the identical
// 64-char secret, matched with the identical crypto/subtle comparison. The
// one property a cookie has that a header does not is ambient attachment, so
// it is issued `SameSite=Strict` — a request originating from any other
// site's page never carries it, which is what keeps a malicious page the
// operator happens to have open from driving this API through their browser.
// `HttpOnly` keeps it out of reach of script on the page, and `Secure` is set
// whenever the request arrived over TLS (unconditionally setting it would
// make the cookie undeliverable over the plaintext loopback listener that is
// this feature's whole point).
//
// Token comparison uses crypto/subtle.ConstantTimeCompare to prevent timing
// attacks. The full design lives in eve/plans/cozy-honking-toast.md Section B.
func bearerAuth(token string, next http.Handler) http.Handler {
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
		if header := r.Header.Get("Authorization"); header != "" {
			const prefix = "Bearer "
			if !strings.HasPrefix(header, prefix) ||
				!tokenMatches(strings.TrimSpace(header[len(prefix):]), expected) {
				deny(w, r, "bad or malformed bearer header")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if param := r.URL.Query().Get(authQueryParam); param != "" {
			if !tokenMatches(param, expected) {
				deny(w, r, "bad token query parameter")
				return
			}
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				redirectWithAuthCookie(w, r, token)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if c, err := r.Cookie(authCookieName); err == nil && tokenMatches(c.Value, expected) {
			next.ServeHTTP(w, r)
			return
		}

		slog.Debug("rejecting request: no credentials presented",
			"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// tokenMatches compares a presented credential against the configured one.
// ConstantTimeCompare returns 0 on any mismatch including a length
// difference, so the comparison stays constant-time for inputs of any size.
func tokenMatches(got string, expected []byte) bool {
	return subtle.ConstantTimeCompare([]byte(got), expected) == 1
}

// redirectWithAuthCookie converts a valid ?token= into a cookie and bounces
// the browser to the same path without it.
//
// The redirect target is built from r.URL's path and query only — never the
// scheme/host the client sent — so this can't be turned into an open
// redirect by a crafted Host header.
func redirectWithAuthCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
	clean := *r.URL
	q := clean.Query()
	q.Del(authQueryParam)
	clean.RawQuery = q.Encode()
	clean.Scheme, clean.Host, clean.User = "", "", nil
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, clean.RequestURI(), http.StatusFound)
}
