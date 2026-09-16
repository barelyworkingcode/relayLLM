package app

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"relayllm/internal/api"
	"relayllm/internal/config"
	"relayllm/internal/netutil"
	"relayllm/internal/registry"
	"relayllm/internal/relay"
	"relayllm/internal/router"
	"relayllm/internal/servermanager"
)

func Main() {
	// Deliberate: first, before anything that can spawn a child (a managed
	// model server) or start a goroutine that might. It closes the inherited
	// launch fd and scrubs RELAY_LAUNCH_FD from this process's environment,
	// so no child can inherit either.
	if launched, hello, err := relay.BootstrapLaunch(); err != nil {
		slog.Error("relay launch handshake failed; refusing to start", "error", err)
		os.Exit(1)
	} else if launched {
		slog.Info("launched by relay; bridge identity bound", "serviceId", hello.ServiceID, "relayPid", hello.RelayPID)
	}

	startTime := time.Now()
	dataDir := flag.String("data-dir", envOrDefault("RELAY_LLM_DATA", ""), "Data directory (default: ~/.config/relayLLM)")
	openaiConfigPath := flag.String("openai-config", envOrDefault("OPENAI_CONFIG", ""), "Path to OpenAI-compatible endpoints config JSON (default: {data-dir}/openai_endpoints.json)")
	socketPath := flag.String("socket", envOrDefault("RELAY_LLM_SOCKET", ""), "Unix domain socket path for this listener. Defaults to {data-dir}/relayllm.sock. Used by direct clients in standalone mode and by relay (via manifest registration) when running under relay.")
	internalToken := flag.String("token", envOrDefault("RELAY_LLM_TOKEN", ""), "Bearer token required on every request. Empty → auto-generated random hex (printed at startup).")
	llamaServerPath := flag.String("llama-server-path", envOrDefault("LLAMA_SERVER_PATH", ""), "Path to llama-server binary (default: llama-server on PATH)")
	mlxServePath := flag.String("mlx-serve-path", envOrDefault("MLX_SERVE_PATH", ""), "Path to mlx-serve binary (default: mlx-serve on PATH)")
	routerPort := flag.String("router-port", envOrDefault("RELAY_ROUTER_PORT", ""), "Port for the unified OpenAI-compatible relay-router fronting managed servers (llama-server, mlx-serve) + OpenAI endpoints (empty to disable)")
	routerBind := flag.String("router-bind", envOrDefault("RELAY_ROUTER_BIND", "127.0.0.1"), "Comma-separated bind addresses for the relay-router TCP listener, one per interface (e.g. 127.0.0.1,192.168.64.1). Include 0.0.0.0 to accept connections from other hosts.")
	routerTLSCert := flag.String("router-tls-cert", envOrDefault("RELAY_LLM_ROUTER_TLS_CERT", ""), "TLS certificate file for the relay-router listener. Requires --router-tls-key; empty (with key also empty) serves plain http.")
	routerTLSKey := flag.String("router-tls-key", envOrDefault("RELAY_LLM_ROUTER_TLS_KEY", ""), "TLS private key file for the relay-router listener. Requires --router-tls-cert.")
	routerSocketPath := flag.String("router-socket", envOrDefault("RELAY_ROUTER_SOCKET", ""), "Unix socket path for relay's private, tokenless path into the relay-router (default: {data-dir}/router.sock). Only opened when launched by relay (RELAY_LAUNCH_FD set); ignored standalone. See plan-broker-and-sessions.md §2 C9.")
	httpPort := flag.String("http-port", envOrDefault("RELAY_LLM_HTTP_PORT", ""), "Port for an additional, unauthenticated TCP listener serving ONLY the read-only /status diagnostics dashboard (GET /status, /api/status, /api/status/detailed) — protected by --http-bind, not a bearer token, same as --router-port. Every other route 404s on this port; use the bearer-authenticated --socket for those. Empty to disable.")
	httpBind := flag.String("http-bind", envOrDefault("RELAY_LLM_HTTP_BIND", "127.0.0.1"), "Comma-separated bind addresses for the --http-port listener, one per interface (e.g. 127.0.0.1,192.168.64.1). Set to 0.0.0.0 to accept connections from other hosts.")
	httpTLSCert := flag.String("http-tls-cert", envOrDefault("RELAY_LLM_HTTP_TLS_CERT", ""), "TLS certificate file for the --http-port listener. Requires --http-tls-key; empty (with key also empty) serves plain http.")
	httpTLSKey := flag.String("http-tls-key", envOrDefault("RELAY_LLM_HTTP_TLS_KEY", ""), "TLS private key file for the --http-port listener. Requires --http-tls-cert.")
	flag.Parse()

	// C9 (L-S1): a relay-launched relayLLM reaches relay only through
	// router.sock; --router-port would be a second, ungated path to the same
	// model broker.
	refuseLaunchedRouterPortOrExit(*routerPort, os.Exit)

	// Parsed once here; every downstream consumer (loopback validation,
	// listenAddrs, the manifest) works off these lists rather than
	// re-splitting the raw flag value.
	routerBinds := netutil.ParseBindList(*routerBind)
	httpBinds := netutil.ParseBindList(*httpBind)

	if (*routerTLSCert == "") != (*routerTLSKey == "") {
		missing := "--router-tls-cert/RELAY_LLM_ROUTER_TLS_CERT"
		if *routerTLSCert != "" {
			missing = "--router-tls-key/RELAY_LLM_ROUTER_TLS_KEY"
		}
		slog.Error("relay router TLS requires both cert and key", "missing", missing)
		os.Exit(1)
	}

	if err := validateHTTPListener(*httpPort, *httpTLSCert, *httpTLSKey); err != nil {
		slog.Error("refusing to start: --http-port is misconfigured", "error", err)
		os.Exit(1)
	}

	if *dataDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			dir, _ = os.UserHomeDir()
		}
		*dataDir = filepath.Join(dir, "relayLLM")
	}
	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		slog.Error("failed to create data directory", "path", *dataDir, "error", err)
		os.Exit(1)
	}

	// Listener address: relayLLM picks where it binds. relay (if running)
	// learns the path via manifest registration; standalone clients dial it
	// directly. Default keeps the socket under data-dir so it's discoverable.
	if *socketPath == "" {
		*socketPath = filepath.Join(*dataDir, "relayllm.sock")
	}
	// Bearer token: relayLLM picks its own. Auto-generated if unset so the
	// listener is never unauthenticated. Under relay the token travels via
	// manifest registration. For standalone direct-client use, pin it with
	// --token / RELAY_LLM_TOKEN — we deliberately do not log the
	// auto-generated value so it doesn't end up in shipped log files.
	tokenAutoGenerated := *internalToken == ""
	if tokenAutoGenerated {
		*internalToken = api.GenerateBearerToken()
	}

	slog.Info("starting relayLLM",
		"socket", *socketPath,
		"dataDir", *dataDir,
		"tokenAutoGenerated", tokenAutoGenerated)

	cfg, err := config.LoadConfig(*dataDir, *openaiConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// router.anthropic and router.passthrough forward whatever credential the
	// client sent (Claude Code's or ChatGPT's OAuth bearer, or an API key)
	// straight to the upstream on every request. That's fine on loopback;
	// exposed on a non-loopback bind with no TLS on the router's own
	// listener, it's a credential leaving the box in plaintext to anyone who
	// can reach the port. Same fail-closed shape as the router-TLS-pair guard
	// above.
	if cfg.Router.ForwardsClientCredentials() && *routerTLSCert == "" {
		if host, ok := netutil.FirstNonLoopbackBind(routerBinds); ok {
			slog.Error("relay router: router.anthropic or router.passthrough is configured with a non-loopback --router-bind and no TLS cert; refusing to start (passthrough forwards the client's real credential on every request)",
				"router-bind", host)
			os.Exit(1)
		}
	}

	if len(cfg.OpenAI.Endpoints) > 0 {
		slog.Info("openai endpoints loaded", "count", len(cfg.OpenAI.Endpoints), "names", cfg.OpenAI.Names())
	}
	var llamaManager *servermanager.ServerManager
	if len(cfg.Llama.Models) > 0 {
		llamaManager = servermanager.NewServerManager(servermanager.LlamaProfile, cfg.Llama, *llamaServerPath)
		llamaManager.StartIdleReaper()
		slog.Info("llama models configured", "count", len(cfg.Llama.Models), "binary", llamaManager.BinaryPath())
	}

	var mlxManager *servermanager.ServerManager
	if len(cfg.Mlx.Models) > 0 {
		mlxManager = servermanager.NewServerManager(servermanager.MlxProfile, cfg.Mlx, *mlxServePath)
		mlxManager.StartIdleReaper()
		slog.Info("mlx models configured", "count", len(cfg.Mlx.Models), "binary", mlxManager.BinaryPath())
	}

	var proxyRegistry *registry.ProxyRegistry
	if len(cfg.OpenAI.Endpoints) > 0 {
		proxyRegistry = registry.NewProxyRegistry(cfg.OpenAI)
	}

	// Slice order is the router's dispatch priority: llama wins alias
	// collisions with mlx.
	var managers []*servermanager.ServerManager
	if llamaManager != nil {
		managers = append(managers, llamaManager)
	}
	if mlxManager != nil {
		managers = append(managers, mlxManager)
	}
	warnAliasShadowing(managers, cfg.OpenAI.Endpoints)
	warnVirtualModelConfig(cfg.Virtual, managers, cfg.OpenAI.Endpoints)
	warnAnthropicModelMap(cfg.Router.Anthropic, managers, cfg.OpenAI.Endpoints, cfg.Virtual)

	routerAddrs := netutil.ListenAddrs(routerBinds, *routerPort)
	// Built exactly once — see BuildRelayRouter's doc comment for why:
	// setAnthropic/setPassthrough each log a warning per invalid config
	// entry, so building a second router.RelayRouter from the same config
	// (the shape an earlier version of this file had, gating a rebuild on
	// StartRelayRouter having returned nil) would double every one of those
	// log lines for no reason. cfg.Router is passed straight in rather than
	// set on the router afterward — see BuildRelayRouter's own setters for
	// why a separate post-construction call would race the router's first
	// accepted connection.
	relayRouter := router.BuildRelayRouter(managers, proxyRegistry, cfg.Virtual, cfg.Router, *routerTLSCert, *routerTLSKey)
	// C10: standalone router keys. Factored into a plain function (see its
	// own doc comment) so a *testing.T can drive the exact same decision
	// Main makes, rather than a test re-implementing the condition
	// alongside it — a re-implemented condition would still pass if the
	// real guard here were ever deleted.
	maybeEnableStandaloneRouterKeys(relayRouter, *dataDir)
	// MaybeServeTCP binds every configured address best-effort (see
	// listenAll) — one that can't be bound in this deployment is logged and
	// skipped, not fatal, since --router-bind may legitimately name an
	// address that's only assignable in some environments. Only if every
	// single one fails does it return an error, the same as the http
	// front's below: that's the case where the router would otherwise be
	// silently absent from a healthy-looking process.
	tcpStarted, err := relayRouter.MaybeServeTCP(routerAddrs, cfg.Router)
	if err != nil {
		slog.Error("failed to start relay router", "error", err)
		os.Exit(1)
	}
	if !tcpStarted && !relay.Launched() {
		// Nothing to serve over TCP (no --router-port, or nothing configured
		// to route to) and no router.sock to keep the object alive for
		// either: drop it, matching the pre-router.sock "no router at all"
		// nil the dashboard (DetailedStatusDeps.Router) already treats as
		// absent.
		relayRouter = nil
	}

	// router.sock (C9): relay's private, tokenless path into the relay-router,
	// opened only when relay actually launched this process — RelayIdentity()
	// is what admits a caller, and standalone has no such identity to admit
	// against. relayRouter is guaranteed non-nil here whenever launched (the
	// drop-to-nil branch above requires !relay.Launched()): a relay-launched
	// relayLLM must register as relay's model host regardless of whether it
	// also serves TCP, or relay's own model.sock 503s "no host" forever for
	// a deployment that never set --router-port.
	var resolvedRouterSocketPath string
	if relay.Launched() {
		resolvedRouterSocketPath, err = resolveRouterSocketPath(*routerSocketPath, *dataDir)
		if err != nil {
			slog.Error("failed to resolve router socket path", "error", err)
			os.Exit(1)
		}
		if err := relayRouter.ListenSocket(resolvedRouterSocketPath, relay.RelayIdentity); err != nil {
			slog.Error("failed to listen on router socket", "path", resolvedRouterSocketPath, "error", err)
			os.Exit(1)
		}
		slog.Info("router.sock listening", "path", resolvedRouterSocketPath)
	}

	mux := http.NewServeMux()
	api.RegisterStatusRoutes(mux, llamaManager, mlxManager, startTime)
	api.RegisterDetailedStatusRoutes(mux, api.DetailedStatusDeps{
		Managers:  managers,
		Registry:  proxyRegistry,
		Virtual:   cfg.Virtual,
		Router:    relayRouter,
		StartTime: startTime,
	})

	// Build the handler chain. recoverMiddleware sits closest to the mux so it
	// catches panics from real handlers regardless of which front is used.
	//
	// The Unix socket and the --http-port TCP front deliberately do NOT
	// share one handler value: the socket carries relay's manifest bridging
	// (a machine client, never a browser), so it keeps an auth layer in
	// front and serves the full route table. The TCP front carries no
	// bearer token at all — reachability there is still gated by
	// --http-bind, the same posture --router-port has always had — but on
	// top of that it is wrapped in api.TCPDiagnosticsOnly, which serves only
	// the read-only /status dashboard and 404s everything else. See
	// TCPDiagnosticsOnly's doc comment.
	recovered := api.RecoverMiddleware(mux)
	socketHandler := api.BearerAuth(*internalToken, recovered)
	tcpHandler := api.TCPDiagnosticsOnly(recovered)

	server := &http.Server{Handler: socketHandler}

	if err := os.MkdirAll(filepath.Dir(*socketPath), 0o700); err != nil {
		slog.Error("failed to create socket parent dir", "path", *socketPath, "error", err)
		os.Exit(1)
	}
	// Remove any stale socket file from a previous crashed run.
	_ = os.Remove(*socketPath)
	listener, err := net.Listen("unix", *socketPath)
	if err != nil {
		slog.Error("failed to listen on socket", "path", *socketPath, "error", err)
		os.Exit(1)
	}
	if err := os.Chmod(*socketPath, 0o600); err != nil {
		slog.Warn("failed to chmod socket", "path", *socketPath, "error", err)
	}

	// Optional second front on TCP, restricted to the read-only diagnostics
	// allowlist (see TCPDiagnosticsOnly) rather than the socket's full route
	// table — not bearer-authenticated (see the handler-chain comment above
	// and bearerAuth's doc comment): reachability is gated purely by which
	// --http-bind addresses actually bound. Each configured address binds
	// best-effort (see listenAll): one that fails is logged and skipped, not
	// fatal, since the address may only be assignable in some deployments.
	// Only a total failure — every address rejected — is fatal, the same
	// reason the socket's is: it would otherwise leave the operator with a
	// silently absent listener and a healthy-looking process.
	httpAddrs := netutil.ListenAddrs(httpBinds, *httpPort)
	tcpServer, err := startMainTCPListener(httpAddrs, *httpTLSCert, *httpTLSKey, tcpHandler)
	if err != nil {
		slog.Error("failed to listen on http port", "addrs", httpAddrs, "error", err)
		os.Exit(1)
	}

	// Tell relay (if present) where to dispatch front-door traffic, then
	// (C9) where to reach the relay-router's socket. Standalone runs are a
	// clean no-op (both calls check relay.Launched() themselves). Run in a
	// goroutine so a slow relay-bridge round-trip doesn't delay the listener
	// accepting traffic — but RegisterModelHost's refusal must still exit the
	// whole process (RegisterModelHostOrExit calls os.Exit(78) from inside
	// this goroutine, which is as fatal to the process as calling it from
	// Main would be): unlike a manifest-dispatch miss, which just means eve
	// can't reach this service through relay yet, a refused model host means
	// relayLLM believes router.sock is relay's upstream when relay actually
	// disagrees — every model call routed through relay would silently 503
	// forever. See model_host.go's registerWithRelay for why this does NOT
	// skip RegisterModelHost just because RegisterManifest failed.
	go registerWithRelay(*dataDir, *socketPath, *internalToken, resolvedRouterSocketPath, os.Exit)

	// Graceful shutdown: drain HTTP requests, then stop managed model servers.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Both fronts drain against the one 5s budget rather than 5s each —
		// they serve the same handler, so a request in flight on either is
		// the same kind of work and the process has one grace period total.
		if tcpServer != nil {
			_ = tcpServer.Shutdown(ctx)
		}
		_ = server.Shutdown(ctx)
		_ = listener.Close()
		_ = os.Remove(*socketPath)
	}()

	slog.Info("listening", "socket", *socketPath)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	// Server stopped — clean up background resources. Managers stop in
	// parallel so shutdown is bounded by one SIGTERM grace period, not one
	// per manager.
	if relayRouter != nil {
		relayRouter.Close()
	}
	var wg sync.WaitGroup
	for _, mgr := range managers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.StopAll()
		}()
	}
	wg.Wait()
	slog.Info("shutdown complete")
}

