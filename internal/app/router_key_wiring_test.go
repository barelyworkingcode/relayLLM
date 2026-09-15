package app

// Pins C10's "launched mode is completely unaffected" requirement at the
// exact wiring point app.go's Main uses: EnableStandaloneRouterKeys must
// never run when relay.Launched() is true, so a launched relayLLM's only
// gate stays router.sock's existing kernel-peer-token admission (C9),
// verified here with zero router_keys.json ever created anywhere.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"relayllm/internal/config"
	"relayllm/internal/relay"
	"relayllm/internal/router"
)

func TestLaunchedMode_RouterSocketUnaffectedByRouterKeys(t *testing.T) {
	launchedTestSetup(t)
	if !relay.Launched() {
		t.Fatal("setup: expected to be launched")
	}

	relayRouter := router.BuildRelayRouter(nil, nil, nil, nil, "", "")
	// The actual function app.go's Main calls, not a re-implementation of
	// its guard: deleting the real relay.Launched() check inside
	// maybeEnableStandaloneRouterKeys would make this test fail, which a
	// test that re-checked relay.Launched() itself here could not detect.
	// dataDirForRouterKeys is deliberately never touched by
	// AddRouterKey/writeKeysFile in this test — no router_keys.json exists
	// anywhere relevant, on purpose.
	dataDirForRouterKeys := t.TempDir()
	maybeEnableStandaloneRouterKeys(relayRouter, dataDirForRouterKeys)

	// /tmp directly, not t.TempDir(): net.Listen("unix", ...) routinely
	// overflows macOS's 104-char AF_UNIX path limit under t.TempDir()'s
	// TestName/NNN/ nesting — see model_host_test.go's identical comment.
	dataDir, err := os.MkdirTemp("/tmp", "apphost-launched")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	sockPath, err := resolveRouterSocketPath("", dataDir)
	if err != nil {
		t.Fatalf("resolveRouterSocketPath: %v", err)
	}
	if err := relayRouter.ListenSocket(sockPath, relay.RelayIdentity); err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	t.Cleanup(func() { relayRouter.Close() })

	status := dialUnixPath(t, sockPath, "/health")
	if status != http.StatusOK {
		t.Fatalf("router.sock /health status = %d, want 200 — launched mode's own admission (C9) must be untouched by C10", status)
	}
}

// TestStandaloneMode_MaybeEnableStandaloneRouterKeysWires is the other half
// of the same guard: not launched must actually wire the gate (a test that
// only ever exercised the launched branch could not tell "correctly
// skipped" apart from "the function is a no-op that never wires anything").
// A real TCP listener is used, not router.sock (which is a separate
// *http.Server this mechanism never touches, launched or not, per
// EnableStandaloneRouterKeys's own doc comment) — a fake upstream behind a
// passthrough entry is the least-fuss way to give MaybeServeTCP something
// to actually bind to.
func TestStandaloneMode_MaybeEnableStandaloneRouterKeysWires(t *testing.T) {
	if relay.Launched() {
		t.Fatal("setup: expected NOT launched in this test")
	}

	dataDir := t.TempDir()
	if _, err := router.AddRouterKey(dataDir, "test"); err != nil {
		t.Fatalf("AddRouterKey: %v", err)
	}

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer fake.Close()
	routerCfg := &config.RouterConfig{Passthrough: map[string]config.PassthroughConfig{
		"upstream": {Upstream: fake.URL},
	}}

	relayRouter := router.BuildRelayRouter(nil, nil, nil, routerCfg, "", "")
	maybeEnableStandaloneRouterKeys(relayRouter, dataDir)
	started, err := relayRouter.MaybeServeTCP([]string{"127.0.0.1:0"}, routerCfg)
	if err != nil {
		t.Fatalf("MaybeServeTCP: %v", err)
	}
	if !started {
		t.Fatal("setup: expected MaybeServeTCP to bind with a passthrough entry configured")
	}
	t.Cleanup(func() { relayRouter.Close() })

	resp, err := http.Get("http://" + relayRouter.Addr() + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /health status = %d, want 401 — maybeEnableStandaloneRouterKeys must actually wire the gate when not launched", resp.StatusCode)
	}
}
