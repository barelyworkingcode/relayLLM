package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Security regression suite. One file = one audit surface. Every test here
// corresponds to a specific attack-shape that the production code already
// prevents; removing the underlying guard should flip the test to failing.
//
// Naming convention (borrowed from ../relay/security_regression_test.go): each
// test is `TestSec_<surface>_<expected-behavior>` so a future audit can scan
// one file and answer "is this covered?" without reading the bodies.
//
// Several guards here intentionally duplicate an assertion already made in a
// mechanical test (e.g. headless flags in provider_claude_spawn_test.go). The
// duplication is the point: a refactor that quietly relaxes the mechanical test
// still has to get past the security file, which reads as a checklist of
// invariants that must never regress.

// ---------------------------------------------------------------------------
// HTTP bearer auth boundary (auth.go)
// ---------------------------------------------------------------------------

const secTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// secAuthResponse drives a request through bearerAuth-wrapped handler and
// returns the resulting status code. The inner handler writes 200, so any
// status other than 200 means the request was rejected before reaching it.
func secAuthResponse(t *testing.T, configuredToken, authHeader string) int {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	bearerAuth(configuredToken, inner).ServeHTTP(rec, req)
	return rec.Code
}

func TestSec_BearerAuth_RejectsMissingHeader(t *testing.T) {
	if code := secAuthResponse(t, secTestToken, ""); code != http.StatusUnauthorized {
		t.Errorf("missing Authorization header: status %d; want 401", code)
	}
}

func TestSec_BearerAuth_RejectsWrongToken(t *testing.T) {
	if code := secAuthResponse(t, secTestToken, "Bearer not-the-token"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d; want 401", code)
	}
}

// One byte different — guards that the comparison is a real equality check, not
// a prefix/length heuristic.
func TestSec_BearerAuth_RejectsOneByteOffToken(t *testing.T) {
	off := secTestToken[:len(secTestToken)-1] + "0" // last 'f' -> '0'
	if off == secTestToken {
		t.Fatal("test setup: mutated token equals original")
	}
	if code := secAuthResponse(t, secTestToken, "Bearer "+off); code != http.StatusUnauthorized {
		t.Errorf("one-byte-off token: status %d; want 401", code)
	}
}

func TestSec_BearerAuth_RejectsEmptyBearerValue(t *testing.T) {
	if code := secAuthResponse(t, secTestToken, "Bearer "); code != http.StatusUnauthorized {
		t.Errorf("empty bearer value: status %d; want 401", code)
	}
}

// Wrong scheme (e.g. Basic) must not satisfy the Bearer requirement.
func TestSec_BearerAuth_RejectsNonBearerScheme(t *testing.T) {
	if code := secAuthResponse(t, secTestToken, "Basic "+secTestToken); code != http.StatusUnauthorized {
		t.Errorf("Basic scheme: status %d; want 401", code)
	}
}

func TestSec_BearerAuth_AcceptsCorrectToken(t *testing.T) {
	if code := secAuthResponse(t, secTestToken, "Bearer "+secTestToken); code != http.StatusOK {
		t.Errorf("correct token: status %d; want 200", code)
	}
}

// Empty configured token is the documented dev-mode pass-through (main.go warns
// loudly). This test pins that behavior so it can only ever change deliberately.
func TestSec_BearerAuth_EmptyConfiguredTokenIsPassThrough(t *testing.T) {
	if code := secAuthResponse(t, "", ""); code != http.StatusOK {
		t.Errorf("empty configured token should pass through: status %d; want 200", code)
	}
}

// ---------------------------------------------------------------------------
// Auto-provisioned token entropy (auth.go)
// ---------------------------------------------------------------------------

func TestSec_GeneratedBearerToken_Is256BitHexAndUnique(t *testing.T) {
	a := generateBearerToken()
	b := generateBearerToken()
	if len(a) != 64 { // 32 bytes -> 64 hex chars
		t.Errorf("token length = %d; want 64 hex chars (256-bit)", len(a))
	}
	if a == b {
		t.Error("two generated tokens are identical; entropy source is broken")
	}
}

// ---------------------------------------------------------------------------
// Claude headless isolation (provider_claude.go)
// ---------------------------------------------------------------------------
//
// The hook auto-approves tool calls only when RELAY_LLM_HEADLESS=true. If a
// non-headless, default-permission session ever emitted that env var or the
// --dangerously-skip-permissions flag, an interactive user's tool calls would
// be silently auto-approved. This must never regress.

func TestSec_ClaudeSpawn_DefaultSessionNeverBypassesPermissions(t *testing.T) {
	p := &ClaudeProvider{session: &Session{ID: "s", Model: "m"}, model: "m"}

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
	p := &ClaudeProvider{session: &Session{ID: "s", Model: "m", Headless: true}, model: "m"}
	env := p.buildClaudeEnv(nil, "")
	if v, ok := envValue(env, "RELAY_LLM_HEADLESS"); !ok || v != "true" {
		t.Errorf("headless session RELAY_LLM_HEADLESS=%q (present=%v); want true", v, ok)
	}
}

