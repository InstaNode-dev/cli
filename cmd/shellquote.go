package cmd

// shellquote.go — POSIX shell-safe quoting for the `--emit-env` path of
// `instant up`. The previous implementation used Go's %q verb which produces
// a DOUBLE-quoted Go string — that happens to coincide with shell quoting
// for boring values but is INSECURE for hostile ones: a connection URL
// containing `$(...)`, backticks, `!`, or an embedded `"` was either a parse
// error or a command-injection vector via `eval $(instant up --emit-env)`.
//
// POSIX shell single-quotes are the safe choice:
//
//   - Everything between '...' is literal — no $, no `, no \, no !.
//   - The ONE thing that needs escaping is the single quote itself, which
//     POSIX does by closing the quoted run, emitting an escaped quote
//     (\'), and reopening: `it's` becomes `'it'\''s'`.
//
// This is the same algorithm shellwords / Python shlex.quote / Bash
// `printf %q` (modulo Bash-specific extensions) implement.

import (
	"strings"
)

// shellQuote returns a POSIX-shell-safe quoted form of s, suitable for
// inclusion on a line that will be evaluated by `eval` or sourced by `sh`.
//
//	shellQuote("")              -> "''"
//	shellQuote("simple")        -> "'simple'"
//	shellQuote("with space")    -> "'with space'"
//	shellQuote("it's a $TEST")  -> "'it'\\''s a $TEST'"   (literal $TEST)
//	shellQuote("`whoami`")      -> "'`whoami`'"           (literal backticks)
//
// The returned string is always wrapped in single quotes so a downstream
// `eval` cannot interpret the contents. Use this for any value the CLI
// emits as part of `export NAME=...`.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// Escape embedded single quotes via the POSIX idiom: close the quoted
	// run, emit an escaped quote, reopen.
	if !strings.ContainsRune(s, '\'') {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\'' {
			b.WriteString(`'\''`)
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// isValidShellIdentifier reports whether name is a syntactically valid
// POSIX shell variable name: starts with [A-Z_], followed by [A-Z0-9_].
// We restrict to UPPER_SNAKE_CASE because that's the convention for
// environment variables and refusing mixed-case is a forcing function that
// surfaces typos.
func isValidShellIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		ok := r == '_' || (r >= 'A' && r <= 'Z')
		if i > 0 {
			ok = ok || (r >= '0' && r <= '9')
		}
		if !ok {
			return false
		}
	}
	return true
}

// sanitizeExportName turns a user-provided manifest resource name into a
// valid shell identifier. The rules:
//
//   - lowercase letters become uppercase
//   - dashes, spaces, dots, slashes become underscores
//   - a leading digit gets a `R_` prefix (resource_)
//   - everything else is dropped
//
// If the result is empty after sanitization, we return "" so the caller
// can choose to error rather than emit an invalid `export = ...` line.
func sanitizeExportName(name string) string {
	var b strings.Builder
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteString("R_")
			}
			b.WriteRune(r)
		case r == '_', r == '-', r == ' ', r == '.', r == '/':
			b.WriteByte('_')
		}
		// anything else (punctuation, unicode) is dropped
	}
	return b.String()
}
