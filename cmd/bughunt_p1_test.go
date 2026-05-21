package cmd

// bughunt_p1_test.go — regression tests for the P1 findings from BugHunt
// 2026-05-20 (T16) and T10. Each test pins one bug-fix; future regressions
// would re-introduce a documented audit-confirmed bug. Tests are part of the
// hermetic suite (no network), runnable with `go test ./... -race`.
//
// Map of test → finding:
//
//   TestBugHunt_T16_P1_4_UpAbortsOnListFetchFailure       — T16 P1-4
//   TestBugHunt_T16_P1_4_UpAbortsOnListFetch_5xx          — T16 P1-4
//   TestBugHunt_T16_P1_4_UpAbortsOnListFetch_429          — T16 P1-4
//   TestBugHunt_T16_P1_3_ExitCodes_UpResourceFailed       — T16 P1-3
//   TestBugHunt_T16_P1_3_ExitCodes_UpManifestParseError   — T16 P1-3
//   TestBugHunt_T16_P1_3_ExitCodes_UpAuthRequiredForNonProd — T16 P1-3
//   TestBugHunt_T16_P1_5_EmitEnvShellQuotesHostileValues  — T16 P1-5
//   TestBugHunt_T16_P1_5_EmitEnvSanitizesExportName       — T16 P1-5
//   TestBugHunt_T16_P1_1_WhoamiTruncatesKey               — T16 P1-1
//   TestBugHunt_T16_P1_2_UniformExitCodeFor401            — T16 P1-2
//   TestBugHunt_T10_NoStaleInstantDevDomain               — T10 (cli surface)

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InstaNode-dev/cli/internal/cliconfig"
	"github.com/InstaNode-dev/cli/internal/secretstore"
)

// ── T16 P1-4 — `up` MUST NOT silently re-provision when the list fetch
// ── fails. A 401 with a saved token, a 429, or a 5xx all aborted the
// ── reconcile blind before this fix.

// p1_4_fixture sets up `up` against a mock that succeeds at provisioning
// but fails the list-fetch with the given status. We then assert:
//   1. zero resources are created on the mock
//   2. the CLI returned a non-nil error with the documented exit code
func p1_4_fixture(t *testing.T, listStatus int, expectedExitCode int) {
	t.Helper()
	c := newITContext(t)

	// Override the mock handler so GET /api/v1/resources fails.
	c.mock.mu.Lock()
	c.mock.failListStatus = listStatus
	c.mock.mu.Unlock()

	manifest := writeManifest(t, `
env: development
resources:
  - type: postgres
    name: dont-double-up
    export: DB_URL
`)
	resetUpFlags()

	var gotErr error
	captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest)
		gotErr = err
	})

	if gotErr == nil {
		t.Fatalf("up with list-fetch %d MUST error, got nil (would have re-provisioned blind)",
			listStatus)
	}
	if c.mock.count() != 0 {
		t.Errorf("up with list-fetch %d MUST NOT provision; mock holds %d resources (names=%v)",
			listStatus, c.mock.count(), c.mock.names())
	}
	if code := ExitCodeFor(gotErr); code != expectedExitCode {
		t.Errorf("up list-fetch %d: expected exit code %d, got %d (err: %v)",
			listStatus, expectedExitCode, code, gotErr)
	}
}

func TestBugHunt_T16_P1_4_UpAbortsOnListFetch_5xx(t *testing.T) {
	p1_4_fixture(t, http.StatusServiceUnavailable, ExitResourceFailed)
}

func TestBugHunt_T16_P1_4_UpAbortsOnListFetch_429(t *testing.T) {
	p1_4_fixture(t, http.StatusTooManyRequests, ExitResourceFailed)
}

