package relay_test

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"relayllm/internal/relay"
	"relayllm/internal/testutil"
)

var validSecret = strings.Repeat("0123456789abcdef", 4)

func assertClosed(t *testing.T, f *os.File) {
	t.Helper()
	if _, err := f.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Errorf("launch fd not closed: read err = %v", err)
	}
}

func TestReadLaunchSecret_Valid(t *testing.T) {
	r := testutil.LaunchPipe(t, validSecret)
	got, err := relay.ReadLaunchSecret(r)
	if err != nil {
		t.Fatalf("ReadLaunchSecret: %v", err)
	}
	if got != validSecret {
		t.Errorf("secret = %q, want %q", got, validSecret)
	}
	assertClosed(t, r)
}

func TestReadLaunchSecret_Malformed(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"short":            validSecret[:63],
		"long":             validSecret + "a",
		"trailing newline": validSecret + "\n",
		"uppercase":        strings.ToUpper(validSecret),
		"non-hex":          strings.Repeat("g", 64),
		"oversized":        strings.Repeat("a", 4096),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			r := testutil.LaunchPipe(t, content)
			_, err := relay.ReadLaunchSecret(r)
			if err == nil {
				t.Fatal("want error")
			}
			if len(content) >= 8 && strings.Contains(err.Error(), content[:8]) {
				t.Errorf("error leaks secret material: %v", err)
			}
			assertClosed(t, r)
		})
	}
}

func TestHello_Success(t *testing.T) {
	t.Cleanup(relay.ResetRelayIdentityForTesting)
	fb := testutil.NewFakeBridge(t)
	res, err := relay.Hello(fb.SocketPath(), "relay-llm", validSecret)
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if res.ServiceID != "relay-llm" || res.RelayPID != os.Getpid() {
		t.Errorf("result = %+v", res)
	}
	hellos := fb.Hellos()
	if len(hellos) != 1 {
		t.Fatalf("hellos = %d, want 1", len(hellos))
	}
	if h := hellos[0]; h.Type != relay.ReqHello || h.Name != "relay-llm" || h.Token != validSecret || len(h.Arguments) != 0 {
		t.Errorf("hello request = %+v", h)
	}

	// C9/SP1: FakeBridge's Hello handler runs in this same test process, so
	// the connecting end's LOCAL_PEERTOKEN (read by Hello) names THIS
	// process — exactly the self-dial shape spike SP1 measured. RelayIdentity
	// must reflect it once Hello has returned.
	proc, ok := relay.RelayIdentity()
	if !ok {
		t.Fatal("RelayIdentity() not set after a successful Hello")
	}
	if int(proc.PID) != os.Getpid() {
		t.Errorf("captured identity pid = %d, want %d", proc.PID, os.Getpid())
	}
	if proc.PIDVersion == 0 {
		t.Error("captured identity pidversion is zero; the peer token read is wrong")
	}
}

// TestHello_PeerTokenPidMismatchFailsClosed pins C9's cross-check: the OK
// reply's relay_pid must agree with the kernel's own account of who answered
// the connection (spike SP1), or Hello fails and no identity is stored — a
// process able to forge the JSON reply must not be able to forge the token.
func TestHello_PeerTokenPidMismatchFailsClosed(t *testing.T) {
	t.Cleanup(relay.ResetRelayIdentityForTesting)
	fb := testutil.NewFakeBridge(t)
	// FakeBridge is this test process, so the real peer token's pid is
	// os.Getpid() — scripting any other relay_pid manufactures exactly the
	// disagreement Hello must refuse to trust.
	other := os.Getpid() + 1
	raw, _ := json.Marshal(relay.HelloResult{ServiceID: "relay-llm", RelayPID: other})
	fb.SetHelloResponse(relay.BridgeResponse{Type: relay.RespOK, Data: raw})

	if _, err := relay.Hello(fb.SocketPath(), "relay-llm", validSecret); err == nil {
		t.Fatal("want error on a relay_pid / peer-token mismatch")
	}
	if _, ok := relay.RelayIdentity(); ok {
		t.Error("RelayIdentity() must stay unset after a failed Hello")
	}
}

func TestHello_ErrorFrame(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	fb.SetHelloResponse(relay.BridgeResponse{Type: relay.RespError, Code: -32001, Message: "bad secret " + validSecret})
	_, err := relay.Hello(fb.SocketPath(), "relay-llm", validSecret)
	if err == nil {
		t.Fatal("want error on Error frame")
	}
	if strings.Contains(err.Error(), validSecret) {
		t.Errorf("error leaks secret: %v", err)
	}
	if !strings.Contains(err.Error(), "-32001") {
		t.Errorf("error should carry the code: %v", err)
	}
}

