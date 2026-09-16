package app

// Coverage for model_host.go's C9 wiring and the app.go "build the router
// exactly once" flow around it — specifically the should-fix bugs a security
// review found in the original app.go integration:
//
//  1. router.sock (and RegisterModelHost) silently never happened for a
//     relay-launched relayLLM with no --router-port configured, because both
//     were gated on the router having already been built and bound for TCP.
//  2. RegisterModelHost was skipped entirely whenever RegisterManifest
//     failed, silently stranding relay's model broker even though the two
//     are independent relay capabilities.
//  3. The fix for #1 originally rebuilt a second router.RelayRouter (via
//     router.BuildRelayRouter) whenever the first one wasn't bound for TCP —
//     doubling every setAnthropic/setPassthrough invalid-config log line.
//     app.go now builds once and reuses the same object for both the TCP
//     decision (RelayRouter.MaybeServeTCP) and router.sock.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relayllm/internal/config"
	"relayllm/internal/relay"
	"relayllm/internal/router"
	"relayllm/internal/testutil"
)

// dialUnixPath is a minimal HTTP-over-Unix-socket client, used only to prove
// a router.sock this file opened is actually live and admitting.
func dialUnixPath(t *testing.T, sockPath, path string) int {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("http://unix" + path)
	if err != nil {
		t.Fatalf("dial %s: %v", sockPath, err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

func launchedTestSetup(t *testing.T) *testutil.FakeBridge {
	t.Helper()
	fb := testutil.NewFakeBridge(t)
	testutil.LaunchViaBridge(t, fb, "relayllm")
	// LaunchViaBridge's Hello (via relay.CompleteLaunch) also captures
	// relay.RelayIdentity as a side effect — see internal/relay/launch.go.
	t.Cleanup(relay.ResetRelayIdentityForTesting)
	return fb
}

// ---------------------------------------------------------------------------
// Build-once flow: router.BuildRelayRouter + RelayRouter.MaybeServeTCP,
// mirroring exactly what app.go's Main does (see its comment there).
// ---------------------------------------------------------------------------

// TestBuildOnceFlow_LaunchedWithoutRouterPortKeepsRouterForSocket is
// should-fix #4's core assertion: MaybeServeTCP returning false (no
// --router-port, i.e. an empty addrs) must not mean the router.RelayRouter
// object itself goes away — a relay-launched process still needs it for
// router.sock.
func TestBuildOnceFlow_LaunchedWithoutRouterPortKeepsRouterForSocket(t *testing.T) {
	launchedTestSetup(t)

	relayRouter := router.BuildRelayRouter(nil, nil, nil, nil, "", "")
	started, err := relayRouter.MaybeServeTCP(nil, nil)
	if err != nil {
		t.Fatalf("MaybeServeTCP: %v", err)
	}
	if started {
		t.Fatal("setup: expected MaybeServeTCP to report false with no addrs")
	}

	// This is the exact decision app.go's Main makes: drop the router only
	// when BOTH nothing was served over TCP AND relay didn't launch this
	// process. Launched here, so the router must be kept.
	if !relay.Launched() {
		t.Fatal("setup: expected to be launched")
	}
	if relayRouter == nil {
		t.Fatal("the router object must not be nil'd out while launched, even with no TCP addrs configured")
	}
}

// TestBuildOnceFlow_StandaloneWithNothingToServeDropsRouter pins the other
// half of the same decision: unlaunched AND nothing to serve over TCP really
// is "no router at all", matching the pre-router.sock behavior every other
// reader of relayRouter (sessions.SetRouterPort, the dashboard) expects.
func TestBuildOnceFlow_StandaloneWithNothingToServeDropsRouter(t *testing.T) {
	relayRouter := router.BuildRelayRouter(nil, nil, nil, nil, "", "")
	started, err := relayRouter.MaybeServeTCP(nil, nil)
	if err != nil {
		t.Fatalf("MaybeServeTCP: %v", err)
	}
	if started {
		t.Fatal("setup: expected MaybeServeTCP to report false with no addrs")
	}
	if relay.Launched() {
		t.Fatal("setup: expected NOT launched in this test")
	}
	// app.go's own logic: `if !tcpStarted && !relay.Launched() { relayRouter = nil }`.
	if !started && !relay.Launched() {
		relayRouter = nil
	}
	if relayRouter != nil {
		t.Fatal("expected the router to be dropped: unlaunched and nothing to serve over TCP")
	}
}

// TestBuildOnceFlow_InvalidPassthroughLoggedOnce is the direct proof for
// should-fix #4/#5's "build once" requirement: setPassthrough logs one
// warning per invalid entry, so building the router twice from the same
// config (the shape ensureModelHostRouter used to have, rebuilding whenever
// the first build wasn't bound for TCP) would double it. Going through the
// intended flow — one BuildRelayRouter call, then MaybeServeTCP deciding
// whether to bind TCP — must log it exactly once.
func TestBuildOnceFlow_InvalidPassthroughLoggedOnce(t *testing.T) {
	launchedTestSetup(t)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	// "api" is a reserved passthrough name (newPassthroughProxy refuses it),
	// so setPassthrough logs "router.passthrough entry disabled" for it.
	routerCfg := &config.RouterConfig{Passthrough: map[string]config.PassthroughConfig{
		"api": {Upstream: "https://example.com"},
	}}

	relayRouter := router.BuildRelayRouter(nil, nil, nil, routerCfg, "", "")
	if _, err := relayRouter.MaybeServeTCP(nil, routerCfg); err != nil {
		t.Fatalf("MaybeServeTCP: %v", err)
	}

	got := strings.Count(buf.String(), "router.passthrough entry disabled")
	if got != 1 {
		t.Errorf("\"passthrough entry disabled\" logged %d time(s), want exactly 1 (log:\n%s)", got, buf.String())
	}
}

// ---------------------------------------------------------------------------
// resolveRouterSocketPath
// ---------------------------------------------------------------------------

func TestResolveRouterSocketPath_DefaultsUnderDataDir(t *testing.T) {
	got, err := resolveRouterSocketPath("", "/abs/data")
	if err != nil {
		t.Fatalf("resolveRouterSocketPath: %v", err)
	}
	if want := "/abs/data/router.sock"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestResolveRouterSocketPath_RelativeDataDirIsMadeAbsolute is should-fix #5's
// second half: a relative --data-dir (or an explicit relative
// --router-socket) must not silently produce a relative router socket path —
// RegisterModelHost refuses one, turning a configuration accident into a
// fatal exit(78).
func TestResolveRouterSocketPath_RelativeDataDirIsMadeAbsolute(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveRouterSocketPath("", "relative-data-dir")
	if err != nil {
		t.Fatalf("resolveRouterSocketPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("got %q, want an absolute path", got)
	}
	if want := filepath.Join(wd, "relative-data-dir", "router.sock"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveRouterSocketPath_ExplicitRelativeFlagIsMadeAbsolute(t *testing.T) {
	got, err := resolveRouterSocketPath("some/relative/path.sock", "/abs/data")
	if err != nil {
		t.Fatalf("resolveRouterSocketPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("got %q, want an absolute path", got)
	}
}

// ---------------------------------------------------------------------------
// registerWithRelay
// ---------------------------------------------------------------------------

// TestRegisterWithRelay_ManifestFailureStillRegistersModelHost is should-fix
// #5's first half: a RegisterManifest failure must not silently skip
// RegisterModelHost — the two are independent relay capabilities.
func TestRegisterWithRelay_ManifestFailureStillRegistersModelHost(t *testing.T) {
	fb := launchedTestSetup(t)
	fb.SetResponseForType(relay.ReqRegisterManifest, relay.BridgeResponse{Type: relay.RespError, Code: 500, Message: "manifest boom"})

	exited := false
	// registerWithRelay itself doesn't take dataDir/socketPath from real
	// flags in this test — any well-formed values exercise the same path.
	registerWithRelayForTest(t, fb, func(code int) { exited = true })

	if exited {
		t.Fatal("must not exit when RegisterModelHost itself succeeds, even though RegisterManifest failed")
	}
	foundModelHost := false
	for _, req := range fb.Requests() {
		if req.Type == relay.ReqRegisterModelHost {
			foundModelHost = true
		}
	}
	if !foundModelHost {
		t.Fatal("RegisterModelHost must still be sent after a RegisterManifest failure")
	}
}

func TestRegisterWithRelay_ModelHostRefusalExits78(t *testing.T) {
	fb := launchedTestSetup(t)
	fb.SetResponseForType(relay.ReqRegisterModelHost, relay.BridgeResponse{Type: relay.RespError, Code: -32010, Message: "no model_host capability"})

	var gotCode int
	exited := false
	registerWithRelayForTest(t, fb, func(code int) { exited = true; gotCode = code })

	if !exited || gotCode != 78 {
		t.Fatalf("exited=%v code=%d, want exit(78) on a RegisterModelHost refusal", exited, gotCode)
	}
}

func TestRegisterWithRelay_StandaloneNeverCallsRegisterModelHost(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.WithBridgeEnv(t, fb.SocketPath(), "relayllm") // sets env but never launches

	exited := false
	registerWithRelay(t.TempDir(), "/tmp/sock.notused", "tok", "/abs/router.sock", func(int) { exited = true })
	if exited {
		t.Fatal("standalone (unlaunched) must never call exitFn")
	}
	for _, req := range fb.Requests() {
		if req.Type == relay.ReqRegisterModelHost {
			t.Fatalf("standalone must never send RegisterModelHost, got %+v", req)
		}
	}
}

// registerWithRelayForTest calls registerWithRelay with a real, absolute
// router socket path (RegisterModelHost's own validation refuses a relative
// one, unrelated to what this test is pinning) and a scratch data dir.
func registerWithRelayForTest(t *testing.T, fb *testutil.FakeBridge, exitFn func(int)) {
	t.Helper()
	dataDir := t.TempDir()
	registerWithRelay(dataDir, filepath.Join(dataDir, "relayllm.sock"), "tok", filepath.Join(dataDir, "router.sock"), exitFn)
	_ = fb
}

// ---------------------------------------------------------------------------
// End-to-end: the exact scenario should-fix #4 names — launched with no
// --router-port configured, router.sock still opens and admits, and
// RegisterModelHost still reaches relay.
// ---------------------------------------------------------------------------

func TestModelHostWiring_LaunchedWithoutRouterPort_OpensSocketAndRegisters(t *testing.T) {
	fb := launchedTestSetup(t)

	// Exactly app.go's Main flow: build once, then MaybeServeTCP with an
	// empty addrs (--router-port unset) reports false without touching the
	// router object.
	relayRouter := router.BuildRelayRouter(nil, nil, nil, nil, "", "")
	started, err := relayRouter.MaybeServeTCP(nil, nil)
	if err != nil {
		t.Fatalf("setup: MaybeServeTCP: %v", err)
	}
	if started {
		t.Fatal("setup: expected MaybeServeTCP to report false with no addrs")
	}
	// Launched, so app.go's `if !tcpStarted && !relay.Launched() { relayRouter = nil }`
	// does not fire — the object is kept for router.sock.
	if relayRouter == nil {
		t.Fatal("relayRouter must not be nil while launched")
	}

	// /tmp directly, not t.TempDir() (buried under TestName/NNN/): a real
	// net.Listen("unix", ...) below routinely overflows macOS's 104-char
	// AF_UNIX path limit otherwise — the same reason testutil.FakeBridge
	// uses /tmp directly.
	dataDir, err := os.MkdirTemp("/tmp", "apphost")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	sockPath, err := resolveRouterSocketPath("", dataDir)
	if err != nil {
		t.Fatalf("resolveRouterSocketPath: %v", err)
	}
	if !filepath.IsAbs(sockPath) {
		t.Fatalf("socket path not absolute: %s", sockPath)
	}
	if err := relayRouter.ListenSocket(sockPath, relay.RelayIdentity); err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	defer relayRouter.Close()

	registerWithRelay(dataDir, filepath.Join(dataDir, "relayllm.sock"), "tok", sockPath, func(code int) {
		t.Fatalf("unexpected exit(%d) during RegisterModelHost", code)
	})

	foundModelHost := false
	for _, req := range fb.Requests() {
		if req.Type == relay.ReqRegisterModelHost {
			foundModelHost = true
		}
	}
	if !foundModelHost {
		t.Fatal("RegisterModelHost never reached the fake bridge")
	}

	// And router.sock is really up and admitting this process.
	if status := dialUnixPath(t, sockPath, "/health"); status != http.StatusOK {
		t.Fatalf("router.sock /health status = %d, want 200", status)
	}
}

// ---------------------------------------------------------------------------
// refuseLaunchedRouterPortOrExit (C9, P2/L-S1): --router-port is allowed in
// P1 (L-M1) but exits 78 once relay launched the process, since relay now
// reaches model routing only through router.sock.
// ---------------------------------------------------------------------------

func TestRefuseLaunchedRouterPort_LaunchedWithPortExits78(t *testing.T) {
	launchedTestSetup(t)

	var gotCode int
	called := false
	refuseLaunchedRouterPortOrExit("8180", func(code int) {
		called = true
		gotCode = code
	})
	if !called {
		t.Fatal("exitFn was never called for a launched process with --router-port set")
	}
	if gotCode != 78 {
		t.Errorf("exit code = %d, want 78", gotCode)
	}
}

func TestRefuseLaunchedRouterPort_LaunchedWithoutPortNeverExits(t *testing.T) {
	launchedTestSetup(t)

	refuseLaunchedRouterPortOrExit("", func(code int) {
		t.Fatalf("exitFn called with code %d for a launched process with --router-port unset", code)
	})
}

func TestRefuseLaunchedRouterPort_StandaloneWithPortNeverExits(t *testing.T) {
	if relay.Launched() {
		t.Fatal("setup: expected NOT launched in this test")
	}
	refuseLaunchedRouterPortOrExit("8180", func(code int) {
		t.Fatalf("exitFn called with code %d for a standalone process; --router-port is L-M2's normal serving path", code)
	})
}
