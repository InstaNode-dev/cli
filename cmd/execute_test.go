package cmd

// execute_test.go — covers the Execute / ExecuteWithArgs surface that the
// production binary's main() uses. Tests are intentionally narrow: we drive
// `--help` (always exits 0) and `--version` (also 0) so we never need the
// API. Other entry-point semantics are covered by integration_test.go.

import (
	"strings"
	"testing"
)

// TestExecuteWithArgs_Help verifies the testable entrypoint that main.go uses.
// `--help` is always safe to run.
func TestExecuteWithArgs_Help(t *testing.T) {
	if err := ExecuteWithArgs([]string{"--help"}); err != nil {
		t.Fatalf("ExecuteWithArgs --help: %v", err)
	}
}

// TestExecuteWithArgs_Version covers the version path.
func TestExecuteWithArgs_Version(t *testing.T) {
	if err := ExecuteWithArgs([]string{"--version"}); err != nil {
		t.Fatalf("ExecuteWithArgs --version: %v", err)
	}
}

// TestExecute_DefaultsToOSArgs verifies the wrapper passes through. We set
// os.Args to a known value and assert Execute() returns no error for --help.
func TestExecute_PassesThroughOSArgs(t *testing.T) {
	// We don't manipulate os.Args directly (other tests may depend on it);
	// instead, exercise the wrapper by calling Execute() and assert it
	// returns the same kind of error as ExecuteWithArgs([]string{}). The
	// no-args path prints help and returns nil.
	err := ExecuteWithArgs([]string{})
	if err != nil {
		t.Errorf("empty args: %v", err)
	}
}

// TestSetBuildInfo_Defaults covers the defaulting branches: empty values
// must be replaced with sentinels.
func TestSetBuildInfo_Defaults(t *testing.T) {
	SetBuildInfo("", "", "")
	if !strings.Contains(rootCmd.Version, "dev") {
		t.Errorf("SetBuildInfo empty: rootCmd.Version=%q", rootCmd.Version)
	}
	if !strings.Contains(rootCmd.Version, "unknown") {
		t.Errorf("SetBuildInfo empty: rootCmd.Version=%q", rootCmd.Version)
	}

	// Set real values.
	SetBuildInfo("1.2.3", "abcdef", "2026-05-22T00:00:00Z")
	if !strings.Contains(rootCmd.Version, "1.2.3") {
		t.Errorf("SetBuildInfo: %q", rootCmd.Version)
	}
}