// 401 specifically routes to ExitAuthRequired (session expired) when the
// caller had a saved token. We have to set a token + auth-required state
// on the mock for this path. Anonymous 401 → empty list → no double-up
// either (covered by the up reconcile happy path with no items).
func TestBugHunt_T16_P1_4_UpAbortsOnListFetch_401_AuthenticatedSession(t *testing.T) {
	c := newITContext(t)

	// Put a saved token on disk so haveAuth() == true.
	cfg := &cliconfig.Config{}
	cfg.APIKey = "inst_live_stale_session"
	cfg.Email = "x@example.com"
	cfg.APIBaseURL = c.srv
	if err := cfg.Save(); err != nil {
		t.Fatalf("save cfg: %v", err)
	}
	// Re-init the package HTTP client so it picks up the saved token.
	initConfig()
	APIBaseURL = c.srv
	t.Cleanup(func() {
		_ = cliconfig.Clear()
		_ = secretstore.Delete()
	})

	// Make the mock always return 401 with auth required.
	c.mock.mu.Lock()
	c.mock.requireAuth = true
	c.mock.authToken = "different-from-saved" // saved token mismatches
	c.mock.mu.Unlock()

	manifest := writeManifest(t, `
env: development
resources:
  - type: postgres
    name: dont-double-up-401
    export: DB_URL
`)
	resetUpFlags()

	var gotErr error
	captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest)
		gotErr = err
	})

	if gotErr == nil {
		t.Fatal("up with 401 list-fetch on authed session MUST error")
	}
	if c.mock.count() != 0 {
		t.Errorf("authed 401: no resources should be provisioned, got %d", c.mock.count())
	}
	if code := ExitCodeFor(gotErr); code != ExitAuthRequired {
		t.Errorf("authed 401 list-fetch: expected ExitAuthRequired (%d), got %d (err: %v)",
			ExitAuthRequired, code, gotErr)
	}
}

// ── T16 P1-3 — exit codes match the documented `up` contract.

func TestBugHunt_T16_P1_3_ExitCodes_UpManifestParseError(t *testing.T) {
	newITContext(t)
	resetUpFlags()
	_, _, err := run("up", "--file", filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("missing manifest must error")
	}
	if code := ExitCodeFor(err); code != ExitGeneric {
		t.Errorf("manifest parse error: expected exit %d (ExitGeneric), got %d",
			ExitGeneric, code)
	}
}

func TestBugHunt_T16_P1_3_ExitCodes_UpResourceFailed(t *testing.T) {
	c := newITContext(t)
	// Fail the next provision but let the list succeed (empty list = no
	// existing resources, so up tries to provision).
	c.mock.mu.Lock()
	c.mock.failProvisionStatus = http.StatusServiceUnavailable
	c.mock.mu.Unlock()

	manifest := writeManifest(t, `
env: development
resources:
  - type: postgres
    name: doomed
`)
	resetUpFlags()
	var gotErr error
	captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest)
		gotErr = err
	})

	if gotErr == nil {
		t.Fatal("up with failing provision must error")
	}
	if code := ExitCodeFor(gotErr); code != ExitResourceFailed {
		t.Errorf("resource-failed: expected exit %d (ExitResourceFailed), got %d (err: %v)",
			ExitResourceFailed, code, gotErr)
	}
}

func TestBugHunt_T16_P1_3_ExitCodes_UpAuthRequiredForNonProd(t *testing.T) {
	newITContext(t)
	manifest := writeManifest(t, `
resources:
  - type: postgres
    name: staging-db
`)
	resetUpFlags()
	_, _, err := run("up", "--file", manifest, "--env", "staging")
	if err == nil {
		t.Fatal("up --env=staging without auth must error")
	}
	if code := ExitCodeFor(err); code != ExitAuthRequired {
		t.Errorf("non-prod no-auth: expected exit %d (ExitAuthRequired), got %d (err: %v)",
			ExitAuthRequired, code, err)
	}
}

// ── T16 P1-5 — --emit-env values are POSIX shell-quoted, not Go-%q'd.

func TestBugHunt_T16_P1_5_EmitEnvShellQuotesHostileValues(t *testing.T) {
	c := newITContext(t)

	// Tell the mock to mint connection URLs containing every hostile char
	// %q would mishandle: $, backtick, !, double-quote, embedded space.
	hostile := "postgres://u:p$(echo PWNED)`whoami`!\"@h:5432/db with spaces"
	c.mock.mu.Lock()
	c.mock.connURLOverride = hostile
	c.mock.mu.Unlock()

	manifest := writeManifest(t, `
env: development
resources:
  - type: postgres
    name: hostile-url
    export: HOSTILE_URL
`)
	t.Cleanup(func() {
		c.mock.mu.Lock()
		toks := make([]string, 0, len(c.mock.resources))
		for tok := range c.mock.resources {
			toks = append(toks, tok)
		}
		c.mock.mu.Unlock()
		for _, tok := range toks {
			c.deleteResource(tok)
		}
	})

	resetUpFlags()
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest, "--emit-env")
		if err != nil {
			t.Fatalf("up --emit-env failed: %v", err)
		}
	})

	// The hostile value MUST appear single-quoted, NOT double-quoted-with-
	// backslash-escapes (that's what Go's %q does).
	wantLine := "export HOSTILE_URL=" + shellQuote(hostile)
	if !strings.Contains(stdout, wantLine) {
		t.Fatalf("expected shell-quoted line:\n  want: %s\n  got stdout: %q",
			wantLine, stdout)
	}
	// And the raw "$(echo PWNED)" must not appear UNQUOTED (Go %q would
	// produce \"...$(echo PWNED)...\" which a downstream eval may re-expand
	// because shell still interprets $() inside double quotes).
	if strings.Contains(stdout, `"$(echo PWNED)"`) {
		t.Errorf("--emit-env used double-quotes around a $(...) — eval would execute it!\nstdout: %s", stdout)
	}
}