func TestHello_RejectsMismatchedOrMissingIdentity(t *testing.T) {
	for name, data := range map[string]relay.HelloResult{
		"other service": {ServiceID: "eve", RelayPID: 42},
		"no relay pid":  {ServiceID: "relay-llm"},
	} {
		t.Run(name, func(t *testing.T) {
			fb := testutil.NewFakeBridge(t)
			raw, _ := json.Marshal(data)
			fb.SetHelloResponse(relay.BridgeResponse{Type: relay.RespOK, Data: raw})
			if _, err := relay.Hello(fb.SocketPath(), "relay-llm", validSecret); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestCompleteLaunch_MalformedSecretNeverDialsAndStaysStandalone(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.WithBridgeEnv(t, fb.SocketPath(), "relay-llm")
	r := testutil.LaunchPipe(t, "not-a-secret")
	if _, err := relay.CompleteLaunch(r); err == nil {
		t.Fatal("want error")
	}
	if relay.Launched() || len(fb.Hellos()) != 0 {
		t.Errorf("launched=%v hellos=%d; want standalone and no dial", relay.Launched(), len(fb.Hellos()))
	}
}

func TestCompleteLaunch_HelloFailureStaysStandalone(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	fb.SetHelloResponse(relay.BridgeResponse{Type: relay.RespError, Code: -32001, Message: "unauthorized"})
	testutil.WithBridgeEnv(t, fb.SocketPath(), "relay-llm")
	if _, err := relay.CompleteLaunch(testutil.LaunchPipe(t, validSecret)); err == nil {
		t.Fatal("want error")
	}
	if relay.Launched() {
		t.Error("Launched() true after failed Hello")
	}
}

func TestCompleteLaunch_MissingServiceIDClosesFdAndFails(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.WithBridgeEnv(t, fb.SocketPath(), "")
	r := testutil.LaunchPipe(t, validSecret)
	if _, err := relay.CompleteLaunch(r); err == nil {
		t.Fatal("want error")
	}
	assertClosed(t, r)
	if relay.Launched() {
		t.Error("Launched() true without service id")
	}
}

// After a launch, every bridge request carries an empty token; before it,
// SendBridgeRequest refuses without dialing.
func TestSendBridgeRequest_EmptyTokenOnlyAfterLaunch(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.WithBridgeEnv(t, fb.SocketPath(), "relay-llm")
	if _, err := relay.SendBridgeRequest("SomeRequestType", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unlaunched SendBridgeRequest must fail")
	}
	if len(fb.Requests()) != 0 {
		t.Fatal("unlaunched SendBridgeRequest must not dial")
	}

	testutil.LaunchViaBridge(t, fb, "relay-llm")
	for _, typ := range []string{relay.ReqRegisterManifest, relay.ReqRegisterModelHost, "ListProjects", "GetProject"} {
		if _, err := relay.SendBridgeRequest(typ, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
	}
	for _, req := range fb.Requests() {
		if req.Token != "" || req.Name != "" {
			t.Errorf("%s carried token=%q name=%q; want both empty", req.Type, req.Token, req.Name)
		}
	}
}

func TestBootstrapLaunch_UnsetIsStandalone(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.WithBridgeEnv(t, fb.SocketPath(), "relay-llm")
	t.Setenv(relay.EnvServiceToken, "stale")
	t.Setenv(relay.EnvServiceTokenLegacy, "stale")
	t.Setenv(relay.EnvFrontendToken, "stale")
	os.Unsetenv(relay.EnvLaunchFD)

	launched, _, err := relay.BootstrapLaunch()
	if err != nil || launched || relay.Launched() {
		t.Fatalf("launched=%v err=%v; want standalone", launched, err)
	}
	if len(fb.Hellos()) != 0 {
		t.Error("standalone bootstrap must not dial the bridge")
	}
	for _, k := range []string{relay.EnvServiceToken, relay.EnvServiceTokenLegacy, relay.EnvFrontendToken} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s not scrubbed from own environment", k)
		}
	}
}

// dupFd hands BootstrapLaunch its own descriptor so closing it never touches
// the *os.File the test still owns.
func dupFd(t *testing.T, f *os.File) int {
	t.Helper()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	return fd
}

func TestBootstrapLaunch_ReadsFdAndHellos(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	testutil.WithBridgeEnv(t, fb.SocketPath(), "relay-llm")
	fd := dupFd(t, testutil.LaunchPipe(t, validSecret))
	t.Setenv(relay.EnvLaunchFD, strconv.Itoa(fd))

	launched, res, err := relay.BootstrapLaunch()
	if err != nil || !launched || !relay.Launched() {
		t.Fatalf("launched=%v err=%v", launched, err)
	}
	if res.ServiceID != "relay-llm" {
		t.Errorf("result = %+v", res)
	}
	if _, ok := os.LookupEnv(relay.EnvLaunchFD); ok {
		t.Errorf("%s still in own environment after bootstrap", relay.EnvLaunchFD)
	}
	if err := syscall.Fstat(fd, &syscall.Stat_t{}); err == nil {
		t.Errorf("launch fd %d still open after bootstrap", fd)
	}
}

func TestBootstrapLaunch_FailsClosed(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	cases := map[string]func(t *testing.T) string{
		"empty":     func(*testing.T) string { return "" },
		"not a num": func(*testing.T) string { return "three" },
		"stdin":     func(*testing.T) string { return "0" },
		"bad secret": func(t *testing.T) string {
			return strconv.Itoa(dupFd(t, testutil.LaunchPipe(t, "short")))
		},
	}
	for name, fdValue := range cases {
		t.Run(name, func(t *testing.T) {
			testutil.WithBridgeEnv(t, fb.SocketPath(), "relay-llm")
			t.Setenv(relay.EnvLaunchFD, fdValue(t))
			launched, _, err := relay.BootstrapLaunch()
			if err == nil || launched || relay.Launched() {
				t.Fatalf("launched=%v err=%v; want failure", launched, err)
			}
		})
	}
	if len(fb.Hellos()) != 0 {
		t.Errorf("hellos = %d; a failed read must never reach the bridge", len(fb.Hellos()))
	}
}
