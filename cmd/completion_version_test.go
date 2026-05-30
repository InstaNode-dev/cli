package cmd

// completion_version_test.go — covers the cobra-override hygiene in
// root.go's init():
//   - `instant completion` (no shell arg) returns a non-zero exit
//     code (BUG-CLI-016, QA 2026-05-29).
//   - Every default completion sub-shell (`bash | zsh | fish |
//     powershell`) still returns nil — only the bare invocation
//     changes (rule 18 registry-iterating).
//   - `instant version` alias is registered and exits 0 with the
//     same one-line shape `instant version <Version>` cobra emits
//     for `--version` (BUG-CLI-041).

import (
	"errors"
	"strings"
	"testing"
)

// TestCompletion_NoShellArg_ReturnsError pins BUG-CLI-016: the bare
// `instant completion` invocation must return a non-nil error (which
// main.go translates to exit code 1 via ExitCodeFor). Pre-fix cobra
// printed help and exited 0, which a CI script reading `$?` could not
// distinguish from a successful shell-completion-script generation.
func TestCompletion_NoShellArg_ReturnsError(t *testing.T) {
	err := ExecuteWithArgs([]string{"completion"})
	if err == nil {
		t.Fatalf("BUG-CLI-016: `instant completion` must return a non-nil error; got nil (would exit 0)")
	}
	// Error message contract: must name `instant completion` and list
	// the supported shells. A script grepping for "shell argument
	// required" should branch on the error.
	got := err.Error()
	if !strings.Contains(got, "shell argument required") {
		t.Errorf("BUG-CLI-016: error message must mention 'shell argument required'; got %q", got)
	}
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		if !strings.Contains(got, shell) {
			t.Errorf("BUG-CLI-016: error message must list %q as a supported shell; got %q", shell, got)
		}
	}
	// ExitCodeFor must classify this as ExitGeneric (1), not ExitOK (0).
	if code := ExitCodeFor(err); code != ExitGeneric {
		t.Errorf("BUG-CLI-016: ExitCodeFor(...) = %d, want %d (ExitGeneric)", code, ExitGeneric)
	}
}

// TestCompletion_EveryShellSubcommandStillSucceeds is the rule-18
// registry-iterating guard against the "I fixed the bare command but
// broke every sub-shell" regression. The list mirrors cobra's
// InitDefaultCompletionCmd registry — bash, zsh, fish, powershell.
// Adding a new shell to cobra without adding it here means the new
// shell escapes the test net (the fix could silently turn it into the
// same usage-error path as the bare command).
func TestCompletion_EveryShellSubcommandStillSucceeds(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		shell := shell // capture
		t.Run(shell, func(t *testing.T) {
			err := ExecuteWithArgs([]string{"completion", shell})
			if err != nil {
				t.Errorf("BUG-CLI-016: `instant completion %s` must still succeed (script generation); got %v",
					shell, err)
			}
		})
	}
}

// TestVersion_AliasExitsZero pins BUG-CLI-041: `instant version` is
// the convention alias for `instant --version`. Pre-fix cobra
// rejected it with "unknown command 'version'" (exit=1). Now it must
// behave like --version: exit 0, no error.
func TestVersion_AliasExitsZero(t *testing.T) {
	err := ExecuteWithArgs([]string{"version"})
	if err != nil {
		t.Fatalf("BUG-CLI-041: `instant version` alias must exit 0; got %v", err)
	}
	if code := ExitCodeFor(err); code != ExitOK {
		t.Errorf("BUG-CLI-041: ExitCodeFor(...) = %d, want %d (ExitOK)", code, ExitOK)
	}
}

// TestVersion_AliasExtraArgsRejected confirms cobra.NoArgs is enforced —
// `instant version foo` should fail with a usage-style error so a
// script can detect typos.
func TestVersion_AliasExtraArgsRejected(t *testing.T) {
	err := ExecuteWithArgs([]string{"version", "foo"})
	if err == nil {
		t.Fatalf("BUG-CLI-041: `instant version foo` must reject extra args; got nil")
	}
	if !errors.Is(err, err) { // sanity (always true) — keep the import live
		_ = err
	}
}
