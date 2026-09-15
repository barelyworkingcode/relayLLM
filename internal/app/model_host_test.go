package app

// Coverage for model_host.go's C9 wiring — specifically the two should-fix
// bugs a security review found in the original app.go integration:
//
//  1. router.sock (and RegisterModelHost) silently never happened for a
//     relay-launched relayLLM with no --router-port configured, because both
//     were gated on StartRelayRouter's return value being non-nil.
//  2. RegisterModelHost was skipped entirely whenever RegisterManifest
//     failed, silently stranding relay's model broker even though the two
//     are independent relay capabilities.

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

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
// ensureModelHostRouter
// ---------------------------------------------------------------------------

func TestEnsureModelHostRouter_StandaloneReturnsNilUnchanged(t *testing.T) {
	// Not launched: relay.Launched() is false by default in a fresh test
	// process/without LaunchViaBridge, so this must be a no-op.
	got := ensureModelHostRouter(nil, nil, nil, nil, nil, "", "")
	if got != nil {
		t.Fatalf("got %v, want nil for a standalone (non-launched) process", got)
	}
}

func TestEnsureModelHostRouter_AlreadyBuiltIsReturnedUnchanged(t *testing.T) {
	launchedTestSetup(t)
	existing := router.NewRelayRouter(":0", nil, nil, nil)
	got := ensureModelHostRouter(existing, nil, nil, nil, nil, "", "")
	if got != existing {
		t.Fatalf("ensureModelHostRouter replaced an already-built router")
	}
}

// TestEnsureModelHostRouter_LaunchedBuildsRouterWithoutRouterPort is should-fix
// #4's core assertion: StartRelayRouter returns nil when --router-port is
// unset (len(routerAddrs) == 0) — a relay-launched process must still get a
// router object to serve router.sock on.
func TestEnsureModelHostRouter_LaunchedBuildsRouterWithoutRouterPort(t *testing.T) {
	launchedTestSetup(t)

	// Mirrors what app.go's Main does with an empty --router-port: routerAddrs
	// ends up empty, and StartRelayRouter's own early return kicks in.
	nilFromStart, err := router.StartRelayRouter(nil, nil, nil, nil, nil, "", "")
	if err != nil {
		t.Fatalf("setup: StartRelayRouter: %v", err)
	}
	if nilFromStart != nil {
		t.Fatal("setup: expected StartRelayRouter to return nil with no addrs")
	}

	got := ensureModelHostRouter(nilFromStart, nil, nil, nil, nil, "", "")
	if got == nil {
		t.Fatal("ensureModelHostRouter must build a router when launched, even with no TCP addrs configured")
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

	// routerAddrs empty, exactly like Main() with --router-port unset.
	relayRouter, err := router.StartRelayRouter(nil, nil, nil, nil, nil, "", "")
	if err != nil {
		t.Fatalf("setup: StartRelayRouter: %v", err)
	}
	relayRouter = ensureModelHostRouter(relayRouter, nil, nil, nil, nil, "", "")
	if relayRouter == nil {
		t.Fatal("ensureModelHostRouter returned nil while launched")
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