func TestBugHunt_T16_P1_5_EmitEnvSanitizesExportName(t *testing.T) {
	c := newITContext(t)
	manifest := writeManifest(t, `
env: development
resources:
  - type: postgres
    name: My App DB
`)
	t.Cleanup(func() {
		c.mock.mu.Lock()
		toks := make([]string, 0, len(c.mock.resources))
		for tok := range c.mock.resources {
			toks = append(toks, tok)
		}
		c.mock.mu.Unlock()
		for _, tok := range toks {
			c.deleteResource(tok)
		}
	})

	resetUpFlags()
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest, "--emit-env")
		if err != nil {
			t.Fatalf("up --emit-env failed: %v", err)
		}
	})
	// The previous CLI produced `export MY APP DB_URL=...` (literal space
	// in the name) which is a shell syntax error. The fix sanitizes the
	// space to _.
	if !strings.Contains(stdout, "export MY_APP_DB_URL=") {
		t.Errorf("expected sanitized export name MY_APP_DB_URL, stdout=%q", stdout)
	}
	if strings.Contains(stdout, "export MY APP") {
		t.Errorf("export name MUST NOT contain a literal space: stdout=%q", stdout)
	}
}

// ── T16 P1-1 — whoami must not print >8 chars of the bearer token.

func TestBugHunt_T16_P1_1_WhoamiTruncatesKey(t *testing.T) {
	c := newITContext(t)

	cfg := &cliconfig.Config{}
	cfg.APIKey = "inst_live_VERY_LONG_SECRET_THAT_MUST_NOT_LEAK"
	cfg.Email = "secret-leak-test@example.com"
	cfg.Tier = "hobby"
	cfg.APIBaseURL = c.srv
	if err := cfg.Save(); err != nil {
		t.Fatalf("save cfg: %v", err)
	}
	t.Cleanup(func() {
		_ = cliconfig.Clear()
		_ = secretstore.Delete()
	})

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami")
		if err != nil {
			t.Fatalf("whoami: %v", err)
		}
	})

	// 8 chars + ellipsis IS allowed; 9+ chars of the key is NOT.
	for cut := 9; cut <= len(cfg.APIKey); cut++ {
		if strings.Contains(stdout, cfg.APIKey[:cut]) {
			t.Fatalf("whoami leaks %d chars of the bearer token: %q", cut, stdout)
		}
	}
	// The leading 8 chars must be present so the user can tell which key.
	if !strings.Contains(stdout, cfg.APIKey[:8]) {
		t.Errorf("whoami dropped the leading 8 chars: %q", stdout)
	}
	// The backend label must surface.
	if !strings.Contains(stdout, "Stored:") {
		t.Errorf("whoami: expected 'Stored:' line surfacing the secret backend, got %q", stdout)
	}
}

// TestBugHunt_T16_P1_1_NoPlaintextKeyInConfigFile asserts the bearer token
// is NOT written to ~/.instant-config when the keychain backend is in use.
// With the in-memory secret store wired in tests, the on-disk file must
// contain only the non-secret display fields.
func TestBugHunt_T16_P1_1_NoPlaintextKeyInConfigFile(t *testing.T) {
	c := newITContext(t)

	cfg := &cliconfig.Config{}
	cfg.APIKey = "inst_live_must_NOT_be_on_disk_xyz"
	cfg.Email = "diskcheck@example.com"
	cfg.APIBaseURL = c.srv
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	t.Cleanup(func() {
		_ = cliconfig.Clear()
		_ = secretstore.Delete()
	})

	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".instant-config"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "inst_live_must_NOT_be_on_disk_xyz") {
		t.Fatalf("CRITICAL: bearer token leaked to ~/.instant-config: %s",
			string(data))
	}
	if strings.Contains(string(data), `"api_key"`) {
		t.Fatalf("legacy api_key field must NOT be written; got: %s", string(data))
	}
}

