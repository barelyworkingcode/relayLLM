package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaterializePiImageGenSkill verifies that the skill file lands at
// the documented path with the expected curl-via-unix-socket invocation
// in its body — the curl line is what pi will copy into bash, so a typo
// would silently break image generation for every pi user.
func TestMaterializePiImageGenSkill(t *testing.T) {
	dataDir := t.TempDir()
	dir, err := MaterializePiImageGenSkill(dataDir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	want := filepath.Join(dataDir, "pi-skills")
	if dir != want {
		t.Errorf("returned dir = %q, want %q", dir, want)
	}

	path := filepath.Join(want, "comfyui-image-gen", "SKILL.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	bodyStr := string(body)

	// Frontmatter sanity — pi extracts the description from here.
	if !strings.Contains(bodyStr, "name: comfyui-image-gen") {
		t.Error("skill frontmatter missing name")
	}
	// The literal curl invocation the LLM will copy. Asserting both
	// the socket and token env vars catches regressions where the
	// authentication scheme changes but the skill body is forgotten.
	for _, needle := range []string{
		"curl",
		"--unix-socket \"$RELAY_LLM_SOCKET\"",
		"Authorization: Bearer $RELAY_LLM_TOKEN",
		"/api/generate-image",
		"\"status\": \"success\"",
	} {
		if !strings.Contains(bodyStr, needle) {
			t.Errorf("skill body missing %q", needle)
		}
	}
}

// TestPiOverlayAttachesImageGenSkill verifies the round trip from
// PiOverlayInputs.HasImageGen → settings.json["skills"] → pi runtime.
// Without this, the skill file exists but pi never loads it.
func TestPiOverlayAttachesImageGenSkill(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	globalDir := filepath.Join(tmp, ".pi", "agent")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(globalDir, "auth.json"), `{}`)

	projectDir := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	skillRoot := filepath.Join(tmp, "data", "pi-skills")
	if err := os.MkdirAll(filepath.Join(skillRoot, "comfyui-image-gen"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &PiConfig{ProjectOverlay: PiProjectOverlay{Mode: AutoRegenAlways}}
	inputs := PiOverlayInputs{
		HasImageGen:      true,
		ImageGenSkillDir: skillRoot,
		RelayLLMSocket:   "/tmp/relayllm.sock",
		RelayLLMToken:    "test-token",
	}

	overlayDir, err := MaterializePiOverlay(projectDir, cfg, inputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	var settings map[string]any
	mustReadJSON(t, filepath.Join(overlayDir, "settings.json"), &settings)
	skills, _ := settings["skills"].([]any)
	found := false
	for _, s := range skills {
		if str, _ := s.(string); str == skillRoot {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("skills array missing image-gen dir %q (got %v)", skillRoot, skills)
	}

	// Env injection: the skill is useless without RELAY_LLM_SOCKET +
	// RELAY_LLM_TOKEN in pi's environment.
	env, err := applyPiOverlayEnv(nil, projectDir, cfg, inputs)
	if err != nil {
		t.Fatalf("applyPiOverlayEnv: %v", err)
	}
	got := envMap(env)
	if got["RELAY_LLM_SOCKET"] != "/tmp/relayllm.sock" {
		t.Errorf("RELAY_LLM_SOCKET=%q, want /tmp/relayllm.sock", got["RELAY_LLM_SOCKET"])
	}
	if got["RELAY_LLM_TOKEN"] != "test-token" {
		t.Errorf("RELAY_LLM_TOKEN=%q, want test-token", got["RELAY_LLM_TOKEN"])
	}

	// Negative case: HasImageGen=false leaves env clean and skips skill.
	inputs.HasImageGen = false
	env, err = applyPiOverlayEnv(nil, projectDir, cfg, inputs)
	if err != nil {
		t.Fatalf("applyPiOverlayEnv (disabled): %v", err)
	}
	got = envMap(env)
	if _, ok := got["RELAY_LLM_SOCKET"]; ok {
		t.Errorf("RELAY_LLM_SOCKET leaked when HasImageGen=false")
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			out[kv[:idx]] = kv[idx+1:]
		}
	}
	return out
}
