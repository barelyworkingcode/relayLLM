package provider

import (
	"strings"
	"testing"

	"relayllm/internal/relay"
	"relayllm/internal/sshhost"
	"relayllm/internal/types"
)

// Security regression suite (Claude spawn / env / host-exec slice). See
// security_regression_test.go at the repo root for the full suite's naming
// convention and rationale; this file holds the tests that need
// package-internal helpers (spawnHasFlag, envValue, claudeArgsProvider,
// buildHostExec) that moved here with the Claude provider.

// ---------------------------------------------------------------------------
// Claude headless isolation (claude.go)
// ---------------------------------------------------------------------------
//
// The hook auto-approves tool calls only when RELAY_LLM_HEADLESS=true. If a
// non-headless, default-permission session ever emitted that env var or the
// --dangerously-skip-permissions flag, an interactive user's tool calls would
// be silently auto-approved. This must never regress.

func TestSec_ClaudeSpawn_DefaultSessionNeverBypassesPermissions(t *testing.T) {
	p := &ClaudeProvider{session: &types.Session{ID: "s", Model: "m"}, model: "m"}

	args := p.buildClaudeArgs("")
	if spawnHasFlag(args, "--dangerously-skip-permissions") {
		t.Error("default session leaked --dangerously-skip-permissions")
	}

	env := p.buildClaudeEnv(nil, "")
	if v, ok := envValue(env, "RELAY_LLM_HEADLESS"); ok {
		t.Errorf("default session set RELAY_LLM_HEADLESS=%q; must be unset", v)
	}
}

func TestSec_ClaudeSpawn_HeadlessSessionMarksHeadlessEnv(t *testing.T) {
	p := &ClaudeProvider{session: &types.Session{ID: "s", Model: "m", Headless: true}, model: "m"}
	env := p.buildClaudeEnv(nil, "")
	if v, ok := envValue(env, "RELAY_LLM_HEADLESS"); !ok || v != "true" {
		t.Errorf("headless session RELAY_LLM_HEADLESS=%q (present=%v); want true", v, ok)
	}
}

// ---------------------------------------------------------------------------
// Project-token fail-closed (claude.go + internal/spawn)
// ---------------------------------------------------------------------------
//
// When relay can't resolve a project token, the child must get NO token — never
// the full-access service token. Guards the documented invariant in
// CLAUDE.md / relay ADR-007.

func TestSec_ClaudeEnv_EmptyProjectTokenInjectsNoToken(t *testing.T) {
	p := &ClaudeProvider{session: &types.Session{ID: "s"}}
	env := p.buildClaudeEnv(nil, "") // empty resolved token == fail to resolve

	for _, key := range []string{relay.EnvProjectToken, relay.EnvProjectTokenLegacy, relay.EnvServiceToken, relay.EnvServiceTokenLegacy} {
		if v, ok := envValue(env, key); ok {
			t.Errorf("empty project token still set %s=%q; must be absent (fail closed)", key, v)
		}
	}
}

func TestSec_ClaudeEnv_ResolvedProjectTokenInjectedDualNamed(t *testing.T) {
	p := &ClaudeProvider{session: &types.Session{ID: "s"}}
	env := p.buildClaudeEnv(nil, "proj-token-xyz")

	if v, ok := envValue(env, relay.EnvProjectToken); !ok || v != "proj-token-xyz" {
		t.Errorf("%s = %q (present=%v); want proj-token-xyz", relay.EnvProjectToken, v, ok)
	}
	if v, ok := envValue(env, relay.EnvProjectTokenLegacy); !ok || v != "proj-token-xyz" {
		t.Errorf("%s = %q (present=%v); want proj-token-xyz", relay.EnvProjectTokenLegacy, v, ok)
	}
	// The service token must never be how we authenticate a child.
	if _, ok := envValue(env, relay.EnvServiceToken); ok {
		t.Errorf("%s leaked into child env", relay.EnvServiceToken)
	}
}

// ---------------------------------------------------------------------------
// Host session isolation (claude.go, ../relay/docs/ssh-hosts.md)
// ---------------------------------------------------------------------------
//
// A host has no PreToolUse hook binary or bridge socket, and v1 carries no
// relay MCPs or project tokens there (decision 6). A host exec must never
// carry the hook socket, hook token, or any relay token in its argv or env —
// leaking any of those would hand the remote host credentials that assume a
// trusted, same-machine child.

func TestSec_HostExec_ArgvNeverContainsHookOrRelaySecrets(t *testing.T) {
	spec := &types.HostSpec{SSHArgv: []string{"ssh", "-o", "BatchMode=yes", "admin@devbox"}, ClaudePath: "/opt/homebrew/bin/claude"}
	session := &types.Session{ID: "sess-1", Model: "sonnet", Host: spec}
	p := claudeArgsProvider(session)
	args := p.buildClaudeArgs("")

	env := map[string]string{"RELAY_LLM_SESSION_ID": session.ID}
	_, argv := buildHostExec(spec, "/proj", args, env)
	// The remote command is base64-encoded inside argv, so decode it before
	// grepping for secrets — a plaintext substring check on argv itself would
	// only ever see the encoded form and always pass, vacuously.
	decoded := sshhost.RemoteShellCommandDecodedForTest(argv[len(argv)-1])
	joined := strings.Join(argv[:len(argv)-1], " ") + " " + decoded

	for _, secret := range []string{"hook.sock", "RELAY_LLM_HOOK_SOCKET", "RELAY_LLM_HOOK_TOKEN", "RELAY_PROJECT_TOKEN", "RELAY_TOKEN", "RELAY_SERVICE_TOKEN"} {
		if strings.Contains(joined, secret) {
			t.Errorf("host exec argv leaked %q: %s", secret, joined)
		}
	}
	if !strings.Contains(joined, "RELAY_LLM_SESSION_ID") {
		t.Error("host exec must still carry RELAY_LLM_SESSION_ID")
	}
}

func TestSec_HostExec_EnvIsSessionIDOnly(t *testing.T) {
	spec := &types.HostSpec{SSHArgv: []string{"ssh", "admin@devbox"}, ClaudePath: "/opt/homebrew/bin/claude"}
	env := map[string]string{"RELAY_LLM_SESSION_ID": "sess-1"}
	_, argv := buildHostExec(spec, "/proj", []string{"--print"}, env)

	// The env is baked into the remote command's `exec env 'K'='v' …` clause;
	// assert the decoded script carries exactly one env assignment.
	remote := argv[len(argv)-1]
	decoded := sshhost.RemoteShellCommandDecodedForTest(remote)
	count := strings.Count(decoded, "'='")
	// buildRemoteScript never quotes '=' itself; each K=V pair renders as
	// 'KEY'='VALUE', so a single assignment produces exactly one such pair.
	if count != 1 {
		t.Errorf("decoded remote script has %d env assignments, want 1: %s", count, decoded)
	}
	if !strings.Contains(decoded, "'RELAY_LLM_SESSION_ID'='sess-1'") {
		t.Errorf("decoded remote script missing RELAY_LLM_SESSION_ID: %s", decoded)
	}
}