// ── T16 P1-2 — 401 produces a uniform exit code across surfaces.

// TestBugHunt_T16_P1_2_UniformExitCodeFor401 hits the three CLI surfaces
// the audit listed (resources, up, direct provisioning) with a stale-token
// 401 and asserts they all exit with the same documented code.
func TestBugHunt_T16_P1_2_UniformExitCodeFor401(t *testing.T) {
	c := newITContext(t)

	// Saved-but-stale session: a token exists but the mock rejects it.
	cfg := &cliconfig.Config{}
	cfg.APIKey = "inst_live_stale"
	cfg.Email = "stale@example.com"
	cfg.APIBaseURL = c.srv
	if err := cfg.Save(); err != nil {
		t.Fatalf("save cfg: %v", err)
	}
	initConfig()
	APIBaseURL = c.srv
	t.Cleanup(func() {
		_ = cliconfig.Clear()
		_ = secretstore.Delete()
	})

	c.mock.mu.Lock()
	c.mock.requireAuth = true
	c.mock.authToken = "expected-different-token"
	c.mock.mu.Unlock()

	// (a) `resources`
	var resourcesErr error
	captureStdout(t, func() {
		_, _, resourcesErr = run("resources")
	})
	if code := ExitCodeFor(resourcesErr); code != ExitAuthRequired {
		t.Errorf("resources 401: expected ExitAuthRequired (%d), got %d (err: %v)",
			ExitAuthRequired, code, resourcesErr)
	}

	// (b) `db new` (direct provisioning)
	resetProvisionFlags()
	var dbErr error
	captureStdout(t, func() {
		_, _, dbErr = run("db", "new", "--name", "stale-test")
	})
	if code := ExitCodeFor(dbErr); code != ExitAuthRequired {
		t.Errorf("db new 401: expected ExitAuthRequired (%d), got %d (err: %v)",
			ExitAuthRequired, code, dbErr)
	}

	// (c) `up`
	manifest := writeManifest(t, `
env: development
resources:
  - type: postgres
    name: stale-up
`)
	resetUpFlags()
	var upErr error
	captureStdout(t, func() {
		_, _, upErr = run("up", "--file", manifest)
	})
	if code := ExitCodeFor(upErr); code != ExitAuthRequired {
		t.Errorf("up 401: expected ExitAuthRequired (%d), got %d (err: %v)",
			ExitAuthRequired, code, upErr)
	}

	// No resources should have been created on any of those calls.
	if c.mock.count() != 0 {
		t.Errorf("401 path on three surfaces must not provision; mock has %d resources",
			c.mock.count())
	}
}

// ── T10 — no wrong-domain references remain in the CLI surface.
// The audit flagged the server-issued `auth_url` pointing to a different
// company's domain. We grep every file in the CLI repo for that domain
// pattern. The literal we look for is built at runtime so this test file
// itself doesn't trip the scan.
func TestBugHunt_T10_NoStaleInstantDevDomain(t *testing.T) {
	// Construct the wrong-domain literal at runtime (so it doesn't appear
	// as a bare string in this source file).
	wrong := "instant" + "." + "dev"
	correct := "instanode" + "." + "dev"

	root := repoRoot(t)
	files := []string{}
	walkErr := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			// Skip vendor / build artefacts.
			name := info.Name()
			if name == "vendor" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip THIS test file (it constructs the wrong-domain literal at
		// runtime, which would otherwise trip the regex below).
		if strings.HasSuffix(p, "bughunt_p1_test.go") {
			return nil
		}
		if strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".md") ||
			strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") {
			files = append(files, p)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	var leaks []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		// Strip "instanode.dev" matches first; anything left containing
		// the wrong-domain literal is a real leak.
		s := strings.ReplaceAll(string(b), correct, "")
		if strings.Contains(s, wrong) {
			leaks = append(leaks, f)
		}
	}
	if len(leaks) > 0 {
		t.Errorf("T10: %d file(s) reference the wrong domain — must use %q:\n  %s",
			len(leaks), correct, strings.Join(leaks, "\n  "))
	}
}

// repoRoot returns the CLI repo root by walking up from the test binary's
// directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find repo root (no go.mod up from %s)", dir)
	return ""
}
