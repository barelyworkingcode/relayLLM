package main

import (
	"strings"
	"testing"
)

// Hermetic coverage of Claude CLI spawn argv/env construction. These lock the
// flag matrix that previously was only exercised by the //go:build llm tier,
// so a regression that (for example) drops --model, leaks --resume, or
// half-applies the headless escape hatch fails on the default `go test ./...`.
//
// The security-framed invariants (headless isolation, project-token
// fail-closed) are duplicated in security_regression_test.go on purpose — see
// the note there.

// ---------------------------------------------------------------------------
// argv/env assertion helpers (shared with provider_pi_spawn_test.go and
// security_regression_test.go — all package main).
// ---------------------------------------------------------------------------

// spawnFlagValue returns the token following the first occurrence of flag, and
// whether the flag was present with a following value.
func spawnFlagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

// spawnHasFlag reports whether args contains flag as a standalone token.
func spawnHasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// envValue returns the value of KEY in a "KEY=VALUE" env slice, and whether it
// was present.
func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):], true
		}
	}
	return "", false
}

func claudeArgsProvider(sess *Session) *ClaudeProvider {
	return &ClaudeProvider{session: sess, model: sess.Model}
}

// ---------------------------------------------------------------------------
// buildClaudeArgs
// ---------------------------------------------------------------------------

func TestBuildClaudeArgs_BaseFlags(t *testing.T) {
	p := claudeArgsProvider(&Session{ID: "s1", Model: "claude-sonnet-4-6"})
	args := p.buildClaudeArgs("")

	// The fixed stream-json contract Eve's renderer depends on.
	for _, want := range []string{"--print", "--input-format", "stream-json", "--verbose"} {
		if !spawnHasFlag(args, want) {
			t.Errorf("base args missing %q; got %v", want, args)
		}
	}
	if v, ok := spawnFlagValue(args, "--output-format"); !ok || v != "stream-json" {
		t.Errorf("--output-format = %q (present=%v); want stream-json", v, ok)
	}
	if v, ok := spawnFlagValue(args, "--model"); !ok || v != "claude-sonnet-4-6" {
		t.Errorf("--model = %q (present=%v); want claude-sonnet-4-6", v, ok)
	}
}

func TestBuildClaudeArgs_ResumeOnlyWithSessionID(t *testing.T) {
	fresh := claudeArgsProvider(&Session{ID: "s1", Model: "m"})
	if spawnHasFlag(fresh.buildClaudeArgs(""), "--resume") {
		t.Error("fresh session must not pass --resume")
	}

	resumed := claudeArgsProvider(&Session{ID: "s1", Model: "m"})
	resumed.claudeSessionID = "abc-123"
	if v, ok := spawnFlagValue(resumed.buildClaudeArgs(""), "--resume"); !ok || v != "abc-123" {
		t.Errorf("--resume = %q (present=%v); want abc-123", v, ok)
	}
}

// The core security guard: a normal (non-headless, default-mode) session must
// never receive the permission escape hatches.
func TestBuildClaudeArgs_DefaultModeHasNoPermissionFlags(t *testing.T) {
	p := claudeArgsProvider(&Session{ID: "s1", Model: "m"})
	args := p.buildClaudeArgs("")

	if spawnHasFlag(args, "--dangerously-skip-permissions") {
		t.Error("default session must NOT pass --dangerously-skip-permissions")
	}
	if spawnHasFlag(args, "--permission-mode") {
		t.Error("default session must NOT pass --permission-mode")
	}
}

func TestBuildClaudeArgs_HeadlessAddsBypassAndSkip(t *testing.T) {
	p := claudeArgsProvider(&Session{ID: "s1", Model: "m", Headless: true})
	args := p.buildClaudeArgs("")

	if v, ok := spawnFlagValue(args, "--permission-mode"); !ok || v != "bypassPermissions" {
		t.Errorf("--permission-mode = %q (present=%v); want bypassPermissions", v, ok)
	}
	if !spawnHasFlag(args, "--dangerously-skip-permissions") {
		t.Error("headless session must pass --dangerously-skip-permissions")
	}
}

// Headless is the legacy synonym and must win even when a different mode is set.
func TestBuildClaudeArgs_HeadlessOverridesExplicitMode(t *testing.T) {
	p := claudeArgsProvider(&Session{ID: "s1", Model: "m", Headless: true, PermissionMode: "acceptEdits"})
	args := p.buildClaudeArgs("")
	if v, _ := spawnFlagValue(args, "--permission-mode"); v != "bypassPermissions" {
		t.Errorf("headless must force bypassPermissions, got --permission-mode %q", v)
	}
	if !spawnHasFlag(args, "--dangerously-skip-permissions") {
		t.Error("headless must add --dangerously-skip-permissions even with explicit mode")
	}
}

