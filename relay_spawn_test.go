package main

import (
	"testing"
)

func TestSpawnSubsExpand(t *testing.T) {
	s := SpawnSubs{
		ProjectPath: "/proj",
		RelayToken:  "tok123",
	}
	in := "--cwd ${project.path} --skill ${PROJECT_PATH}/.claude/skills --token ${RELAY_TOKEN}"
	got := s.Expand(in)
	want := "--cwd /proj --skill /proj/.claude/skills --token tok123"
	if got != want {
		t.Fatalf("Expand:\n got: %q\nwant: %q", got, want)
	}
}

// TestRelayManagedSpec_NotManaged confirms that a spec with neither
// UseRelayToken nor a ProjectID bypasses the bridge entirely.
func TestRelayManagedSpec_NotManaged(t *testing.T) {
	spec := RelayManagedSpec{Directory: "/tmp/proj"}
	subs, err := spec.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if subs.ProjectPath != "/tmp/proj" {
		t.Errorf("ProjectPath: got %q want /tmp/proj", subs.ProjectPath)
	}
	if subs.RelayToken != "" {
		t.Errorf("expected empty relayToken, got %+v", subs)
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
