package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHookBinary_EndToEnd_ScopedTokenGetsDecision builds the real hook
// binary and runs it against a fake permission server dialed over a real
// Unix socket, exactly as relayLLM's own /api/permission does. It proves
// the hook's half of the scoped-token contract: whatever RELAY_LLM_HOOK_TOKEN
// holds — the hook has no idea, and must not care, whether it is relayLLM's
// old internal bearer or a per-session credential — goes out verbatim as
// the Bearer header, and the JSON decision that comes back reaches the
// hook's stdout in Claude Code's expected hookSpecificOutput shape.
//
// relayLLM's own side of the contract (minting a token, accepting it only
// for the session it names, rejecting it everywhere else) is covered by
// internal/api's hook-token tests; this test cannot reach relayLLM's
// internal packages (cmd/hook is a separate module by design — see this
// repo's root CLAUDE.md), so it fakes just enough of /api/permission to
// prove the hook side of the wire.
func TestHookBinary_EndToEnd_ScopedTokenGetsDecision(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "relayllm-hook-e2e")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	const scopedToken = "scoped-session-token-for-e2e-test"
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/permission", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if gotAuth != "Bearer "+scopedToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"decision": "allow",
			"reason":   "test-server auto-allow",
		})
	})

	sockPath := filepath.Join(dir, "relayllm.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	fakeServer := &http.Server{Handler: mux}
	go func() { _ = fakeServer.Serve(ln) }()
	t.Cleanup(func() { _ = fakeServer.Close() })

	hookBin := filepath.Join(dir, "hook")
	build := exec.Command("go", "build", "-o", hookBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hook binary: %v\n%s", err, out)
	}

	cmd := exec.Command(hookBin)
	cmd.Env = []string{
		"RELAY_LLM_HOOK_SOCKET=" + sockPath,
		"RELAY_LLM_SESSION_ID=sess-e2e-1",
		"RELAY_LLM_HOOK_TOKEN=" + scopedToken,
	}
	cmd.Stdin = strings.NewReader(`{"tool_name":"Read","tool_input":{"path":"/tmp/x"},"tool_use_id":"t1"}`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("hook binary failed: %v (stderr=%s)", err, stderr.String())
	}

	var result struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode hook stdout %q: %v", stdout.String(), err)
	}
	if result.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("permissionDecision = %q; want allow", result.HookSpecificOutput.PermissionDecision)
	}
	if gotAuth != "Bearer "+scopedToken {
		t.Errorf("server saw Authorization %q; want Bearer %s", gotAuth, scopedToken)
	}
}
