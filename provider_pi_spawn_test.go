package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Hermetic coverage of `pi --mode rpc` argv construction. Locks the model
// routing flags (--provider/--model/--thinking), session resume (--session),
// the --skill auto-mount decision, and ${PROFILE} extraArgs expansion so a
// regression in any of these fails the default suite rather than only the
// //go:build live tier. argv/env helpers (spawnFlagValue etc.) live in
// provider_claude_spawn_test.go — same package.

func piArgsProvider(sess *Session) *PiProvider {
	return &PiProvider{session: sess}
}

func TestBuildPiArgs_BaseMode(t *testing.T) {
	p := piArgsProvider(&Session{ID: "s"})
	args := p.buildPiArgs(SpawnSubs{}, "/data/pi-sessions", "")

	// `--mode rpc` must be the leading pair.
	if len(args) < 2 || args[0] != "--mode" || args[1] != "rpc" {
		t.Fatalf("args must lead with --mode rpc; got %v", args)
	}
	if v, ok := spawnFlagValue(args, "--session-dir"); !ok || v != "/data/pi-sessions" {
		t.Errorf("--session-dir = %q (present=%v); want /data/pi-sessions", v, ok)
	}
}

func TestBuildPiArgs_OmitsEmptyOptionals(t *testing.T) {
	p := piArgsProvider(&Session{ID: "s"}) // no provider/model/thinking/session
	args := p.buildPiArgs(SpawnSubs{}, "/d", "")

	for _, flag := range []string{"--provider", "--model", "--thinking", "--session", "--skill", "--append-system-prompt"} {
		if spawnHasFlag(args, flag) {
			t.Errorf("unset field leaked flag %q; got %v", flag, args)
		}
	}
}

func TestBuildPiArgs_RoutingFlags(t *testing.T) {
	p := piArgsProvider(&Session{ID: "s", SystemPrompt: "be terse"})
	p.provider = "anthropic"
	p.modelID = "claude-sonnet-4-20250514"
	p.thinkingLevel = "high"
	p.piSessionID = "pi-uuid-1"
	args := p.buildPiArgs(SpawnSubs{}, "/d", "")

	checks := map[string]string{
		"--provider":             "anthropic",
		"--model":                "claude-sonnet-4-20250514",
		"--thinking":             "high",
		"--session":              "pi-uuid-1",
		"--append-system-prompt": "be terse",
	}
	for flag, want := range checks {
		if v, ok := spawnFlagValue(args, flag); !ok || v != want {
			t.Errorf("%s = %q (present=%v); want %q", flag, v, ok, want)
		}
	}
}

func TestBuildPiArgs_SkillAppendedWhenResolved(t *testing.T) {
	p := piArgsProvider(&Session{ID: "s"})

	if spawnHasFlag(p.buildPiArgs(SpawnSubs{}, "/d", ""), "--skill") {
		t.Error("empty skillDir must omit --skill")
	}
	if v, ok := spawnFlagValue(p.buildPiArgs(SpawnSubs{}, "/d", "/proj/.claude/skills"), "--skill"); !ok || v != "/proj/.claude/skills" {
		t.Errorf("--skill = %q (present=%v); want /proj/.claude/skills", v, ok)
	}
}

func TestBuildPiArgs_ExtraArgsExpanded(t *testing.T) {
	p := piArgsProvider(&Session{ID: "s"})
	p.extraArgs = []string{"--no-context-files", "--cwd", "${PROJECT_PATH}", "--token", "${RELAY_TOKEN}"}
	subs := SpawnSubs{ProjectPath: "/home/eve/proj", RelayToken: "ptok"}

	args := p.buildPiArgs(subs, "/d", "")

	if v, _ := spawnFlagValue(args, "--cwd"); v != "/home/eve/proj" {
		t.Errorf("--cwd = %q; want expanded /home/eve/proj", v)
	}
	if v, _ := spawnFlagValue(args, "--token"); v != "ptok" {
		t.Errorf("--token = %q; want expanded ptok", v)
	}
	if !spawnHasFlag(args, "--no-context-files") {
		t.Error("plain extraArg --no-context-files dropped")
	}
}

// ---------------------------------------------------------------------------
// resolveSkillDir — the filesystem-probing half, kept out of buildPiArgs.
// ---------------------------------------------------------------------------

func TestResolveSkillDir_FoundUnderDirectory(t *testing.T) {
	root := t.TempDir()
	skills := filepath.Join(root, ".claude", "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	p := &PiProvider{session: &Session{ID: "s"}, directory: root}

	if got := p.resolveSkillDir(SpawnSubs{}); got != skills {
		t.Errorf("resolveSkillDir = %q; want %q", got, skills)
	}
}

func TestResolveSkillDir_AbsentDirReturnsEmpty(t *testing.T) {
	p := &PiProvider{session: &Session{ID: "s"}, directory: t.TempDir()} // no .claude/skills
	if got := p.resolveSkillDir(SpawnSubs{}); got != "" {
		t.Errorf("resolveSkillDir = %q; want empty for absent dir", got)
	}
}

func TestResolveSkillDir_SkippedWhenExtraArgsHasSkill(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &PiProvider{session: &Session{ID: "s"}, directory: root, extraArgs: []string{"--skill", "/custom"}}

	// Even though the convention dir exists, an explicit --skill wins.
	if got := p.resolveSkillDir(SpawnSubs{}); got != "" {
		t.Errorf("resolveSkillDir = %q; want empty when extraArgs already has --skill", got)
	}
}

// SpawnSubs.ProjectPath (relay-resolved) takes precedence over the local
// directory for locating skills.
func TestResolveSkillDir_PrefersSubsProjectPath(t *testing.T) {
	relayRoot := t.TempDir()
	skills := filepath.Join(relayRoot, ".claude", "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	p := &PiProvider{session: &Session{ID: "s"}, directory: t.TempDir()} // local dir has no skills

	if got := p.resolveSkillDir(SpawnSubs{ProjectPath: relayRoot}); got != skills {
		t.Errorf("resolveSkillDir = %q; want %q (from subs.ProjectPath)", got, skills)
	}
}
