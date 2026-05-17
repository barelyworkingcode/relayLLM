package main

import (
	"strings"
	"testing"
)

// Real `pi --list-models` output: fixed-width columns, space-padded,
// model column can contain spaces. Header positions matter — provider at
// col 0, model at 11, context at 31.
const piListSample = `provider   model               context  max-out  thinking  images
llama-cpp  Qwen3.6 27B Q4      128K     16.4K    no        no
llama-cpp  Qwen3.6 27B Q6 MTP  128K     16.4K    no        no
llama-cpp  Qwen3.6 MoE 35      128K     16.4K    no        no
anthropic  claude-sonnet-4     200K     16K      yes       yes
openai     gpt-4o              128K     16K      no        yes
`

func TestParsePiListModels_MultiWordNames(t *testing.T) {
	got := parsePiListModels([]byte(piListSample))
	if len(got) != 5 {
		t.Fatalf("want 5 models, got %d: %#v", len(got), got)
	}

	want := map[string]string{
		"pi/llama-cpp/Qwen3.6 27B Q4":     "Pi · llama-cpp",
		"pi/llama-cpp/Qwen3.6 27B Q6 MTP": "Pi · llama-cpp",
		"pi/llama-cpp/Qwen3.6 MoE 35":     "Pi · llama-cpp",
		"pi/anthropic/claude-sonnet-4":    "Pi · anthropic",
		"pi/openai/gpt-4o":                "Pi · openai",
	}
	for _, m := range got {
		group, ok := want[m.Value]
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
		delete(want, m.Value)
	}
	for v := range want {
		t.Errorf("missing model: %q", v)
	}
}

func TestParsePiListModels_SkipsBlankLines(t *testing.T) {
	raw := "\n" + piListSample + "\n\n"
	if got := parsePiListModels([]byte(raw)); len(got) != 5 {
		t.Fatalf("want 5 models, got %d", len(got))
	}
}

func TestParsePiListModels_DedupesIdenticalRows(t *testing.T) {
	raw := strings.Join([]string{
		"provider   model           context  max-out  thinking  images",
		"anthropic  claude-haiku    200K     16K      no        yes",
		"anthropic  claude-haiku    200K     16K      no        yes",
	}, "\n")
	got := parsePiListModels([]byte(raw))
	if len(got) != 1 || got[0].Value != "pi/anthropic/claude-haiku" {
		t.Fatalf("unexpected: %#v", got)
	}
}

func TestParsePiListModels_NoHeader(t *testing.T) {
	// Without the expected header landmarks we can't slice rows; return nil
	// rather than guess.
	raw := "some error message from pi\n"
	if got := parsePiListModels([]byte(raw)); got != nil {
		t.Fatalf("want nil, got %#v", got)
	}
}

func TestParsePiListModels_Empty(t *testing.T) {
	if got := parsePiListModels([]byte("")); got != nil {
		t.Fatalf("want nil, got %#v", got)
	}
}