// ---------------------------------------------------------------------------
// Project-token fail-closed (provider_claude.go + relay_spawn.go)
// ---------------------------------------------------------------------------
//
// When relay can't resolve a project token, the child must get NO token — never
// the full-access service token. Guards the documented invariant in
// CLAUDE.md / relay ADR-007.

func TestSec_ClaudeEnv_EmptyProjectTokenInjectsNoToken(t *testing.T) {
	p := &ClaudeProvider{session: &Session{ID: "s"}}
	env := p.buildClaudeEnv(nil, "") // empty resolved token == fail to resolve

	for _, key := range []string{envProjectToken, envProjectTokenLegacy, envServiceToken, envServiceTokenLegacy} {
		if v, ok := envValue(env, key); ok {
			t.Errorf("empty project token still set %s=%q; must be absent (fail closed)", key, v)
		}
	}
}

// ---------------------------------------------------------------------------
// Host session isolation (provider_claude.go, ../relay/docs/ssh-hosts.md)
// ---------------------------------------------------------------------------
//
// A host has no PreToolUse hook binary or bridge socket, and v1 carries no
// relay MCPs or project tokens there (decision 6). A host exec must never
// carry the hook socket, hook token, or any relay token in its argv or env —
// leaking any of those would hand the remote host credentials that assume a
// trusted, same-machine child.

func TestSec_HostExec_ArgvNeverContainsHookOrRelaySecrets(t *testing.T) {
	spec := &HostSpec{SSHArgv: []string{"ssh", "-o", "BatchMode=yes", "admin@devbox"}, ClaudePath: "/opt/homebrew/bin/claude"}
	session := &Session{ID: "sess-1", Model: "sonnet", Host: spec}
	p := claudeArgsProvider(session)
	args := p.buildClaudeArgs("")

	env := map[string]string{"RELAY_LLM_SESSION_ID": session.ID}
	_, argv := buildHostExec(spec, "/proj", args, env)
	// The remote command is base64-encoded inside argv, so decode it before
	// grepping for secrets — a plaintext substring check on argv itself would
	// only ever see the encoded form and always pass, vacuously.
	decoded := RemoteShellCommandDecodedForTest(argv[len(argv)-1])
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
	spec := &HostSpec{SSHArgv: []string{"ssh", "admin@devbox"}, ClaudePath: "/opt/homebrew/bin/claude"}
	env := map[string]string{"RELAY_LLM_SESSION_ID": "sess-1"}
	_, argv := buildHostExec(spec, "/proj", []string{"--print"}, env)

	// The env is baked into the remote command's `exec env 'K'='v' …` clause;
	// assert the decoded script carries exactly one env assignment.
	remote := argv[len(argv)-1]
	decoded := RemoteShellCommandDecodedForTest(remote)
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

// Claude's own child env for a host spawn (childBaseEnv, no ensurePath/token
// injection) must not carry any relay secret either — belt and suspenders
// alongside the argv guard above, since the env is what a leaked debug log
// would actually dump.
func TestSec_HostSpawn_ChildBaseEnvHasNoRelaySecrets(t *testing.T) {
	t.Setenv(envServiceToken, "svc-secret")
	t.Setenv(envProjectToken, "proj-secret")
	t.Setenv(envProjectTokenLegacy, "proj-secret-legacy")

	env := childBaseEnv()
	for _, k := range []string{envServiceToken, envServiceTokenLegacy, envFrontendToken, envProjectToken, envProjectTokenLegacy} {
		if envHasKey(env, k) {
			t.Errorf("host spawn child env leaked %s", k)
		}
	}
}

// A host terminal never gets a project token or any relay secret in its argv
// (v1 carries no relay MCPs/tokens onto a host — decision 6).
func TestSec_HostTerminalExec_ArgvNeverContainsRelaySecrets(t *testing.T) {
	spec := hostTerminalSpec()
	for _, tmplID := range []string{"shell", "claude", "npm-test"} {
		_, argv := buildHostTerminalExec(spec, tmplID, "/proj", "npm", []string{"test"})
		decoded := RemoteShellCommandDecodedForTest(argv[len(argv)-1])
		for _, secret := range []string{"RELAY_PROJECT_TOKEN", "RELAY_TOKEN", "RELAY_SERVICE_TOKEN", "RELAY_LLM_HOOK"} {
			if strings.Contains(decoded, secret) {
				t.Errorf("tmplID=%q host terminal script leaked %q: %s", tmplID, secret, decoded)
			}
		}
	}
}

func TestSec_ClaudeEnv_ResolvedProjectTokenInjectedDualNamed(t *testing.T) {
	p := &ClaudeProvider{session: &Session{ID: "s"}}
	env := p.buildClaudeEnv(nil, "proj-token-xyz")

	if v, ok := envValue(env, envProjectToken); !ok || v != "proj-token-xyz" {
		t.Errorf("%s = %q (present=%v); want proj-token-xyz", envProjectToken, v, ok)
	}
	if v, ok := envValue(env, envProjectTokenLegacy); !ok || v != "proj-token-xyz" {
		t.Errorf("%s = %q (present=%v); want proj-token-xyz", envProjectTokenLegacy, v, ok)
	}
	// The service token must never be how we authenticate a child.
	if _, ok := envValue(env, envServiceToken); ok {
		t.Errorf("%s leaked into child env", envServiceToken)
	}
}
