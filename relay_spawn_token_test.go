package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func envHasKey(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// childBaseEnv must strip ALL of relayLLM's relay credentials — including any
// inherited project-token value — so nothing leaks into a child we don't
// explicitly inject for. The correct per-child project token is added back via
// setProjectTokenEnv after childBaseEnv.
func TestChildBaseEnv_StripsRelaySecrets(t *testing.T) {
	t.Setenv(envServiceToken, "svc-secret")
	t.Setenv(envServiceTokenLegacy, "mcp-secret")
	t.Setenv(envFrontendToken, "frontend-secret")
	t.Setenv(envProjectToken, "stale-project")
	t.Setenv(envProjectTokenLegacy, "stale-legacy-project")

	env := childBaseEnv()
	for _, k := range []string{envServiceToken, envServiceTokenLegacy, envFrontendToken, envProjectToken, envProjectTokenLegacy} {
		if envHasKey(env, k) {
			t.Errorf("%s must be stripped from child base env", k)
		}
	}
	if !envHasKey(env, "PATH") {
		t.Error("non-relay env (PATH) must be preserved")
	}
}

// setProjectTokenEnv writes the project token under both the current and legacy
// names (transition shim) and no-ops on empty.
func TestSetProjectTokenEnv(t *testing.T) {
	got := setProjectTokenEnv(nil, "tok123")
	if !envHasKey(got, envProjectToken) || !envHasKey(got, envProjectTokenLegacy) {
		t.Errorf("expected both project-token names set, got %v", got)
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, envProjectToken+"=") && kv != envProjectToken+"=tok123" {
			t.Errorf("wrong value: %q", kv)
		}
	}
	if n := len(setProjectTokenEnv(nil, "")); n != 0 {
		t.Errorf("empty token must be a no-op, got %d entries", n)
	}
}

// A non-empty ProjectID makes a spawn project-scoped: relay is consulted and a
// project-scoped token is returned, even though UseRelayToken is false (the
// new default for project shells). The request must carry the project id.
func TestRelayManagedSpec_ProjectIDIsManaged(t *testing.T) {
	fb := NewFakeBridge(t)
	data, _ := json.Marshal(RelayPtyEnvResponse{RelayToken: "scoped-tok", WorkingDir: "/proj"})
	fb.SetResponse(relayBridgeResponse{Type: respPtyEnv, Data: data})
	withBridgeEnv(t, fb.SocketPath(), "relay-llm", "svc-token")

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
	var got RelayPtyEnvRequest
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

// resolveProjectToken resolves from relay's bridge by the session's project id.
func TestResolveProjectToken_FromBridge(t *testing.T) {
	fb := NewFakeBridge(t)
	data, _ := json.Marshal(RelayPtyEnvResponse{RelayToken: "jit-tok", WorkingDir: "/proj"})
	fb.SetResponse(relayBridgeResponse{Type: respPtyEnv, Data: data})
	withBridgeEnv(t, fb.SocketPath(), "relay-llm", "svc-token")

	got := resolveProjectToken(&Session{ID: "s1", ProjectID: "proj-9", Directory: "/proj"})
	if got != "jit-tok" {
		t.Errorf("token = %q, want jit-tok", got)
	}
	reqs := fb.Requests()
	if len(reqs) != 1 {
		t.Fatalf("bridge requests = %d, want 1", len(reqs))
	}
	var req RelayPtyEnvRequest
	_ = json.Unmarshal(reqs[0].Arguments, &req)
	if req.ProjectID != "proj-9" {
		t.Errorf("request project_id = %q, want proj-9", req.ProjectID)
	}
}

// Standalone relayLLM (no service token in env) never dials the bridge and never
// escalates to a service token — it returns empty so callers fail closed.
func TestResolveProjectToken_StandaloneEmpty(t *testing.T) {
	t.Setenv(envServiceToken, "")
	t.Setenv(envServiceTokenLegacy, "")
	if got := resolveProjectToken(&Session{ID: "s1", ProjectID: "proj-1"}); got != "" {
		t.Errorf("token = %q, want empty", got)
	}
}
