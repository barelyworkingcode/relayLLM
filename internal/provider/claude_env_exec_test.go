package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relayllm/internal/permission"
	"relayllm/internal/types"
)

// TestClaudeProvider_Start_RealChildNeverSeesInternalBearer spawns a real
// child process (a fake "claude" found via $PATH, exactly how
// spawn.ResolveClaudePath resolves the genuine binary) and reads the
// environment the child actually received from inside the child itself,
// rather than trusting buildClaudeEnv's return value. That distinction
// matters here specifically: buildClaudeEnv's own unit tests already prove
// what the builder *intends* to pass; this proves ChildBaseEnv's scrub and
// exec.Cmd.Env wiring actually take effect for a process really spawned by
// Start(), the same path production runs.
//
// The scenario: relayLLM's own process environment carries RELAY_LLM_TOKEN
// (as it would when relay launched it via the flag's env-var form — see
// app.go's `--token` flag). That must never reach the child; only the
// per-session hook token minted for this provider may appear, under
// RELAY_LLM_HOOK_TOKEN.
func TestClaudeProvider_Start_RealChildNeverSeesInternalBearer(t *testing.T) {
	if _, err := os.Stat("/usr/local/bin/claude"); err == nil {
		t.Skip("a real /usr/local/bin/claude exists on this machine; ResolveClaudePath would prefer it over the fake, which would make this test meaningless")
	}

	homeDir := t.TempDir()
	binDir := t.TempDir()
	workDir := t.TempDir()
	envOutFile := filepath.Join(workDir, "child-env.txt")

	// /usr/bin/env by absolute path, not the bare "env" builtin lookup — PATH
	// below is deliberately narrowed to binDir alone (so ResolveClaudePath's
	// fallback finds only our fake claude), which would otherwise leave the
	// script unable to find `env` itself.
	script := "#!/bin/sh\n/usr/bin/env > " + envOutFile + "\n"
	scriptPath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}

	// HOME clears spawn.ResolveClaudePath's ~/.local/bin and ~/.claude/local
	// candidates so it falls through to the PATH lookup below, which finds
	// only the fake script.
	t.Setenv("HOME", homeDir)
	t.Setenv("PATH", binDir)

	const internalBearer = "internal-bearer-must-not-leak-into-child-env"
	t.Setenv("RELAY_LLM_TOKEN", internalBearer)

	perms := permission.NewPermissionManager()
	session := &types.Session{ID: "sess-env-exec", Model: "test-model", Directory: workDir}
	p := NewClaudeProvider(session, noopHandler, "/tmp/does-not-matter.sock", perms)
	scopedToken := p.hookToken
	if scopedToken == "" {
		t.Fatal("provider minted no hook token")
	}

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-p.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("fake claude child never exited")
	}

	data, err := os.ReadFile(envOutFile)
	if err != nil {
		t.Fatalf("read child env dump: %v", err)
	}
	childEnv := string(data)

	if strings.Contains(childEnv, internalBearer) {
		t.Error("the internal bearer's plaintext appeared in the child's real environment")
	}
	if strings.Contains(childEnv, "RELAY_LLM_TOKEN=") {
		t.Error("RELAY_LLM_TOKEN reached the child's real environment")
	}
	if !strings.Contains(childEnv, "RELAY_LLM_HOOK_TOKEN="+scopedToken+"\n") {
		t.Error("the scoped per-session hook token did not reach the child's real environment")
	}
}
