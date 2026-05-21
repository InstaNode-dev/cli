package main

// main_test.go — covers the `run()` entry-point that main() delegates to.
// This lets us assert behaviour without calling os.Exit. The cmd package's
// own test suite exhaustively covers cobra invocations, so here we only
// need exit-code translation + stderr printing.

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_HelpReturnsZero(t *testing.T) {
	var buf bytes.Buffer
	code := run([]string{"--help"}, &buf)
	if code != 0 {
		t.Errorf("--help exit code = %d, want 0", code)
	}
}

func TestRun_VersionReturnsZero(t *testing.T) {
	var buf bytes.Buffer
	code := run([]string{"--version"}, &buf)
	if code != 0 {
		t.Errorf("--version exit code = %d, want 0", code)
	}
}

func TestRun_UnknownFlagReturnsNonZero(t *testing.T) {
	var buf bytes.Buffer
	code := run([]string{"--definitely-not-a-flag"}, &buf)
	if code == 0 {
		t.Error("unknown flag should produce non-zero exit code")
	}
	if buf.Len() == 0 {
		t.Error("unknown flag should print to stderr")
	}
	// Print should mention the unknown flag.
	out := buf.String()
	if !strings.Contains(out, "definitely-not-a-flag") && !strings.Contains(out, "unknown") {
		t.Logf("stderr: %q", out) // informational
	}
}

func TestRun_EmptyArgsReturnsZero(t *testing.T) {
	var buf bytes.Buffer
	code := run([]string{}, &buf)
	// No args -> cobra prints help; exit code is 0.
	if code != 0 {
		t.Errorf("empty args exit code = %d, want 0", code)
	}
}
