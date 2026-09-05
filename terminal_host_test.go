package main

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// buildHostTerminalExec (../relay/docs/ssh-hosts.md decision 5/8)
// ---------------------------------------------------------------------------

func hostTerminalSpec() *HostSpec {
	return &HostSpec{
		ID:         "h1",
		Name:       "devbox",
		SSHArgv:    []string{"ssh", "-o", "BatchMode=yes", "admin@devbox"},
		ClaudePath: "/opt/homebrew/bin/claude",
	}
}

func TestBuildHostTerminalExec_ArgvPrefixAndFlag(t *testing.T) {
	name, argv := buildHostTerminalExec(hostTerminalSpec(), "shell", "/proj", "", nil)
	if name != "ssh" {
		t.Fatalf("name = %q, want ssh", name)
	}
	want := []string{"-o", "BatchMode=yes", "admin@devbox", "-tt", "--"}
	if len(argv) != len(want)+1 {
		t.Fatalf("argv = %v, want prefix %v plus one remote-command token", argv, want)
	}
	for i, w := range want {
		if argv[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], w)
		}
	}
}

func TestBuildHostTerminalExec_ShellTemplate(t *testing.T) {
	for _, tmplID := range []string{"", "shell"} {
		_, argv := buildHostTerminalExec(hostTerminalSpec(), tmplID, "/proj", "", nil)
		decoded := RemoteShellCommandDecodedForTest(argv[len(argv)-1])
		want := `cd '/proj' && exec env 'TERM'='xterm-256color' "$SHELL" -l`
		if decoded != want {
			t.Errorf("tmplID=%q decoded = %q, want %q", tmplID, decoded, want)
		}
	}
}

func TestBuildHostTerminalExec_ShellTemplate_NoDirLandsInHostHome(t *testing.T) {
	_, argv := buildHostTerminalExec(hostTerminalSpec(), "shell", "", "", nil)
	decoded := RemoteShellCommandDecodedForTest(argv[len(argv)-1])
	if strings.HasPrefix(decoded, "cd ") {
		t.Errorf("empty directory must omit cd (lands in host's login home), got %q", decoded)
	}
}

func TestBuildHostTerminalExec_ClaudeTemplate(t *testing.T) {
	spec := hostTerminalSpec()
	_, argv := buildHostTerminalExec(spec, "claude", "/proj", "claude", []string{"--resume", "abc"})
	decoded := RemoteShellCommandDecodedForTest(argv[len(argv)-1])
	want := `cd '/proj' && exec env 'TERM'='xterm-256color' '/opt/homebrew/bin/claude' '--resume' 'abc'`
	if decoded != want {
		t.Errorf("decoded = %q, want %q", decoded, want)
	}
}

func TestBuildHostTerminalExec_ClaudeTemplate_UsesHostClaudePathNotCommand(t *testing.T) {
	spec := hostTerminalSpec()
	// "command" (the template's Command field) is deliberately wrong here —
	// the claude branch must always use spec.ClaudePath, never the template's
	// own (locally-resolved) command string.
	_, argv := buildHostTerminalExec(spec, "claude", "/proj", "/usr/local/bin/claude-wrong", nil)
	decoded := RemoteShellCommandDecodedForTest(argv[len(argv)-1])
	if !strings.Contains(decoded, "/opt/homebrew/bin/claude") {
		t.Errorf("decoded script must use host.ClaudePath, got %q", decoded)
	}
	if strings.Contains(decoded, "claude-wrong") {
		t.Errorf("decoded script must not use the template's local command, got %q", decoded)
	}
}

func TestBuildHostTerminalExec_OtherTemplate_RunsThroughInteractiveShell(t *testing.T) {
	_, argv := buildHostTerminalExec(hostTerminalSpec(), "npm-test", "/proj", "npm", []string{"test"})
	decoded := RemoteShellCommandDecodedForTest(argv[len(argv)-1])
	if !strings.Contains(decoded, `-lic`) {
		t.Errorf("other-template branch must invoke the login shell with -lic, got %q", decoded)
	}
	if !strings.HasPrefix(decoded, `cd '/proj' && exec env 'TERM'='xterm-256color' "$SHELL" -lic `) {
		t.Errorf("unexpected script shape: %q", decoded)
	}
}

// ---------------------------------------------------------------------------
// resolveTerminalHost — TerminalManager.Create's host-resolution helper.
// Tested standalone (not through Create) so these cases never spawn a real
// ssh subprocess against a host that doesn't exist.
// ---------------------------------------------------------------------------

func TestResolveTerminalHost_ResolvesFromBridge(t *testing.T) {
	fb := NewFakeBridge(t)
	host := &HostSpec{ID: "h1", Name: "devbox", SSHArgv: []string{"ssh", "admin@devbox"}, ClaudePath: "/opt/homebrew/bin/claude"}
	fb.SetHostPtyEnv("/home/admin/proj", host)
	withBridgeEnv(t, fb.SocketPath(), "relay-llm", "svc-token")

	got := resolveTerminalHost("proj-1", "/home/admin/proj")
	if got == nil || got.Name != "devbox" {
		t.Errorf("resolveTerminalHost = %+v, want devbox", got)
	}
}

func TestResolveTerminalHost_NoProjectIDNeverContactsBridge(t *testing.T) {
	fb := NewFakeBridge(t)
	fb.SetHostPtyEnv("/home/admin/proj", &HostSpec{ID: "h1", Name: "devbox"})
	withBridgeEnv(t, fb.SocketPath(), "relay-llm", "svc-token")

	if got := resolveTerminalHost("", "/tmp/scratch"); got != nil {
		t.Errorf("resolveTerminalHost = %+v, want nil for an ad-hoc terminal", got)
	}
	if len(fb.Requests()) != 0 {
		t.Error("an ad-hoc terminal must never contact relay's bridge")
	}
}

func TestResolveTerminalHost_StandaloneReturnsNil(t *testing.T) {
	t.Setenv(envServiceToken, "")
	t.Setenv(envServiceTokenLegacy, "")
	if got := resolveTerminalHost("proj-1", "/tmp/proj"); got != nil {
		t.Errorf("resolveTerminalHost = %+v, want nil when standalone", got)
	}
}

// A terminal on a console (non-host) project must default its directory the
// existing way — resolveTerminalHost itself is exercised above; this pins the
// directory-defaulting decision that reads its result in TerminalManager.Create.
func TestTerminalCreate_ConsoleProject_DefaultsDirectoryToHome(t *testing.T) {
	store := NewTemplateStore(t.TempDir())
	if err := store.Load(nil); err != nil {
		t.Fatalf("template load: %v", err)
	}
	mgr := NewTerminalManager(store, t.TempDir())

	sess, err := mgr.Create("shell", "", "", "", 80, 24, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	if sess.Host != nil {
		t.Errorf("session.Host = %+v, want nil", sess.Host)
	}
	if sess.Directory == "" {
		t.Error("an ad-hoc terminal with no directory must default to the console home")
	}
}
