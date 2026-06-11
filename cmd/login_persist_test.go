package cmd

// login_persist_test.go — guards the headless/detached login no-false-success
// contract. The live dogfood bug: `instant login` from a detached/headless box
// printed "✓ Logged in" while persisting NOTHING readable, so the next
// `instant whoami` said "Not logged in". The durable-persist fix lives in
// cliconfig.Save (keychain write-then-readback → file fallback); this file
// pins the runLogin SURFACE: when neither store accepts the token (cfg.Save
// errors), runLogin must NOT print the success banner and must instead hand
// the user the token + an `export INSTANT_TOKEN=…` escape hatch.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogin_SaveFailure_NoFalseSuccess forces cfg.Save() to fail while leaving
// cliconfig.Load() working, by pre-creating the atomic temp-write target
// (~/.instant-config.tmp) as a DIRECTORY: os.WriteFile to that path errors,
// but Load's read of the (absent) ~/.instant-config returns a clean empty
// config. runLogin must then surface a hard error carrying the
// export-INSTANT_TOKEN escape hatch and the token itself, and must NOT print
// the "Logged in as" banner.
func TestLogin_SaveFailure_NoFalseSuccess(t *testing.T) {
	withCleanState(t)

	// HOME is already a fresh temp dir (withCleanState). Block ONLY the Save
	// temp-write by occupying ~/.instant-config.tmp with a directory.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	if err := os.Mkdir(filepath.Join(home, ".instant-config.tmp"), 0700); err != nil {
		t.Fatalf("seed temp-write blocker: %v", err)
	}

	srv := claimLoginServer(t, nil, 0)
	defer srv.Close()
	withTestAPI(t, srv.URL)

	var runErr error
	stdout, _ := captureStdout(t, func() {
		runErr = runLogin(nil, nil)
	})

	if runErr == nil {
		t.Fatalf("expected runLogin to error when credentials can't be saved; stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, loggedInPrefix) {
		t.Errorf("runLogin printed the success banner despite a failed save (false success); stdout:\n%s", stdout)
	}
	msg := runErr.Error()
	if !strings.Contains(msg, "export INSTANT_TOKEN=") {
		t.Errorf("error must offer the INSTANT_TOKEN escape hatch; got: %v", runErr)
	}
	if !strings.Contains(msg, claimTestAPIKey) {
		t.Errorf("error must include the token so a headless agent can proceed; got: %v", runErr)
	}
}
