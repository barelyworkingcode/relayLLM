package main

import (
	"strings"
	"testing"
)

func TestSpawnSubsExpand(t *testing.T) {
	s := SpawnSubs{
		ProjectPath: "/proj",
		SkillPath:   "/proj/.claude/skills/relay",
		RelayToken:  "tok123",
	}
	in := "--cwd ${project.path} --skill ${SKILL_PATH} --token ${RELAY_TOKEN} --also ${PROJECT_PATH}"
	got := s.Expand(in)
	want := "--cwd /proj --skill /proj/.claude/skills/relay --token tok123 --also /proj"
	if got != want {
		t.Fatalf("Expand:\n got: %q\nwant: %q", got, want)
	}
}

// TestRelayManagedSpec_NotManaged confirms that a spec with neither
// UseRelayToken nor AutoRegenSkills bypasses the bridge entirely.
func TestRelayManagedSpec_NotManaged(t *testing.T) {
	spec := RelayManagedSpec{Directory: "/tmp/proj"}
	subs, err := spec.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if subs.ProjectPath != "/tmp/proj" {
		t.Errorf("ProjectPath: got %q want /tmp/proj", subs.ProjectPath)
	}
	if subs.SkillPath != "" || subs.RelayToken != "" {
		t.Errorf("expected empty skillPath/relayToken, got %+v", subs)
	}
}

// TestRelayManagedSpec_FailsClosedOnMissingSkillPath verifies the
// fail-closed semantics inherited from the PTY version: if AutoRegenSkills
// is set but SkillPath is empty, we must error rather than ask relay for
// regen with no destination.
func TestRelayManagedSpec_FailsClosedOnMissingSkillPath(t *testing.T) {
	spec := RelayManagedSpec{
		Directory:       "/tmp/proj",
		AutoRegenSkills: "always",
		SkillPath:       "",
		Label:           "pi",
	}
	_, err := spec.Resolve()
	if err == nil {
		t.Fatal("expected error for missing skillPath when autoRegenSkills=always, got nil")
	}
	if !strings.Contains(err.Error(), "skillPath required") {
		t.Errorf("error message: got %q want substring 'skillPath required'", err.Error())
	}
	if !strings.Contains(err.Error(), "pi") {
		t.Errorf("error message should include the label 'pi': %q", err.Error())
	}
}

// TestRelayManagedSpec_NeverRegenIsNotManaged verifies that
// AutoRegenSkills="never" with UseRelayToken=false skips the bridge.
func TestRelayManagedSpec_NeverRegenIsNotManaged(t *testing.T) {
	spec := RelayManagedSpec{
		Directory:       "/tmp/proj",
		AutoRegenSkills: "never",
		SkillPath:       "/anywhere",
	}
	subs, err := spec.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if subs.ProjectPath != "/tmp/proj" {
		t.Errorf("ProjectPath: got %q want /tmp/proj", subs.ProjectPath)
	}
}

func TestHasArg(t *testing.T) {
	if !hasArg([]string{"--skill", "/x"}, "--skill") {
		t.Error("hasArg should find --skill")
	}
	if !hasArg([]string{"--SKILL", "/x"}, "--skill") {
		t.Error("hasArg should be case-insensitive")
	}
	if hasArg([]string{"--other"}, "--skill") {
		t.Error("hasArg false positive")
	}
	if hasArg(nil, "--skill") {
		t.Error("hasArg on nil should return false")
	}
}
