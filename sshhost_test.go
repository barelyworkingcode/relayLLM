package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Fixtures pinned byte-for-byte from ../relay/docs/ssh-hosts.md's *Fixtures*
// section. relay, relayLLM and eve each vendor RemoteCommand independently;
// this is what keeps the three implementations from drifting apart.

func TestBuildRemoteScript_Fixture1(t *testing.T) {
	got := buildRemoteScript("/home/a b", []string{"/usr/bin/claude", "--print", "it's"}, nil)
	want := `cd '/home/a b' && exec env '/usr/bin/claude' '--print' 'it'\''s'`
	if got != want {
		t.Errorf("script = %q, want %q", got, want)
	}
}

func TestBuildRemoteScript_Fixture2(t *testing.T) {
	got := buildRemoteScript("", []string{"cat", "/x/y.jsonl"}, map[string]string{"TERM": "xterm-256color"})
	want := `exec env 'TERM'='xterm-256color' 'cat' '/x/y.jsonl'`
	if got != want {
		t.Errorf("script = %q, want %q", got, want)
	}
}

// The launcher wrapping is always exactly this form, standard padded base64,
// no line breaks.
func TestRemoteCommand_LauncherForm(t *testing.T) {
	got := RemoteCommand("/home/a b", []string{"/usr/bin/claude", "--print", "it's"}, nil)

	wantScript := `cd '/home/a b' && exec env '/usr/bin/claude' '--print' 'it'\''s'`
	wantB64 := base64.StdEncoding.EncodeToString([]byte(wantScript))
	want := `sh -c 'eval "$(printf %s ` + wantB64 + ` | base64 -d)"'`

	if got != want {
		t.Errorf("RemoteCommand =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "\n") {
		t.Error("launcher must not contain line breaks")
	}
	// Padded base64: length is a multiple of 4.
	if len(wantB64)%4 != 0 {
		t.Fatalf("test fixture base64 length %d not a multiple of 4", len(wantB64))
	}
}

func TestRemoteCommand_Fixture2Roundtrip(t *testing.T) {
	got := RemoteCommand("", []string{"cat", "/x/y.jsonl"}, map[string]string{"TERM": "xterm-256color"})

	wantScript := `exec env 'TERM'='xterm-256color' 'cat' '/x/y.jsonl'`
	wantB64 := base64.StdEncoding.EncodeToString([]byte(wantScript))
	want := `sh -c 'eval "$(printf %s ` + wantB64 + ` | base64 -d)"'`

	if got != want {
		t.Errorf("RemoteCommand =\n%q\nwant\n%q", got, want)
	}
}

// Env keys are sorted so identical inputs always produce identical bytes,
// regardless of Go's randomized map iteration order.
func TestBuildRemoteScript_EnvKeysSorted(t *testing.T) {
	env := map[string]string{"ZETA": "1", "ALPHA": "2", "MU": "3"}
	got := buildRemoteScript("", []string{"true"}, env)
	want := `exec env 'ALPHA'='2' 'MU'='3' 'ZETA'='1' 'true'`
	if got != want {
		t.Errorf("script = %q, want %q", got, want)
	}
}

func TestSingleQuote_EscapesEmbeddedQuotes(t *testing.T) {
	if got, want := singleQuote(`it's`), `'it'\''s'`; got != want {
		t.Errorf("singleQuote = %q, want %q", got, want)
	}
	if got, want := singleQuote(""), `''`; got != want {
		t.Errorf("singleQuote(\"\") = %q, want %q", got, want)
	}
}

func TestRemoteCommand_NoCwdOmitsCd(t *testing.T) {
	got := RemoteShellCommandDecodedForTest(RemoteCommand("", []string{"true"}, nil))
	if strings.HasPrefix(got, "cd ") {
		t.Errorf("empty cwd must omit the cd prefix, got %q", got)
	}
}

func TestRemoteShellCommand_UnquotedScriptExpandsVerbatim(t *testing.T) {
	decoded := RemoteShellCommandDecodedForTest(RemoteShellCommand("/proj", `exec "$SHELL" -l`))
	want := `cd '/proj' && exec "$SHELL" -l`
	if decoded != want {
		t.Errorf("decoded = %q, want %q", decoded, want)
	}
}

func TestRemoteShellCommand_NoCwd(t *testing.T) {
	decoded := RemoteShellCommandDecodedForTest(RemoteShellCommand("", `exec "$SHELL" -l`))
	want := `exec "$SHELL" -l`
	if decoded != want {
		t.Errorf("decoded = %q, want %q", decoded, want)
	}
}

// RemoteShellCommandDecodedForTest reverses wrapLauncher for assertions —
// production never needs to decode its own launcher, only the host's login
// shell does.
func RemoteShellCommandDecodedForTest(launcher string) string {
	const prefix = `sh -c 'eval "$(printf %s `
	const suffix = ` | base64 -d)"'`
	if !strings.HasPrefix(launcher, prefix) || !strings.HasSuffix(launcher, suffix) {
		panic("not a launcher: " + launcher)
	}
	b64 := launcher[len(prefix) : len(launcher)-len(suffix)]
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		panic(err)
	}
	return string(decoded)
}
