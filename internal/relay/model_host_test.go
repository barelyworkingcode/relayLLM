package relay_test

// Coverage for C9's RegisterModelHost wiring:
//   - the wire payload is snake_case (service_id/router_socket), matching
//     relay's actual bridge.RegisterModelHostRequest;
//   - a relative router socket path is refused before ever dialing;
//   - a refusal from relay (bridge Error frame) is surfaced as an error, and
//     RegisterModelHostOrExit turns that into an injected exit(78) rather
//     than terminating the test process.

import (
	"strings"
	"testing"

	"relayllm/internal/relay"
	"relayllm/internal/testutil"
)

func TestRegisterModelHost_WireIsSnakeCaseWithAbsolutePath(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.LaunchViaBridge(t, fb, "relayllm")

	const sock = "/Users/admin/Library/Application Support/relayLLM/router.sock"
	if err := relay.RegisterModelHost(sock); err != nil {
		t.Fatalf("RegisterModelHost: %v", err)
	}

	reqs := fb.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Type != relay.ReqRegisterModelHost {
		t.Fatalf("type = %q, want %q", req.Type, relay.ReqRegisterModelHost)
	}
	raw := string(req.Arguments)
	if !strings.Contains(raw, `"service_id":"relayllm"`) {
		t.Errorf("arguments missing snake_case service_id: %s", raw)
	}
	if !strings.Contains(raw, `"router_socket":`) || strings.Contains(raw, `"routerSocket"`) {
		t.Errorf("arguments missing snake_case router_socket (or carry the camelCase spelling): %s", raw)
	}
	if !strings.Contains(raw, sock) {
		t.Errorf("arguments do not carry the absolute path verbatim: %s", raw)
	}
}

func TestRegisterModelHost_RelativePathRefusedBeforeDialing(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.LaunchViaBridge(t, fb, "relayllm")

	if err := relay.RegisterModelHost("relative/router.sock"); err == nil {
		t.Fatal("want error for a relative router socket path")
	}
	if len(fb.Requests()) != 0 {
		t.Error("a relative path must be rejected before dialing the bridge at all")
	}
}

func TestRegisterModelHost_RefusalIsAnError(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.LaunchViaBridge(t, fb, "relayllm")
	fb.SetResponse(relay.BridgeResponse{Type: relay.RespError, Code: -32010, Message: "service does not hold model_host"})

	if err := relay.RegisterModelHost("/abs/router.sock"); err == nil {
		t.Fatal("want error when relay refuses the registration")
	}
}

func TestRegisterModelHostOrExit_RefusalCallsExit78(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.LaunchViaBridge(t, fb, "relayllm")
	fb.SetResponse(relay.BridgeResponse{Type: relay.RespError, Code: -32010, Message: "service does not hold model_host"})

	var gotCode int
	called := false
	relay.RegisterModelHostOrExit("/abs/router.sock", func(code int) {
		called = true
		gotCode = code
	})
	if !called {
		t.Fatal("exitFn was never called on a refusal")
	}
	if gotCode != 78 {
		t.Errorf("exit code = %d, want 78", gotCode)
	}
}

func TestRegisterModelHostOrExit_SuccessNeverExits(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.LaunchViaBridge(t, fb, "relayllm")

	relay.RegisterModelHostOrExit("/abs/router.sock", func(code int) {
		t.Fatalf("exitFn called with code %d on a successful registration", code)
	})
}
