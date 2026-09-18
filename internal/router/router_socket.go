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
//   - Mux: the TCP mux, including /api/ (Claude Code's bootstrap passthrough
//     to api.anthropic.com), every router.passthrough /<name>/ route and
//     unmapped /v1/messages. Relay's model endpoint sends those here with
//     the CLIENT'S OWN credential still on the request (plans/
//     client-model-routing.md): relay holds no upstream credential of its
//     own and never forwards one, so what these routes forward is exactly
//     what the client put on the wire, byte for byte. Relay classifies each
//     request (a model in its catalog is served locally, a provider's own
//     model or a passthrough path is forwarded) and strips its own
//     credentials before it dials this socket.
//
// The one difference from the TCP mux is /v1/messages and its count_tokens:
// a model outside router.anthropic.modelMap is forwarded to the real
// Anthropic API only when the request carries a client credential
// (Authorization or x-api-key). Relay strips both from a request it served
// locally, so a local-model request that reached this mux by mistake (a
// stale relay catalog, say) 404s instead of sending its prompt to Anthropic
// with no credential at all. A mapped model is served locally exactly as on
// TCP.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"relayllm/internal/peertoken"
)

type routerSocketAdmittedKey struct{}

// SocketHandler builds the http.Handler router.sock serves — see this file's
// header for how it differs from the TCP mux. Everything but the two
// Anthropic routes is p.mux itself, so a route mounted there later
// (setPassthrough runs after construction) is served here without a second
// registration to forget.
func (p *RelayRouter) SocketHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", p.handleAnthropicMessagesSocket)
	mux.HandleFunc("POST /v1/messages/count_tokens", p.handleAnthropicCountTokensSocket)
	mux.Handle("/", p.mux)
	return mux
}

// hasClientCredential reports whether r carries a credential a client would
// present to its own provider. Only the headers Anthropic's API reads count.
func hasClientCredential(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != ""
}

// removeStaleSocket removes path only when it already exists AND is a
// socket, refusing to silently unlink and recreate a regular file, a
// directory, or anything else a misconfigured --router-socket might name. A
// prior crashed relayLLM leaves exactly a stale socket file here — the same
// convention bridge.NewBridgeServer and relay's own model.sock use — and
// anything else at this path is a configuration mistake this must surface,
// not paper over by deleting whatever was there.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("router.sock: refusing to remove %s: not a socket (mode %s)", path, info.Mode())
	}
	return os.Remove(path)
}

// ListenSocket binds path (0600), replacing any stale file a prior crashed
// process left behind — the same convention relay's own model.sock and
// bridge.NewBridgeServer use. want is consulted fresh on every accepted
// connection (never cached across connections) so admission always reflects
// whatever internal/relay.RelayIdentity currently holds; in practice Hello
// runs once per process lifetime, before this is ever called, but reading it
// live rather than snapshotting at construction costs nothing and removes an
// ordering assumption between the two.
//
// This is deliberate: admission is by kernel peer token (ConnContext below),
// not filesystem permission — a peer that isn't relay is refused regardless
// of who else could open() this path — so the brief window between
// net.Listen creating the file and the os.Chmod just below is not a real
// exposure under an ordinary 022-or-tighter umask, and narrowing the
// process-wide umask around the call (an earlier version of this function
// did) is a global side effect with no test coverage to show it does
// anything the chmod doesn't already guarantee once ListenSocket returns.
func (p *RelayRouter) ListenSocket(path string, want func() (peertoken.Process, bool)) error {
	if err := removeStaleSocket(path); err != nil {
		return err
	}
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
	p.socketPath = path
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
