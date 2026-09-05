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

// ---------------------------------------------------------------------------
// buildClaudeArgs — host branch (../relay/docs/ssh-hosts.md decision 5/6)
// ---------------------------------------------------------------------------

func TestBuildClaudeArgs_Host_AddsStdioPromptToolOmitsMCPConfig(t *testing.T) {
	p := claudeArgsProvider(&Session{ID: "s1", Model: "sonnet", Host: &HostSpec{ID: "h1", Name: "devbox"}})
	args := p.buildClaudeArgs(`{"mcpServers":{}}`)

	if v, ok := spawnFlagValue(args, "--permission-prompt-tool"); !ok || v != "stdio" {
		t.Errorf("--permission-prompt-tool = %q (present=%v); want stdio", v, ok)
	}
	if spawnHasFlag(args, "--mcp-config") {
		t.Error("a host session must never pass --mcp-config, even when mcpCfg is non-empty")
	}
}

func TestBuildClaudeArgs_Console_NeverAddsStdioPromptTool(t *testing.T) {
	p := claudeArgsProvider(&Session{ID: "s1", Model: "sonnet"})
	if spawnHasFlag(p.buildClaudeArgs(""), "--permission-prompt-tool") {
		t.Error("a console session must not pass --permission-prompt-tool")
	}
}

// ---------------------------------------------------------------------------
// buildHostExec
// ---------------------------------------------------------------------------

func TestBuildHostExec_NameIsSSHArgv0(t *testing.T) {
	spec := &HostSpec{SSHArgv: []string{"ssh", "-o", "BatchMode=yes", "admin@localhost"}, ClaudePath: "/opt/homebrew/bin/claude"}
	name, _ := buildHostExec(spec, "/home/a b", []string{"--print"}, map[string]string{"RELAY_LLM_SESSION_ID": "sid-1"})
	if name != "ssh" {
		t.Errorf("name = %q, want ssh", name)
	}
}

func TestBuildHostExec_ArgvShapeAndTrailingRemoteCommand(t *testing.T) {
	spec := &HostSpec{SSHArgv: []string{"ssh", "-o", "BatchMode=yes", "admin@localhost"}, ClaudePath: "/opt/homebrew/bin/claude"}
	args := []string{"--print", "--model", "sonnet"}
	env := map[string]string{"RELAY_LLM_SESSION_ID": "sid-1"}

	_, argv := buildHostExec(spec, "/home/a b", args, env)

	wantPrefix := []string{"-o", "BatchMode=yes", "admin@localhost", "-T", "--"}
	if len(argv) != len(wantPrefix)+1 {
		t.Fatalf("argv len = %d, want %d (prefix + one remote-command token)", len(argv), len(wantPrefix)+1)
	}
	for i, want := range wantPrefix {
		if argv[i] != want {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], want)
		}
	}

	wantRemote := RemoteCommand("/home/a b", append([]string{spec.ClaudePath}, args...), env)
	if argv[len(argv)-1] != wantRemote {
		t.Errorf("trailing remote command = %q, want %q", argv[len(argv)-1], wantRemote)
	}
}

// TestBuildHostExec_ExactArgvFixture pins the exact argv for a session in
// "/home/a b" on a host whose ssh_argv is
// ["ssh","-o","BatchMode=yes","admin@localhost"] and claude_path
// "/opt/homebrew/bin/claude", model sonnet, default permission mode — the
// scenario named in this feature's task report.
func TestBuildHostExec_ExactArgvFixture(t *testing.T) {
	spec := &HostSpec{
		SSHArgv:    []string{"ssh", "-o", "BatchMode=yes", "admin@localhost"},
		ClaudePath: "/opt/homebrew/bin/claude",
	}
	session := &Session{ID: "sess-123", Model: "sonnet", Host: spec}
	p := claudeArgsProvider(session)
	args := p.buildClaudeArgs("") // host branch: mcpCfg is always ignored

	env := map[string]string{"RELAY_LLM_SESSION_ID": session.ID}
	name, argv := buildHostExec(spec, "/home/a b", args, env)

	if name != "ssh" {
		t.Fatalf("name = %q, want ssh", name)
	}

	wantScript := `cd '/home/a b' && exec env 'RELAY_LLM_SESSION_ID'='sess-123' ` +
		`'/opt/homebrew/bin/claude' '--print' '--output-format' 'stream-json' ` +
		`'--input-format' 'stream-json' '--verbose' '--model' 'sonnet' ` +
		`'--permission-prompt-tool' 'stdio'`
	wantRemote := RemoteShellCommandDecodedForTest(RemoteCommand("/home/a b", append([]string{spec.ClaudePath}, args...), env))
	if wantRemote != wantScript {
		t.Fatalf("test fixture drifted from buildRemoteScript output:\n got %q\nwant %q", wantRemote, wantScript)
	}

	wantArgv := []string{
		"-o", "BatchMode=yes", "admin@localhost", "-T", "--",
		RemoteCommand("/home/a b", append([]string{spec.ClaudePath}, args...), env),
	}
	if len(argv) != len(wantArgv) {
		t.Fatalf("argv = %v, want %v", argv, wantArgv)
	}
	for i := range wantArgv {
		if argv[i] != wantArgv[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], wantArgv[i])
		}
	}
}
