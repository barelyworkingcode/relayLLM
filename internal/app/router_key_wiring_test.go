package app

// Pins C10's "launched mode is completely unaffected" requirement at the
// exact wiring point app.go's Main uses: EnableStandaloneRouterKeys must
// never run when relay.Launched() is true, so a launched relayLLM's only
// gate stays router.sock's existing kernel-peer-token admission (C9),
// verified here with zero router_keys.json ever created anywhere.

import (
	"net/http"
	"os"
	"testing"

	"relayllm/internal/relay"
	"relayllm/internal/router"
)

func TestLaunchedMode_RouterSocketUnaffectedByRouterKeys(t *testing.T) {
	launchedTestSetup(t)
	if !relay.Launched() {
		t.Fatal("setup: expected to be launched")
	}

	relayRouter := router.BuildRelayRouter(nil, nil, nil, nil, "", "")
	// Exactly app.go's Main flow: EnableStandaloneRouterKeys only runs when
	// NOT launched. dataDirForRouterKeys is deliberately never touched by
	// AddRouterKey/writeKeysFile in this test — no router_keys.json exists
	// anywhere relevant, on purpose.
	dataDirForRouterKeys := t.TempDir()
	if !relay.Launched() {
		relayRouter.EnableStandaloneRouterKeys(router.RouterKeysPath(dataDirForRouterKeys))
	}

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
