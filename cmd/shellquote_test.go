package cmd

// shellquote_test.go — T16 P1-5 regression tests. The previous implementation
// used Go's `%q` which is NOT shell-safe; this suite documents the contract
// and pins it.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestShellQuote_BasicCases(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"simple", "'simple'"},
		{"with space", "'with space'"},
		{"$VAR", "'$VAR'"},
		{"`whoami`", "'`whoami`'"},
		{"$(rm -rf /)", "'$(rm -rf /)'"},
		{"a\"b", "'a\"b'"},
		{"a\\b", "'a\\b'"},
		{"a!b", "'a!b'"},
		{`it's`, `'it'\''s'`},
		{`'`, `''\'''`},
	}
	for _, tc := range cases {
		got := shellQuote(tc.in)
		if got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestShellQuote_EvalRoundTrip is the canonical end-to-end assertion:
// for every hostile input, `eval` of the quoted form produces back the
// original string. If a shell exists in $PATH we use it; otherwise we fall
// back to the in-process algorithm check above.
func TestShellQuote_EvalRoundTrip(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no /bin/sh available — skipping eval round-trip")
	}

	hostile := []string{
		"plain",
		"with space",
		`it's; rm -rf /`,
		"postgres://u:p@h/db?param=$(id)",
		"redis://default:`whoami`@cache:6379",
		`mongodb://u:!"#$%&'()*+,-./:;<=>?@[\]^_={|}~b/`,
		"control chars \t \r\n end",
	}
	for _, v := range hostile {
		t.Run(strings.SplitN(v, " ", 2)[0], func(t *testing.T) {
			cmd := exec.Command(sh, "-c", "printf %s "+shellQuote(v))
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("eval failed: %v", err)
			}
			if string(out) != v {
				t.Errorf("round-trip mismatch:\n  in:  %q\n  out: %q", v, string(out))
			}
		})
	}
}

// TestShellQuote_NoCommandInjection is the actual security regression for
// T16 P1-5: a value containing `$(...)` must NOT execute on eval.
func TestShellQuote_NoCommandInjection(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no /bin/sh available")
	}

	// If quoting is wrong, `$(echo PWNED)` runs and the output is "PWNED".
	// With correct single-quote quoting the literal string is printed.
	value := `$(echo PWNED)`
	script := "printf %s " + shellQuote(value)
	out, err := exec.Command(sh, "-c", script).Output()
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if string(out) != value {
		t.Fatalf("COMMAND INJECTION: value %q evaluated to %q", value, string(out))
	}
}

// TestShellQuote_EvalAssignsToVariable proves the full `eval $(... --emit-env)`
// flow doesn't break: an `export NAME=<quoted>` line evals cleanly and the
// resulting environment variable contains the original hostile value.
func TestShellQuote_EvalAssignsToVariable(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no /bin/sh available")
	}

	value := `postgres://u:!"#$%&'()*+,-./:;<=>?@[\]^_={|}~b/`
	line := "export X=" + shellQuote(value) + `; printf %s "$X"`
	out, err := exec.Command(sh, "-c", line).Output()
	if err != nil {
		t.Fatalf("eval export failed: %v\nline: %s", err, line)
	}
	if string(out) != value {
		t.Fatalf("export round-trip:\n  in:  %q\n  out: %q", value, string(out))
	}
}

func TestSanitizeExportName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"DATABASE_URL", "DATABASE_URL"},
		{"database_url", "DATABASE_URL"},
		{"app-db", "APP_DB"},
		{"my app", "MY_APP"},
		{"My App DB", "MY_APP_DB"},
		{"app/db", "APP_DB"},
		{"app.db", "APP_DB"},
		{"123db", "R_123DB"},
		{"7", "R_7"},
		{"", ""},
		{"@@@", ""},
	}
	for _, tc := range cases {
		got := sanitizeExportName(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeExportName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsValidShellIdentifier(t *testing.T) {
	valid := []string{"X", "_FOO", "FOO_BAR", "F123", "_1"}
	invalid := []string{"", "1FOO", "foo", "FOO-BAR", "FOO BAR", "FOO.BAR", "$"}
	for _, v := range valid {
		if !isValidShellIdentifier(v) {
			t.Errorf("isValidShellIdentifier(%q) = false, want true", v)
		}
	}
	for _, v := range invalid {
		if isValidShellIdentifier(v) {
			t.Errorf("isValidShellIdentifier(%q) = true, want false", v)
		}
	}
}