// A non-default mode that is NOT bypass forwards the mode but never the skip flag.
func TestBuildClaudeArgs_ExplicitModeNoBypass(t *testing.T) {
	p := claudeArgsProvider(&Session{ID: "s1", Model: "m", PermissionMode: "acceptEdits"})
	args := p.buildClaudeArgs("")

	if v, ok := spawnFlagValue(args, "--permission-mode"); !ok || v != "acceptEdits" {
		t.Errorf("--permission-mode = %q (present=%v); want acceptEdits", v, ok)
	}
	if spawnHasFlag(args, "--dangerously-skip-permissions") {
		t.Error("acceptEdits is not bypass; must NOT pass --dangerously-skip-permissions")
	}
}

func TestBuildClaudeArgs_PolicyTools(t *testing.T) {
	p := claudeArgsProvider(&Session{
		ID:    "s1",
		Model: "m",
		Policy: &PermissionPolicy{
			AllowedTools: []string{"Read", "Grep"},
			DeniedTools:  []string{"Bash"},
		},
	})
	args := p.buildClaudeArgs("")

	if v, ok := spawnFlagValue(args, "--allowedTools"); !ok || v != "Read,Grep" {
		t.Errorf("--allowedTools = %q (present=%v); want Read,Grep", v, ok)
	}
	if v, ok := spawnFlagValue(args, "--disallowedTools"); !ok || v != "Bash" {
		t.Errorf("--disallowedTools = %q (present=%v); want Bash", v, ok)
	}
}

func TestBuildClaudeArgs_MCPConfigOptional(t *testing.T) {
	p := claudeArgsProvider(&Session{ID: "s1", Model: "m"})

	if spawnHasFlag(p.buildClaudeArgs(""), "--mcp-config") {
		t.Error("empty mcpCfg must omit --mcp-config")
	}
	if v, ok := spawnFlagValue(p.buildClaudeArgs(`{"mcpServers":{}}`), "--mcp-config"); !ok || v != `{"mcpServers":{}}` {
		t.Errorf("--mcp-config = %q (present=%v); want the passed JSON", v, ok)
	}
}

// ---------------------------------------------------------------------------
// buildClaudeEnv
// ---------------------------------------------------------------------------

func TestBuildClaudeEnv_AlwaysSocketAndSessionID(t *testing.T) {
	p := &ClaudeProvider{session: &Session{ID: "sess-42"}, hookSocket: "/tmp/h.sock"}
	env := p.buildClaudeEnv(nil, "")

	if v, ok := envValue(env, "RELAY_LLM_HOOK_SOCKET"); !ok || v != "/tmp/h.sock" {
		t.Errorf("RELAY_LLM_HOOK_SOCKET = %q (present=%v); want /tmp/h.sock", v, ok)
	}
	if v, ok := envValue(env, "RELAY_LLM_SESSION_ID"); !ok || v != "sess-42" {
		t.Errorf("RELAY_LLM_SESSION_ID = %q (present=%v); want sess-42", v, ok)
	}
}

func TestBuildClaudeEnv_HookTokenOnlyWhenSet(t *testing.T) {
	without := &ClaudeProvider{session: &Session{ID: "s"}}
	if _, ok := envValue(without.buildClaudeEnv(nil, ""), "RELAY_LLM_HOOK_TOKEN"); ok {
		t.Error("no hookToken must omit RELAY_LLM_HOOK_TOKEN")
	}

	with := &ClaudeProvider{session: &Session{ID: "s"}, hookToken: "tok"}
	if v, ok := envValue(with.buildClaudeEnv(nil, ""), "RELAY_LLM_HOOK_TOKEN"); !ok || v != "tok" {
		t.Errorf("RELAY_LLM_HOOK_TOKEN = %q (present=%v); want tok", v, ok)
	}
}

func TestBuildClaudeEnv_HeadlessFlagTracksMode(t *testing.T) {
	headless := &ClaudeProvider{session: &Session{ID: "s", Headless: true}}
	if v, ok := envValue(headless.buildClaudeEnv(nil, ""), "RELAY_LLM_HEADLESS"); !ok || v != "true" {
		t.Errorf("headless RELAY_LLM_HEADLESS = %q (present=%v); want true", v, ok)
	}

	normal := &ClaudeProvider{session: &Session{ID: "s"}}
	if _, ok := envValue(normal.buildClaudeEnv(nil, ""), "RELAY_LLM_HEADLESS"); ok {
		t.Error("non-headless session must NOT set RELAY_LLM_HEADLESS")
	}
}