// warnAliasShadowing logs startup warnings for names the relay-router's
// dispatch order makes unreachable. Bare-alias collisions between managers
// are won by the earlier (higher-priority) manager. OpenAI endpoint models
// are only addressable as "{endpoint}/{id}", so an endpoint name equal to a
// bare alias collides with nothing — only an alias that itself contains "/"
// can intercept an endpoint model id.
func warnAliasShadowing(managers []*servermanager.ServerManager, endpoints []config.OpenAIEndpoint) {
	for i, mgr := range managers {
		for _, alias := range mgr.Aliases() {
			for _, earlier := range managers[:i] {
				if earlier.HasAlias(alias) {
					slog.Warn("router: alias shadowed by a higher-priority manager; model unreachable via the router",
						"alias", alias, "shadowed", mgr.Profile().Kind, "wins", earlier.Profile().Kind)
					break
				}
			}
			if prefix, _, ok := strings.Cut(alias, "/"); ok {
				for _, ep := range endpoints {
					if ep.Name == prefix {
						slog.Warn("router: managed alias would intercept an openai endpoint model id of the same name",
							"alias", alias, "kind", mgr.Profile().Kind, "endpoint", ep.Name)
					}
				}
			}
		}
	}
}

// warnVirtualModelConfig logs startup warnings for virtual-model
// misconfiguration that would otherwise only surface later as a confusing
// runtime 503 (or, worse, silently route to the wrong place). Mirrors
// warnAliasShadowing's dead-config detection, one layer up.
func warnVirtualModelConfig(virtual *config.VirtualLLMConfig, managers []*servermanager.ServerManager, endpoints []config.OpenAIEndpoint) {
	if virtual == nil {
		return
	}
	seenNames := make(map[string]bool)
	for i := range virtual.Models {
		v := &virtual.Models[i]

		if seenNames[v.Name] {
			slog.Warn("router: two virtual models share a name; only the first definition is reachable",
				"name", v.Name)
		}
		seenNames[v.Name] = true

		for _, mgr := range managers {
			if mgr.HasAlias(v.Name) {
				// handleProxy checks managers before virtuals, so this name
				// can never reach the virtual branch.
				slog.Warn("router: virtual model name matches a managed alias; dispatch checks managers first so the virtual is unreachable",
					"name", v.Name, "shadowed-by-kind", mgr.Profile().Kind)
			}
		}
		if strings.Contains(v.Name, "/") {
			// endpoint/id routing parses on the first "/", so a virtual name
			// containing one can intercept it.
			slog.Warn("router: virtual model name contains \"/\"; it can intercept endpoint/id routing",
				"name", v.Name)
		}

		usable := 0
		// Dispatches on classifyVirtualTarget — the same classification
		// candidatesForVirtual uses — rather than a hand-maintained switch of
		// its own, so this validator and the actual routing logic can never
		// disagree about whether a target is usable.
		for _, target := range v.Targets {
			switch router.ClassifyVirtualTarget(target) {
			case router.VirtualTargetEndpoint:
				found := false
				for _, ep := range endpoints {
					if ep.Name == target.Endpoint {
						found = true
						break
					}
				}
				if !found {
					slog.Warn("router: virtual model target names an endpoint absent from openai.endpoints",
						"name", v.Name, "endpoint", target.Endpoint)
					continue
				}
				usable++
			case router.VirtualTargetAlias:
				found := false
				for _, mgr := range managers {
					if mgr.HasAlias(target.Alias) {
						found = true
						break
					}
				}
				if !found {
					slog.Warn("router: virtual model target names an alias no manager has",
						"name", v.Name, "alias", target.Alias)
					continue
				}
				usable++
			default: // virtualTargetInvalid
				switch {
				case target.Endpoint != "" && target.Model == "":
					slog.Warn("router: virtual model target sets endpoint without model",
						"name", v.Name, "endpoint", target.Endpoint)
				case target.Model != "" && target.Endpoint == "" && target.Alias == "":
					slog.Warn("router: virtual model target sets model without endpoint",
						"name", v.Name, "model", target.Model)
				}
				// else: neither shape set at all — nothing specific to warn.
			}
		}
		if usable == 0 {
			slog.Warn("router: virtual model has no usable target; every request for it will fail",
				"name", v.Name)
		}
	}
}

