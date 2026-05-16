package main

import (
	"strings"
	"testing"
)

func TestParsePiListModels_Tabular(t *testing.T) {
	raw := strings.Join([]string{
		"provider   model                  context  max-out  thinking  images",
		"llama-cpp  Qwen3.6-35B-A3B-UD-Q4  128K     16.4K    no        no",
		"anthropic  claude-sonnet-4-20250514  200K  16K      yes       yes",
		"openai     gpt-4o                 128K     16K      no        yes",
		"",
	}, "\n")

	got := parsePiListModels([]byte(raw))
	if len(got) != 3 {
		t.Fatalf("want 3 models, got %d: %#v", len(got), got)
	}

	wantValues := map[string]string{
		"pi/llama-cpp/Qwen3.6-35B-A3B-UD-Q4":     "Pi · llama-cpp",
		"pi/anthropic/claude-sonnet-4-20250514":  "Pi · anthropic",
		"pi/openai/gpt-4o":                       "Pi · openai",
	}
	for _, m := range got {
		group, ok := wantValues[m.Value]
		if !ok {
			t.Errorf("unexpected model value: %q", m.Value)
			continue
		}
		if m.Group != group {
			t.Errorf("model %q group: got %q want %q", m.Value, m.Group, group)
		}
		if m.Provider != "pi" {
			t.Errorf("model %q provider: got %q want %q", m.Value, m.Provider, "pi")
		}
		delete(wantValues, m.Value)
	}
	for v := range wantValues {
		t.Errorf("missing model: %q", v)
	}
}

func TestParsePiListModels_SkipsHeaderAndBlanks(t *testing.T) {
	raw := "\nprovider model\n\nanthropic claude-haiku\n"
	got := parsePiListModels([]byte(raw))
	if len(got) != 1 || got[0].Value != "pi/anthropic/claude-haiku" {
		t.Fatalf("unexpected: %#v", got)
	}
}

func TestParsePiListModels_Empty(t *testing.T) {
	if got := parsePiListModels([]byte("")); got != nil {
		t.Fatalf("want nil, got %#v", got)
	}
}
