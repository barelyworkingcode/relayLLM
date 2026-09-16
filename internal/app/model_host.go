package app

// C9 (plan-broker-and-sessions.md §2) wiring: router.sock must exist, and
// RegisterModelHost must be attempted, whenever relay launched this process —
// independent of whether --router-port is configured (a relay-launched
// relayLLM with no TCP listener still has to serve relay's model broker, or
// relay's endpoint 503s "no host" forever; app.go's Main keeps the single
// router.RelayRouter built at startup alive for exactly this reason instead
// of rebuilding one here) and independent of whether RegisterManifest
// succeeded (front-door dispatch and model-host registration are separate
// relay capabilities; a manifest hiccup must not silently strand the model
// broker). Factored out of Main into plain functions so both decisions can
// be driven directly by a *testing.T, without flag.Parse or a real os.Exit.

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"relayllm/internal/relay"
)

// refuseLaunchedRouterPortOrExit is C9's own rule for P2 (L-S1): a
// relay-launched relayLLM exposes model routing to relay only through
// router.sock — a --router-port TCP listener would be a second, ungated path
// to the same broker, so a launched process refuses to start with one
// configured, exiting 78 (matching RegisterModelHostOrExit's own convention
// for a refused capability/config combination). Standalone (not launched)
// keeps serving --router-port exactly as before (L-M2). exitFn is injected —
// production (app.go) passes os.Exit — so a test can observe the refusal
// without ending the test process.
func refuseLaunchedRouterPortOrExit(routerPort string, exitFn func(int)) {
	if relay.Launched() && routerPort != "" {
		slog.Error("--router-port is not permitted when relay launched this process; relay reaches model routing only through router.sock (C9)",
			"routerPort", routerPort)
		exitFn(78)
	}
}

// resolveRouterSocketPath applies the {dataDir}/router.sock default and
// makes the result absolute. RegisterModelHost refuses a relative path (relay
// dials it from its own working directory, never this process's), and a
// relative --data-dir — or an explicit relative --router-socket — would
// otherwise produce one silently, turning a configuration accident into a
// fatal RegisterModelHost refusal (exit 78) instead of a working socket.
func resolveRouterSocketPath(flagValue, dataDir string) (string, error) {
	path := flagValue
	if path == "" {
		path = filepath.Join(dataDir, "router.sock")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve router socket path %q: %w", path, err)
	}
	return abs, nil
}

// registerWithRelay runs both of the post-listener relay registrations.
// RegisterModelHost is attempted regardless of whether MaybeRegisterManifest
// succeeded — see this file's header for why that coupling would be a bug,
// not a simplification. exitFn is injected (production: os.Exit) so
// RegisterModelHostOrExit's fatal path is observable from a test.
func registerWithRelay(dataDir, socketPath, internalToken, routerSocketPath string, exitFn func(int)) {
	relay.MaybeRegisterManifest(dataDir, socketPath, internalToken)
	if relay.Launched() {
		relay.RegisterModelHostOrExit(routerSocketPath, exitFn)
	}
}
