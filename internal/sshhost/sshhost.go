// Package sshhost vendors relay's internal/sshhost verbatim
// (../relay/docs/ssh-hosts.md decision 8): the remote-command construction
// that makes an SSH host's login shell — sh, bash, zsh or fish — parse the
// exact same bytes regardless of which one it is. relay, relayLLM and eve
// each own one copy; the doc's Fixtures section is what keeps all three
// byte-identical.
package sshhost

import (
	"encoding/base64"
	"sort"
	"strings"
)

// SingleQuote escapes s for embedding inside a single-quoted POSIX-sh word:
// close the quote, emit a literal quote via a new single-quoted string with an
// escaped one, reopen. This is the only shell-quoting rule sh/bash/zsh/fish
// agree on, which is why the entire remote command line is built out of
// single-quoted tokens rather than any shell's escaping conventions.
func SingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellQuoteJoin single-quotes each of parts and joins them with spaces,
// producing one POSIX-sh command line.
func ShellQuoteJoin(parts []string) string {
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = SingleQuote(p)
	}
	return strings.Join(quoted, " ")
}

// BuildRemoteScript renders the decoded POSIX-sh script: `cd '<cwd>' &&`
// (omitted when cwd is empty) followed by `exec env 'K'='v' … '<argv0>' '<arg1>' …`,
// every value single-quoted. Env keys are sorted so identical inputs always
// produce identical bytes — required for the pinned fixtures in
// ../relay/docs/ssh-hosts.md and for the sec guards that grep this output for
// leaked secrets.
func BuildRemoteScript(cwd string, argv []string, env map[string]string) string {
	var b strings.Builder
	if cwd != "" {
		b.WriteString("cd ")
		b.WriteString(SingleQuote(cwd))
		b.WriteString(" && ")
	}
	b.WriteString("exec env")

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte(' ')
		b.WriteString(SingleQuote(k))
		b.WriteByte('=')
		b.WriteString(SingleQuote(env[k]))
	}

	for _, a := range argv {
		b.WriteByte(' ')
		b.WriteString(SingleQuote(a))
	}
	return b.String()
}

// wrapLauncher base64-encodes script and wraps it in the fixed launcher form
// every remote command uses (decision 8). Standard (padded) base64, no line
// breaks: the only characters the destination's login shell ever parses are
// [A-Za-z0-9+/=] inside a single-quoted string, which every shell treats
// identically — that is what makes this form shell-agnostic.
func wrapLauncher(script string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return `sh -c 'eval "$(printf %s ` + encoded + ` | base64 -d)"'`
}

// RemoteCommand builds the shell-agnostic remote command line that runs argv
// (with cwd as the working directory and env as additional environment) on an
// SSH host's login shell, whichever shell that is. Callers append this as the
// single trailing argument after `ssh_argv... -T|-tt --`.
func RemoteCommand(cwd string, argv []string, env map[string]string) string {
	return wrapLauncher(BuildRemoteScript(cwd, argv, env))
}

// RemoteShellCommand wraps a raw POSIX-sh script fragment — one the caller
// wants the host's login shell to expand verbatim, e.g. `exec "$SHELL" -l` —
// with cwd's `cd` prefix and the same base64 launcher RemoteCommand uses.
// Unlike RemoteCommand's argv, script is not single-quoted: the caller is
// responsible for whatever quoting the fragment itself needs.
func RemoteShellCommand(cwd, script string) string {
	full := script
	if cwd != "" {
		full = "cd " + SingleQuote(cwd) + " && " + script
	}
	return wrapLauncher(full)
}

// RemoteShellCommandDecodedForTest reverses wrapLauncher for assertions —
// production never needs to decode its own launcher, only the host's login
// shell does. Exported (not a _test.go helper) so callers outside this
// package's own tests — relayLLM's provider/terminal test suites — can
// assert against the decoded script without duplicating wrapLauncher's
// format.
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