// warnAnthropicModelMap logs startup warnings for router.anthropic.modelMap
// dead config, mirroring warnAliasShadowing/warnVirtualModelConfig above.
// relay_router.go's handleProxy resolves an anthropic modelMap key BEFORE
// every other dispatch check, so a key equal to an existing managed alias or
// endpoint-prefixed id silently shadows it on every route — not just
// /v1/messages — which is worth flagging here rather than as a confusing
// runtime surprise.
func warnAnthropicModelMap(anthropic *config.AnthropicRouterConfig, managers []*servermanager.ServerManager, endpoints []config.OpenAIEndpoint, virtual *config.VirtualLLMConfig) {
	if anthropic == nil || len(anthropic.ModelMap) == 0 {
		return
	}
	for key, target := range anthropic.ModelMap {
		for _, mgr := range managers {
			if mgr.HasAlias(key) {
				slog.Warn("router: anthropic.modelMap key matches an existing managed alias; it will shadow that alias on every route, not just /v1/messages",
					"key", key, "kind", mgr.Profile().Kind)
			}
		}
		if prefix, _, ok := strings.Cut(key, "/"); ok {
			for _, ep := range endpoints {
				if ep.Name == prefix {
					slog.Warn("router: anthropic.modelMap key looks like an endpoint-prefixed model id; it will shadow that route",
						"key", key, "endpoint", ep.Name)
				}
			}
		}

		found := false
		for _, mgr := range managers {
			if mgr.HasAlias(target) {
				found = true
				break
			}
		}
		if !found {
			if prefix, _, ok := strings.Cut(target, "/"); ok {
				for _, ep := range endpoints {
					if ep.Name == prefix {
						found = true
						break
					}
				}
			}
		}
		if !found && virtual != nil && virtual.Find(target) != nil {
			found = true
		}
		if !found {
			slog.Warn("router: anthropic.modelMap target does not match a configured managed alias, virtual model, or openai endpoint",
				"key", key, "target", target)
		}
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// validateHTTPListener checks the --http-port flag group before anything
// binds. An empty port disables the listener entirely and every other flag
// in the group becomes moot, so it returns nil without looking at them —
// the default deployment must reach zero new failure modes.
//
// The --http-port listener carries no bearer token (see bearerAuth's doc
// comment) — it is protected only by --http-bind, deliberately matching
// --router-port's existing unauthenticated-by-bind-address posture, so
// there is no credential-in-transit rationale for forcing TLS on a
// non-loopback bind here. TLS (--http-tls-cert/--http-tls-key) stays
// available for whoever wants the transport encrypted anyway; this check is
// only the ordinary "both flags or neither" pairing sanity check.
func validateHTTPListener(port, certFile, keyFile string) error {
	if port == "" {
		return nil
	}
	if (certFile == "") != (keyFile == "") {
		missing := "--http-tls-cert/RELAY_LLM_HTTP_TLS_CERT"
		if certFile != "" {
			missing = "--http-tls-key/RELAY_LLM_HTTP_TLS_KEY"
		}
		return fmt.Errorf("TLS requires both cert and key; missing %s", missing)
	}
	return nil
}

// startMainTCPListener serves `handler` on every address in addrs that can
// actually be bound, returning the shared server so the caller can shut it
// down. An empty addrs is the disabled case: (nil, nil), nothing bound, no
// goroutine started.
//
// Every bind happens synchronously (via listenAll), on a best-effort
// basis — an address that fails to bind is logged and skipped rather than
// taking the others down with it (see listenAll's doc comment) — while only
// the accept loops run in the background. The returned error means every
// single requested address failed to bind: that's the one case where "the
// listener silently isn't there" must instead surface as a startup failure.
// One *http.Server is shared across every listener: the stdlib supports
// Serve being called on it from multiple goroutines concurrently, and
// Shutdown/Close on that one server tears down every listener it's
// tracking — so a multi-bind front costs no new shutdown machinery over a
// single-bind one.
func startMainTCPListener(addrs []string, certFile, keyFile string, handler http.Handler) (*http.Server, error) {
	if len(addrs) == 0 {
		return nil, nil
	}
	lns, err := netutil.ListenAll(addrs, "http front")
	if err != nil {
		return nil, err
	}
	// Addr is re-read from the first bound listener, not the requested
	// string, so a port-0 request reports the port it actually got.
	srv := &http.Server{Addr: lns[0].Addr().String(), Handler: handler}
	scheme := "http"
	if certFile != "" {
		scheme = "https"
		// Pinned rather than left at the stdlib default, matching the
		// relay-router's listener: a future toolchain lowering that default
		// must not silently loosen this one.
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	for _, ln := range lns {
		slog.Info("listening", "addr", ln.Addr().String(), "scheme", scheme)
	}
	for _, ln := range lns {
		go func() {
			var serveErr error
			if certFile != "" {
				serveErr = srv.ServeTLS(ln, certFile, keyFile)
			} else {
				serveErr = srv.Serve(ln)
			}
			if serveErr != nil && serveErr != http.ErrServerClosed {
				slog.Error("http listener error", "addr", ln.Addr().String(), "error", serveErr)
			}
		}()
	}
	return srv, nil
}
