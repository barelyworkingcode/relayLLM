package main

import (
	"encoding/base64"
	"sort"
	"strings"
)

// This file vendors relay's internal/sshhost verbatim (../relay/docs/ssh-hosts.md
// decision 8): the remote-command construction that makes an SSH host's login
// shell — sh, bash, zsh or fish — parse the exact same bytes regardless of
// which one it is. relay, relayLLM and eve each own one copy; the doc's
// Fixtures section is what keeps all three byte-identical.

// singleQuote escapes s for embedding inside a single-quoted POSIX-sh word:
// close the quote, emit a literal quote via a new single-quoted string with an
// escaped one, reopen. This is the only shell-quoting rule sh/bash/zsh/fish
// agree on, which is why the entire remote command line is built out of
// single-quoted tokens rather than any shell's escaping conventions.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellQuoteJoin single-quotes each of parts and joins them with spaces,
// producing one POSIX-sh command line.
func shellQuoteJoin(parts []string) string {
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = singleQuote(p)
	}
	return strings.Join(quoted, " ")
}

// buildRemoteScript renders the decoded POSIX-sh script: `cd '<cwd>' &&`
// (omitted when cwd is empty) followed by `exec env 'K'='v' … '<argv0>' '<arg1>' …`,
// every value single-quoted. Env keys are sorted so identical inputs always
// produce identical bytes — required for the pinned fixtures in
// ../relay/docs/ssh-hosts.md and for the sec guards that grep this output for
// leaked secrets.
func buildRemoteScript(cwd string, argv []string, env map[string]string) string {
	var b strings.Builder
	if cwd != "" {
		b.WriteString("cd ")
		b.WriteString(singleQuote(cwd))
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
		b.WriteString(singleQuote(k))
		b.WriteByte('=')
		b.WriteString(singleQuote(env[k]))
	}

	for _, a := range argv {
		b.WriteByte(' ')
		b.WriteString(singleQuote(a))
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
	return wrapLauncher(buildRemoteScript(cwd, argv, env))
}

// RemoteShellCommand wraps a raw POSIX-sh script fragment — one the caller
// wants the host's login shell to expand verbatim, e.g. `exec "$SHELL" -l` —
// with cwd's `cd` prefix and the same base64 launcher RemoteCommand uses.
// Unlike RemoteCommand's argv, script is not single-quoted: the caller is
// responsible for whatever quoting the fragment itself needs.
func RemoteShellCommand(cwd, script string) string {
	full := script
	if cwd != "" {
		full = "cd " + singleQuote(cwd) + " && " + script
	}
	return wrapLauncher(full)
}
