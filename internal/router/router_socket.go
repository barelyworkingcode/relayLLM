package router

// router.sock: relay's private, tokenless path into this router
// (plan-broker-and-sessions.md §2 C9). Two things distinguish it from the
// TCP router built in NewRelayRouter:
//
//   - Admission: every accepted connection's peer kernel audit token must
//     equal the (pid, pidversion) relayLLM captured off relay's own Hello
//     dial (internal/relay's RelayIdentity, spike SP1). A caller on this
//     socket authenticates by being relay, not by a header — there is no
//     bearer to check.
//   - Mux: the TCP mux minus /api/ (Claude Code's OAuth bootstrap
//     passthrough to api.anthropic.com) and every configured
//     router.passthrough /<name>/ route. Both forward whatever credential
//     the CALLER presented — but the caller here is relay's model broker
//     acting on behalf of a project or service grant, never a holder of its
//     own Anthropic/OpenAI credential, so there is no credential for those
//     routes to forward. Refused outright (404) rather than silently falling
//     through to handleProxy's plain model dispatch, so a probe against a
//     passthrough-shaped path gets an unambiguous "not routable here" instead
//     of an accidental dispatch decision. /v1/messages is narrowed the same
//     way: only router.anthropic.modelMap targets are servable — a request
//     naming any other model 404s rather than reaching the real Anthropic
//     passthrough.

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"relayllm/internal/peertoken"
)

type routerSocketAdmittedKey struct{}

// SocketHandler builds the http.Handler router.sock serves — see this file's
// header for exactly what is removed relative to the TCP mux.
func (p *RelayRouter) SocketHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", p.handleModels)
	mux.HandleFunc("GET /models", p.handleModels)
	mux.HandleFunc("POST /models/load", p.handleModelLoad)
	mux.HandleFunc("POST /models/unload", p.handleModelUnload)
	mux.HandleFunc("GET /health", p.handleHealth)
	mux.HandleFunc("POST /v1/audio/transcriptions", p.handleAudioTranscription)
	mux.HandleFunc("POST /v1/messages", p.handleAnthropicMessagesSocket)
	mux.HandleFunc("POST /v1/messages/count_tokens", p.handleAnthropicMessagesSocket)
	mux.HandleFunc("/api/", socketRouteRefused)
	for _, name := range p.passthroughNames {
		mux.HandleFunc("/"+name+"/", socketRouteRefused)
	}
	mux.HandleFunc("/", p.handleProxy)
	return mux
}

// socketRouteRefused answers a credential-passthrough-shaped path on
// router.sock: a clean 404, deliberately distinct from letting the request
// fall through to handleProxy (which would try to read a "model" field out
// of a body shaped for an entirely different upstream).
func socketRouteRefused(w http.ResponseWriter, r *http.Request) {
	writeRouterError(w, http.StatusNotFound, "not routable on router.sock")
}

// ListenSocket binds path (0600), replacing any stale file a prior crashed
// process left behind — the same convention relay's own model.sock and
// bridge.NewBridgeServer use. want is consulted fresh on every accepted
// connection (never cached across connections) so admission always reflects
// whatever internal/relay.RelayIdentity currently holds; in practice Hello
// runs once per process lifetime, before this is ever called, but reading it
// live rather than snapshotting at construction costs nothing and removes an
// ordering assumption between the two.
func (p *RelayRouter) ListenSocket(path string, want func() (peertoken.Process, bool)) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	srv := &http.Server{
		Handler:           admitRelayOnly(want, p.SocketHandler()),
		ReadHeaderTimeout: 30 * time.Second,
		// Admission is decided once per connection, not once per request:
		// the peer's identity cannot change over the life of one accepted
		// net.Conn, and re-reading LOCAL_PEERTOKEN on every keep-alive
		// request would be pure overhead for the same answer.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			admitted := false
			if tok, err := peertoken.FromConn(c); err == nil {
				if proc, ok := want(); ok && tok.Process() == proc {
					admitted = true
				}
			}
			return context.WithValue(ctx, routerSocketAdmittedKey{}, admitted)
		},
	}
	p.socketSrv = srv
	p.socketLn = ln
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("router.sock: serve error", "error", err)
		}
	}()
	return nil
}

// admitRelayOnly refuses any request whose connection did not carry the
// admitted peer identity, with the exact body C9 specifies. "then close":
// the Connection: close response header tells net/http to tear the
// connection down after replying, rather than keep an unauthorized peer's
// connection alive for a further request.
func admitRelayOnly(want func() (peertoken.Process, bool), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admitted, _ := r.Context().Value(routerSocketAdmittedKey{}).(bool)
		if !admitted {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"unauthorized","type":"authentication_error"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
