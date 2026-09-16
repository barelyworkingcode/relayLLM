package relay_test

// Coverage for the relay-facing bridge handshake. Validates:
//   - BuildManifest() returns the expected route table (golden).
//   - MaybeRegisterManifest is a no-op when RELAY_BRIDGE_SOCKET is unset.
//   - With env wired and a FakeBridge running, it sends a well-formed
//     RegisterManifest request carrying the right service id + socket + token.
//
// This is an external test package (relay_test, not relay) because it needs
// testutil.FakeBridge/WithBridgeEnv, and testutil imports relay — an
// in-package relay test importing testutil would be an import cycle.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relayllm/internal/relay"
	"relayllm/internal/testutil"
)

// ---------------------------------------------------------------------------
// BuildManifest golden
// ---------------------------------------------------------------------------

func TestManifest_BuildManifest_HasExpectedRoutes(t *testing.T) {
	dataDir := t.TempDir()
	m := relay.BuildManifest(dataDir)

	// Routes the relay dispatcher must know about. relayLLM only hosts
	// models — session/terminal/permission/generated/ws routes are
	// relay-sessions' territory. Adding a route is a protocol change —
	// break this test deliberately when adding one.
	wantRoutes := []string{
		"/api/status",
		"/api/status/detailed",
		"/api/llama/",
		"/api/mlx/",
	}
	if len(m.Routes) != len(wantRoutes) {
		t.Fatalf("route count: got %d, want %d (%v)", len(m.Routes), len(wantRoutes), m.Routes)
	}
	for _, want := range wantRoutes {
		found := false
		for _, got := range m.Routes {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing route %q in manifest", want)
		}
	}
	if m.Status == nil || m.Status.Path != "/api/status" {
		t.Errorf("status: got %+v, want path=/api/status", m.Status)
	}

	// Config rides on registration so relay can render the settings.json editor.
	if m.Config == nil {
		t.Fatalf("manifest has no Config — relay can't surface the settings editor")
	}
	if want := filepath.Join(dataDir, "settings.json"); m.Config.Path != want {
		t.Errorf("config.path: got %q, want %q", m.Config.Path, want)
	}
	if m.Config.ApplyMode != "restart" {
		t.Errorf("config.applyMode: got %q, want restart", m.Config.ApplyMode)
	}
	if len(m.Config.Schema) == 0 {
		t.Errorf("config.schema is empty — the editor would render nothing")
	}

	// Actions are part of the wire contract: the relay UI builds buttons
	// from this declaration. Adding/removing one is a coordinated change.
	wantActions := map[string]relay.ActionDecl{
		"stop-llama": {
			ID: "stop-llama", Label: "Stop", Method: "DELETE",
			PathTemplate: "/api/llama/instances/{alias}", ForEach: "instances",
		},
		"stop-mlx": {
			ID: "stop-mlx", Label: "Stop", Method: "DELETE",
			PathTemplate: "/api/mlx/instances/{alias}", ForEach: "mlxInstances",
		},
	}
	if len(m.Actions) != len(wantActions) {
		t.Fatalf("Actions count: got %d (%+v), want %d", len(m.Actions), m.Actions, len(wantActions))
	}
	for _, got := range m.Actions {
		want, ok := wantActions[got.ID]
		if !ok {
			t.Errorf("unexpected action %q", got.ID)
			continue
		}
		if got != want {
			t.Errorf("action %q: got %+v, want %+v", got.ID, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// MaybeRegisterManifest
// ---------------------------------------------------------------------------

func TestManifest_MaybeRegister_StandaloneMode_IsNoOp(t *testing.T) {
	bridge := testutil.NewFakeBridge(t)
	// Deliberately do NOT set EnvBridgeSocket. Service ID/token presence
	// shouldn't matter — standalone mode short-circuits before reading them.
	_ = os.Unsetenv(relay.EnvBridgeSocket)

	relay.MaybeRegisterManifest(t.TempDir(), "/tmp/some.sock", "tok123")

	if len(bridge.Requests()) != 0 {
		t.Errorf("standalone mode sent %d requests; want 0", len(bridge.Requests()))
	}
}

func TestManifest_MaybeRegister_SendsCorrectPayload(t *testing.T) {
	bridge := testutil.NewFakeBridge(t)
	testutil.LaunchViaBridge(t, bridge, "relayllm-test")

	relay.MaybeRegisterManifest(t.TempDir(), "/tmp/internal.sock", "internal-token-abc")

	// Bridge accepts and replies are async via goroutine; wait briefly.
	testutil.WaitFor(t, 1*time.Second, func() bool { return len(bridge.Requests()) >= 1 })

	reqs := bridge.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	req := reqs[0]
	if req.Type != relay.ReqRegisterManifest {
		t.Errorf("Type: got %q, want %q", req.Type, relay.ReqRegisterManifest)
	}
	if req.Token != "" {
		t.Errorf("Token: got %q, want empty (relay authenticates by peer identity)", req.Token)
	}
	var args relay.RegisterManifestRequest
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		t.Fatalf("decode Arguments: %v", err)
	}
	if args.ServiceID != "relayllm-test" {
		t.Errorf("ServiceID: got %q, want %q", args.ServiceID, "relayllm-test")
	}
	if args.InternalSocket != "/tmp/internal.sock" {
		t.Errorf("InternalSocket: got %q", args.InternalSocket)
	}
	if args.InternalToken != "internal-token-abc" {
		t.Errorf("InternalToken: got %q", args.InternalToken)
	}
	if len(args.Manifest.Routes) == 0 {
		t.Errorf("Manifest.Routes empty: %+v", args.Manifest)
	}
	// Actions ride along on registration — relay needs them to render
	// buttons and to validate dispatch requests against the whitelist.
	if len(args.Manifest.Actions) == 0 {
		t.Errorf("Manifest.Actions empty — relay can't render action buttons: %+v", args.Manifest)
	}
}

func TestManifest_MaybeRegister_BridgeEnvWithoutLaunch_IsNoOp(t *testing.T) {
	bridge := testutil.NewFakeBridge(t)
	// Bridge env present but no Hello: not launched, so no identity to use.
	testutil.WithBridgeEnv(t, bridge.SocketPath(), "relayllm-test")

	relay.MaybeRegisterManifest(t.TempDir(), "/tmp/x.sock", "internal")

	// MaybeRegisterManifest is synchronous; if it dialed at all the request
	// would already be on the bridge by the time we return here.
	if len(bridge.Requests()) != 0 {
		t.Errorf("unlaunched process should skip registration; got %d requests", len(bridge.Requests()))
	}
}

func TestManifest_MaybeRegister_BridgeErrorIsSwallowed(t *testing.T) {
	bridge := testutil.NewFakeBridge(t)
	bridge.SetResponse(relay.BridgeResponse{
		Type:    relay.RespError,
		Code:    500,
		Message: "internal error",
	})
	testutil.LaunchViaBridge(t, bridge, "relayllm-test")

	// The contract: registration failure logs + continues. The test asserts
	// "doesn't panic" — if MaybeRegisterManifest ever started propagating
	// errors the call site (a goroutine in main) would silently exit and
	// future debugging would be confused.
	relay.MaybeRegisterManifest(t.TempDir(), "/tmp/x.sock", "internal")

	testutil.WaitFor(t, 1*time.Second, func() bool { return len(bridge.Requests()) >= 1 })
	// One request was sent and bridge replied Error — relayLLM should not
	// have retried.
	if len(bridge.Requests()) != 1 {
		t.Errorf("expected exactly 1 request on bridge error; got %d", len(bridge.Requests()))
	}
}
