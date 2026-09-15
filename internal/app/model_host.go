package app

// C9 (plan-broker-and-sessions.md §2) wiring: router.sock must exist, and
// RegisterModelHost must be attempted, whenever relay launched this process —
// independent of whether --router-port is configured (a relay-launched
// relayLLM with no TCP listener still has to serve relay's model broker, or
// relay's endpoint 503s "no host" forever) and independent of whether
// RegisterManifest succeeded (front-door dispatch and model-host
// registration are separate relay capabilities; a manifest hiccup must not
// silently strand the model broker). Factored out of Main into plain
// functions so both decisions can be driven directly by a *testing.T,
// without flag.Parse or a real os.Exit.

import (
	"fmt"
	"path/filepath"

	"relayllm/internal/config"
	"relayllm/internal/registry"
	"relayllm/internal/relay"
	"relayllm/internal/router"
	"relayllm/internal/servermanager"
)

// ensureModelHostRouter returns relayRouter unchanged when it is already
// non-nil (StartRelayRouter built and bound it for --router-port) or when
// relay did not launch this process (standalone has no identity for
// router.sock to admit against, so there is nothing to build). Otherwise it
// builds a fresh, TCP-unbound router purely so router.sock has an object to
// serve — see router.BuildRelayRouter's doc comment for why StartRelayRouter
// itself can't be reused for this: its early returns (no addrs, or no
// backends/passthrough) only make sense for the TCP listener.
func ensureModelHostRouter(relayRouter *router.RelayRouter, managers []*servermanager.ServerManager, proxyRegistry *registry.ProxyRegistry, virtualCfg *config.VirtualLLMConfig, routerCfg *config.RouterConfig, tlsCert, tlsKey string) *router.RelayRouter {
	if !relay.Launched() || relayRouter != nil {
		return relayRouter
	}
	return router.BuildRelayRouter(managers, proxyRegistry, virtualCfg, routerCfg, tlsCert, tlsKey)
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
