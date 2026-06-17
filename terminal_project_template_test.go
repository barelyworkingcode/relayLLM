package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// resolveRelayProjectTemplate maps relay's bridge ProjectTemplate response into a
// TerminalTemplate and carries (projectID, templateID) on the request.
func TestResolveRelayProjectTemplate_RoundTrip(t *testing.T) {
	fb := NewFakeBridge(t)
	data, _ := json.Marshal(RelayProjectTemplateResponse{
		ID:          "ssh-box",
		Name:        "Box SSH",
		Command:     "ssh",
		Args:        []string{"me@box"},
		Env:         map[string]string{"TERM": "xterm"},
		Description: "private",
		Icon:        "shell",
	})
	fb.SetResponse(relayBridgeResponse{Type: respProjectTemplate, Data: data})
	withBridgeEnv(t, fb.SocketPath(), "relay-llm", "svc-token")

	tmpl, err := resolveRelayProjectTemplate("proj-1", "ssh-box")
	if err != nil {
		t.Fatalf("resolveRelayProjectTemplate: %v", err)
	}
	if tmpl.Command != "ssh" || tmpl.Name != "Box SSH" {
		t.Errorf("mapping wrong: %+v", tmpl)
	}
	if len(tmpl.Args) != 1 || tmpl.Args[0] != "me@box" {
		t.Errorf("args not mapped: %+v", tmpl.Args)
	}
	if tmpl.Env["TERM"] != "xterm" {
		t.Errorf("env not mapped: %+v", tmpl.Env)
	}

	reqs := fb.Requests()
	if len(reqs) != 1 {
		t.Fatalf("bridge requests = %d, want 1", len(reqs))
	}
	if reqs[0].Type != reqResolveProjectTemplate {
		t.Errorf("request type = %q, want %q", reqs[0].Type, reqResolveProjectTemplate)
	}
	var got RelayProjectTemplateRequest
	if err := json.Unmarshal(reqs[0].Arguments, &got); err != nil {
		t.Fatalf("decode args: %v", err)
	}
	if got.ProjectID != "proj-1" || got.TemplateID != "ssh-box" {
		t.Errorf("request ids = %+v, want proj-1/ssh-box", got)
	}
}

// A global-store MISS with NO project must fail fast and never touch the bridge:
// an ad-hoc terminal can't carry a private template (global-first precedence).
func TestTerminalCreate_MissNoProject_NoFallback(t *testing.T) {
	store := NewTemplateStore(t.TempDir()) // empty store: every Get misses
	mgr := NewTerminalManager(store, t.TempDir())
	if _, err := mgr.Create("nope", "", t.TempDir(), "", 80, 24, nil); err == nil ||
		!strings.Contains(err.Error(), "terminal template not found") {
		t.Fatalf("err = %v, want 'terminal template not found'", err)
	}
}

// A global-store miss WITH a project but no reachable relay (standalone: no
// service token) must fail closed — the resolve errors out and nothing spawns.
func TestTerminalCreate_ProjectFallback_FailsClosed(t *testing.T) {
	t.Setenv(envServiceToken, "")
	t.Setenv(envServiceTokenLegacy, "")
	store := NewTemplateStore(t.TempDir())
	mgr := NewTerminalManager(store, t.TempDir())
	if _, err := mgr.Create("private-id", "", t.TempDir(), "proj-1", 80, 24, nil); err == nil ||
		!strings.Contains(err.Error(), "resolve project template") {
		t.Fatalf("err = %v, want a 'resolve project template' failure (fail-closed)", err)
	}
}
