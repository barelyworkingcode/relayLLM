package spawn

import (
	"encoding/json"
	"strings"
	"testing"

	"relayllm/internal/relay"
	"relayllm/internal/testutil"
	"relayllm/internal/types"
)

func envHasKey(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// ChildBaseEnv must strip ALL of relayLLM's relay credentials — including any
// inherited project-token value — so nothing leaks into a child we don't
// explicitly inject for. The correct per-child project token is added back via
// SetProjectTokenEnv after ChildBaseEnv.
func TestChildBaseEnv_StripsRelaySecrets(t *testing.T) {
	t.Setenv(relay.EnvServiceToken, "svc-secret")
	t.Setenv(relay.EnvServiceTokenLegacy, "mcp-secret")
	t.Setenv(relay.EnvFrontendToken, "frontend-secret")
	t.Setenv(relay.EnvProjectToken, "stale-project")
	t.Setenv(relay.EnvProjectTokenLegacy, "stale-legacy-project")

	env := ChildBaseEnv()
	for _, k := range []string{relay.EnvServiceToken, relay.EnvServiceTokenLegacy, relay.EnvFrontendToken, relay.EnvProjectToken, relay.EnvProjectTokenLegacy} {
		if envHasKey(env, k) {
			t.Errorf("%s must be stripped from child base env", k)
		}
	}
	if !envHasKey(env, "PATH") {
		t.Error("non-relay env (PATH) must be preserved")
	}
}

// SetProjectTokenEnv writes the project token under both the current and legacy
// names (transition shim) and no-ops on empty.
func TestSetProjectTokenEnv(t *testing.T) {
	got := SetProjectTokenEnv(nil, "tok123")
	if !envHasKey(got, relay.EnvProjectToken) || !envHasKey(got, relay.EnvProjectTokenLegacy) {
		t.Errorf("expected both project-token names set, got %v", got)
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, relay.EnvProjectToken+"=") && kv != relay.EnvProjectToken+"=tok123" {
			t.Errorf("wrong value: %q", kv)
		}
	}
	if n := len(SetProjectTokenEnv(nil, "")); n != 0 {
		t.Errorf("empty token must be a no-op, got %d entries", n)
	}
}

// A non-empty ProjectID makes a spawn project-scoped: relay is consulted and a
// project-scoped token is returned, even though UseRelayToken is false (the
// new default for project shells). The request must carry the project id.
func TestRelayManagedSpec_ProjectIDIsManaged(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	data, _ := json.Marshal(relay.RelayPtyEnvResponse{RelayToken: "scoped-tok", WorkingDir: "/proj"})
	fb.SetResponse(relay.BridgeResponse{Type: relay.RespPtyEnv, Data: data})
	testutil.WithBridgeEnv(t, fb.SocketPath(), "relay-llm", "svc-token")

	spec := RelayManagedSpec{ProjectID: "proj-1", Directory: "/proj"}
	subs, err := spec.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if subs.RelayToken != "scoped-tok" {
		t.Errorf("RelayToken = %q, want scoped-tok", subs.RelayToken)
	}

	reqs := fb.Requests()
	if len(reqs) != 1 {
		t.Fatalf("bridge requests = %d, want 1", len(reqs))
	}
	var got relay.RelayPtyEnvRequest
	if err := json.Unmarshal(reqs[0].Arguments, &got); err != nil {
		t.Fatalf("decode args: %v", err)
	}
	if got.ProjectID != "proj-1" {
		t.Errorf("request project_id = %q, want proj-1", got.ProjectID)
	}
}

// An ad-hoc terminal (no ProjectID, no UseRelayToken, no regen) must NOT contact
// the bridge and must get no token.
func TestRelayManagedSpec_AdHocNoToken(t *testing.T) {
	spec := RelayManagedSpec{Directory: "/tmp/scratch"}
	subs, err := spec.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if subs.RelayToken != "" {
		t.Errorf("ad-hoc terminal got a token: %q", subs.RelayToken)
	}
	if subs.ProjectPath != "/tmp/scratch" {
		t.Errorf("ProjectPath = %q, want /tmp/scratch", subs.ProjectPath)
	}
}

// ResolveProjectToken resolves from relay's bridge by the session's project id.
func TestResolveProjectToken_FromBridge(t *testing.T) {
	fb := testutil.NewFakeBridge(t)
	data, _ := json.Marshal(relay.RelayPtyEnvResponse{RelayToken: "jit-tok", WorkingDir: "/proj"})
	fb.SetResponse(relay.BridgeResponse{Type: relay.RespPtyEnv, Data: data})
	testutil.WithBridgeEnv(t, fb.SocketPath(), "relay-llm", "svc-token")

	got := ResolveProjectToken(&types.Session{ID: "s1", ProjectID: "proj-9", Directory: "/proj"})
	if got != "jit-tok" {
		t.Errorf("token = %q, want jit-tok", got)
	}
	reqs := fb.Requests()
	if len(reqs) != 1 {
		t.Fatalf("bridge requests = %d, want 1", len(reqs))
	}
	var req relay.RelayPtyEnvRequest
	_ = json.Unmarshal(reqs[0].Arguments, &req)
	if req.ProjectID != "proj-9" {
		t.Errorf("request project_id = %q, want proj-9", req.ProjectID)
	}
}

// Standalone relayLLM (no service token in env) never dials the bridge and never
// escalates to a service token — it returns empty so callers fail closed.
func TestResolveProjectToken_StandaloneEmpty(t *testing.T) {
	t.Setenv(relay.EnvServiceToken, "")
	t.Setenv(relay.EnvServiceTokenLegacy, "")
	if got := ResolveProjectToken(&types.Session{ID: "s1", ProjectID: "proj-1"}); got != "" {
		t.Errorf("token = %q, want empty", got)
	}
}

// Claude's own child env for a host spawn (ChildBaseEnv, no EnsurePath/token
// injection) must not carry any relay secret either — belt and suspenders
// alongside the host-exec argv guard in security_regression_test.go, since
// the env is what a leaked debug log would actually dump.
func TestSec_HostSpawn_ChildBaseEnvHasNoRelaySecrets(t *testing.T) {
	t.Setenv(relay.EnvServiceToken, "svc-secret")
	t.Setenv(relay.EnvProjectToken, "proj-secret")
	t.Setenv(relay.EnvProjectTokenLegacy, "proj-secret-legacy")

	env := ChildBaseEnv()
	for _, k := range []string{relay.EnvServiceToken, relay.EnvServiceTokenLegacy, relay.EnvFrontendToken, relay.EnvProjectToken, relay.EnvProjectTokenLegacy} {
		if envHasKey(env, k) {
			t.Errorf("host spawn child env leaked %s", k)
		}
	}
}
