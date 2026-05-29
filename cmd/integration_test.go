package cmd

// integration_test.go — MANDATORY end-to-end integration suite for the
// instant CLI. It drives the real cobra command tree (rootCmd / dbCmd / upCmd
// / resourcesCmd / ...) exactly as a user's shell would, against the hermetic
// stateful mock API in testapi_test.go. No network access is required, so the
// whole suite runs in CI with zero external dependencies.
//
// Coverage (every command + subcommand the CLI exposes):
//   db new / cache new / nosql new / queue new   — provisioning, all 4 types
//   up                                           — manifest reconcile (+ flags)
//   up --dry-run / --emit-env / --env / --file    — flag parsing
//   resources                                    — API list
//   status                                       — local token store
//   whoami / login / logout / upgrade            — auth surface
//
// Rigor: every test asserts exit code (error vs nil), output shape (stdout /
// stderr substrings), error handling (bad flags, server errors, missing auth),
// and the resolved-env default (empty env => "development", CLAUDE.md rule 11).
//
// RESOURCE CLEANUP IS MANDATORY. Every test that provisions a resource:
//   1. defers a per-resource teardown that DELETEs it from the mock, AND
//   2. is wrapped by the suite-level TestMain sweep which fails the run if
//      ANY resource is left behind on the mock API.
// A test that provisions and does not clean up is therefore a hard failure.

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InstaNode-dev/cli/internal/secretstore"
	"github.com/InstaNode-dev/cli/internal/tokens"
	"github.com/spf13/cobra"
)

// ── suite-level sweep ───────────────────────────────────────────────────────

// activeMock is the mock API in use by the currently running integration
// test. The cleanup sweep inspects it after each provisioning test.
var activeMock *mockAPI

// TestMain isolates HOME (so the CLI never touches the developer's real
// ~/.instant-config / ~/.instant-tokens) and runs the suite.
func TestMain(m *testing.M) {
	tmpHome, err := os.MkdirTemp("", "instant-cli-itest-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, "itest: cannot create temp HOME:", err)
		os.Exit(1)
	}
	// Both HOME and (Windows) USERPROFILE — os.UserHomeDir consults both.
	os.Setenv("HOME", tmpHome)
	os.Setenv("USERPROFILE", tmpHome)
	// Never let an ambient token leak into the hermetic suite.
	os.Unsetenv("INSTANT_TOKEN")
	os.Unsetenv("INSTANT_API_URL")
	// Pin the secret backend to an in-memory store so cliconfig.Save /
	// .Load never touch the developer's OS keychain. Also disable the
	// real-keychain probe (no DBus / Security framework calls).
	os.Setenv("INSTANT_DISABLE_KEYCHAIN", "1")
	secretstore.UseMemoryBackend()

	code := m.Run()
	_ = os.RemoveAll(tmpHome)
	os.Exit(code)
}

// itContext bundles everything one integration test needs.
type itContext struct {
	t    *testing.T
	mock *mockAPI
	srv  string // base URL
}

// newITContext spins up a fresh hermetic API, points the CLI at it, isolates
// the token store, and registers the MANDATORY end-of-test cleanup sweep.
func newITContext(t *testing.T) *itContext {
	t.Helper()
	mock, srv := newMockAPI(t)
	activeMock = mock

	// Point the package-global CLI client at the mock.
	prevURL, prevClient := APIBaseURL, HTTPClient
	APIBaseURL = srv.URL
	HTTPClient = &http.Client{Timeout: 5 * time.Second}

	// Fresh per-test token store: HOME is already a temp dir (TestMain), but
	// delete any ~/.instant-tokens left by a previous test in the same run.
	home, _ := os.UserHomeDir()
	_ = os.Remove(filepath.Join(home, ".instant-tokens"))
	_ = os.Remove(filepath.Join(home, ".instant-config"))
	// Reset the in-memory secret store so a token saved by a previous test
	// doesn't leak into "I'm anonymous" assertions.
	secretstore.UseMemoryBackend()
	// Clear cobra's retained --json flag values from any prior test.
	resetJSONFlags()

	t.Cleanup(func() {
		APIBaseURL, HTTPClient = prevURL, prevClient
		// MANDATORY SWEEP: delete every resource still held by the mock and
		// fail the test if any survived its own defer-cleanup.
		leaked := mock.count()
		if leaked > 0 {
			names := mock.names()
			// Sweep them so the next test starts clean even on failure.
			mock.mu.Lock()
			for k := range mock.resources {
				delete(mock.resources, k)
			}
			mock.mu.Unlock()
			t.Errorf("RESOURCE LEAK: %d resource(s) not cleaned up by test: %v", leaked, names)
		}
		activeMock = nil
	})
	return &itContext{t: t, mock: mock, srv: srv.URL}
}

// deleteResource is the cleanup primitive: it issues DELETE /api/v1/resources/:token
// against the mock, mirroring what a real teardown would call. Every test that
// provisions MUST defer this for each token it created.
func (c *itContext) deleteResource(token string) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodDelete, c.srv+"/api/v1/resources/"+token, nil)
	if err != nil {
		c.t.Logf("cleanup: build request: %v", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Logf("cleanup: delete %s: %v", token, err)
		return
	}
	_ = resp.Body.Close()
}

// run executes a CLI invocation against the real rootCmd tree and captures
// stdout+stderr separately plus the error (== non-zero exit).
func run(args ...string) (stdout, stderr string, err error) {
	var outBuf, errBuf bytes.Buffer
	rootCmd.SetArgs(args)
	rootCmd.SetOut(&outBuf)
	rootCmd.SetErr(&errBuf)
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true
	err = rootCmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

// captureStdout redirects os.Stdout/os.Stderr for the duration of fn. The CLI
// prints provisioning output with fmt.Printf (os.Stdout) and progress lines
// with fmt.Fprintf(os.Stderr, ...), so command-tree SetOut/SetErr is not
// enough — we must capture the OS-level streams too.
func captureStdout(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr

	done := make(chan [2]string, 1)
	go func() {
		var ob, eb bytes.Buffer
		oc := make(chan struct{})
		go func() { _, _ = ob.ReadFrom(rOut); close(oc) }()
		_, _ = eb.ReadFrom(rErr)
		<-oc
		done <- [2]string{ob.String(), eb.String()}
	}()

	fn()

	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout, os.Stderr = origOut, origErr
	res := <-done
	return res[0], res[1]
}

// provisionViaCLI runs `instant <group> new --name <name>` through the real
// command tree, captures output, returns the saved token, and registers a
// MANDATORY deferred teardown for that token.
func (c *itContext) provisionViaCLI(group, name string) (out string, token string) {
	c.t.Helper()
	stdout, _ := captureStdout(c.t, func() {
		_, _, err := run(group, "new", "--name", name)
		if err != nil {
			c.t.Fatalf("provision %s/%s failed: %v", group, name, err)
		}
	})
	// The CLI saved the token to the local store; read it back.
	tok := lastSavedToken(c.t)
	if tok == "" {
		c.t.Fatalf("provision %s/%s: no token persisted to local store", group, name)
	}
	c.t.Cleanup(func() { c.deleteResource(tok) })
	return stdout, tok
}

// lastSavedToken returns the most-recently saved token from ~/.instant-tokens.
func lastSavedToken(t *testing.T) string {
	t.Helper()
	store, err := tokens.Load()
	if err != nil || len(store.Entries) == 0 {
		return ""
	}
	return store.Entries[len(store.Entries)-1].Token
}

// resetProvisionFlags clears the global --name + --env flags between table
// cases. makeProvisionCmd binds the package-globals resourceName and
// resourceEnv; cobra retains the last-parsed value, so a follow-up test
// could see stale state. All seven provisioning groups (db / cache / nosql
// / queue / storage / webhook / vector) are reset.
func resetProvisionFlags() {
	resourceName = ""
	resourceEnv = ""
	for _, group := range []*cobra.Command{dbCmd, cacheCmd, nosqlCmd, queueCmd, storageCmd, webhookCmd, vectorCmd} {
		for _, sub := range group.Commands() {
			_ = sub.Flags().Set("name", "")
			// --env is optional so it may not be bound on older builds; the
			// Set call is best-effort and silently no-ops on a missing flag.
			if fl := sub.Flags().Lookup("env"); fl != nil {
				_ = fl.Value.Set("")
				fl.Changed = false
			}
		}
	}
}

// ── provisioning: db / cache / nosql / queue ────────────────────────────────

func TestIntegration_ProvisionAllTypes(t *testing.T) {
	c := newITContext(t)

	cases := []struct {
		group     string
		typeLabel string
	}{
		{"db", "db"},
		{"cache", "cache"},
		{"nosql", "nosql"},
		{"queue", "queue"},
	}
	for _, tc := range cases {
		t.Run(tc.group, func(t *testing.T) {
			resetProvisionFlags()
			out, token := c.provisionViaCLI(tc.group, "app-"+tc.group)

			if !strings.Contains(out, "ok") {
				t.Errorf("%s: expected 'ok' line in output, got: %q", tc.group, out)
			}
			if !strings.Contains(out, tc.typeLabel) {
				t.Errorf("%s: expected type label %q in output, got: %q", tc.group, tc.typeLabel, out)
			}
			if !strings.Contains(out, token) {
				t.Errorf("%s: expected token %q in output, got: %q", tc.group, token, out)
			}
			if !strings.Contains(out, "url") {
				t.Errorf("%s: expected a 'url' line, got: %q", tc.group, out)
			}
			// The resource must now exist on the server.
			if c.mock.count() == 0 {
				t.Errorf("%s: server has no resource after provision", tc.group)
			}
		})
	}
}

// TestIntegration_ProvisionMissingName asserts a missing --name fails with a
// non-zero exit and never reaches the API.
func TestIntegration_ProvisionMissingName(t *testing.T) {
	c := newITContext(t)
	for _, group := range []string{"db", "cache", "nosql", "queue"} {
		t.Run(group, func(t *testing.T) {
			resetProvisionFlags()
			_, stderr, err := run(group, "new")
			if err == nil {
				t.Fatalf("%s new without --name: expected error, got nil", group)
			}
			combined := strings.ToLower(stderr + err.Error())
			if !strings.Contains(combined, "name") {
				t.Errorf("%s: error should mention 'name': %q", group, combined)
			}
			if c.mock.count() != 0 {
				t.Errorf("%s: API was called despite missing --name", group)
			}
		})
	}
}

// TestIntegration_ProvisionInvalidName asserts a syntactically invalid name is
// rejected locally — before any API round trip.
func TestIntegration_ProvisionInvalidName(t *testing.T) {
	c := newITContext(t)
	bad := []string{"-leading-dash", " leading-space", "has/slash", strings.Repeat("x", 65)}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			resetProvisionFlags()
			_, _, err := run("db", "new", "--name", name)
			if err == nil {
				t.Fatalf("invalid name %q: expected error, got nil", name)
			}
			if c.mock.count() != 0 {
				t.Errorf("invalid name %q: API was called", name)
			}
		})
	}
}

// TestIntegration_ProvisionServerError asserts a 5xx from the API surfaces as
// a non-zero exit with the server status in the message.
func TestIntegration_ProvisionServerError(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	c.mock.mu.Lock()
	c.mock.failProvisionStatus = http.StatusServiceUnavailable
	c.mock.mu.Unlock()

	_, _, err := run("db", "new", "--name", "doomed-db")
	if err == nil {
		t.Fatal("expected error on 503, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention status 503, got: %q", err.Error())
	}
	if c.mock.count() != 0 {
		t.Error("failed provision must not leave a resource on the server")
	}
}

// TestIntegration_ProvisionResolvedEnvDefault asserts the resolved-env default:
// a provision with no env lands the resource in "development" (CLAUDE.md
// rule 11 — the lowest-stakes bucket).
func TestIntegration_ProvisionResolvedEnvDefault(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	_, token := c.provisionViaCLI("db", "env-default-db")

	c.mock.mu.Lock()
	res := c.mock.resources[token]
	c.mock.mu.Unlock()
	if res == nil {
		t.Fatalf("resource %s not found on server", token)
	}
	if res.Env != "development" {
		t.Errorf("resolved-env default: want %q, got %q", "development", res.Env)
	}
}

// ── up: manifest reconcile ──────────────────────────────────────────────────

// writeManifest writes an instant.yaml into a temp dir and returns its path.
func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "instant.yaml")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return p
}

// resetUpFlags clears `up` flag globals between cases.
func resetUpFlags() {
	upFile, upEnv, upEmitEnv, upDryRun = "instant.yaml", "", false, false
	for _, f := range []string{"file", "env", "emit-env", "dry-run"} {
		if fl := upCmd.Flags().Lookup(f); fl != nil {
			_ = fl.Value.Set(fl.DefValue)
			fl.Changed = false
		}
	}
}

// resetJSONFlags clears the --json flag globals for resources/status/whoami/
// resource between cases. Cobra retains the last-parsed value otherwise,
// which would leak the JSON output mode from one test into a following
// human-readable expectation.
//
// B15-P2: also resets --filter / --limit on resources and the persistent
// --token on the root so a previous test's auth-override doesn't bleed
// into the next case.
func resetJSONFlags() {
	resourcesJSON = false
	statusJSON = false
	whoamiJSON = false
	resourceDetailJSON = false
	resourceDeleteYes = false
	resourcesFilter = nil
	resourcesLimit = 0
	adHocToken = ""
	for _, c := range []struct {
		cmd  string
		flag string
	}{
		{"resources", "json"},
		{"resources", "filter"},
		{"resources", "limit"},
		{"status", "json"},
		{"whoami", "json"},
		{"resource", "json"},
		{"resource", "yes"},
	} {
		if base := rootCmd.Commands(); base != nil {
			for _, sub := range base {
				if sub.Use == c.cmd || strings.HasPrefix(sub.Use, c.cmd+" ") || strings.HasPrefix(sub.Use, c.cmd+" |") {
					if fl := sub.Flags().Lookup(c.flag); fl != nil {
						// StringArray flags carry DefValue="[]" which the
						// pflag setter parses as a single-element slice
						// `["[]"]` — not empty! Reset directly via the
						// underlying SliceValue (pflag.SliceValue.Replace
						// with nil) when available, falling back to the
						// generic Set for scalars. We also clear .Changed
						// so cobra doesn't think the user-provided default
						// was explicitly set in this run.
						if sv, ok := fl.Value.(interface{ Replace([]string) error }); ok {
							_ = sv.Replace(nil)
						} else {
							_ = fl.Value.Set(fl.DefValue)
						}
						fl.Changed = false
					}
				}
			}
		}
	}
	// Also reset the persistent --token at the root.
	if fl := rootCmd.PersistentFlags().Lookup("token"); fl != nil {
		_ = fl.Value.Set(fl.DefValue)
		fl.Changed = false
	}
}

func TestIntegration_UpProvisionsAndReconciles(t *testing.T) {
	c := newITContext(t)
	// T16 P2-3 — env=development is now the platform default for anonymous
	// runs (CLAUDE.md rule 11). Any other env requires auth; tests that do
	// not establish a session must use the default env to exercise the
	// happy path.
	manifest := writeManifest(t, `
env: development
resources:
  - type: postgres
    name: up-db
    export: DATABASE_URL
  - type: redis
    name: up-cache
    export: REDIS_URL
`)
	// MANDATORY cleanup: sweep everything `up` provisioned.
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
	stdout, stderr := captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest)
		if err != nil {
			t.Fatalf("up failed: %v", err)
		}
	})

	if c.mock.count() != 2 {
		t.Errorf("up: expected 2 resources provisioned, got %d", c.mock.count())
	}
	if !strings.Contains(stdout, "export DATABASE_URL=") {
		t.Errorf("up: expected export DATABASE_URL line, stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "export REDIS_URL=") {
		t.Errorf("up: expected export REDIS_URL line, stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "PROVISION") {
		t.Errorf("up: expected PROVISION action line on stderr, stderr=%q", stderr)
	}

	// Second run = idempotent reconcile. Existing resources are REUSEd, not
	// re-provisioned, so the count stays at 2.
	resetUpFlags()
	stdout2, stderr2 := captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest)
		if err != nil {
			t.Fatalf("up (2nd run) failed: %v", err)
		}
	})
	if c.mock.count() != 2 {
		t.Errorf("up reconcile: expected still 2 resources, got %d", c.mock.count())
	}
	if !strings.Contains(stderr2, "REUSE") {
		t.Errorf("up reconcile: expected REUSE action on 2nd run, stderr=%q", stderr2)
	}
	_ = stdout2
}

func TestIntegration_UpDryRun(t *testing.T) {
	c := newITContext(t)
	manifest := writeManifest(t, `
resources:
  - type: postgres
    name: dry-db
`)
	resetUpFlags()
	_, stderr := captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest, "--dry-run")
		if err != nil {
			t.Fatalf("up --dry-run failed: %v", err)
		}
	})
	if c.mock.count() != 0 {
		t.Errorf("up --dry-run must NOT provision; got %d resources", c.mock.count())
	}
	if !strings.Contains(stderr, "PLAN") {
		t.Errorf("up --dry-run: expected PLAN line, stderr=%q", stderr)
	}
}

func TestIntegration_UpEmitEnv(t *testing.T) {
	c := newITContext(t)
	manifest := writeManifest(t, `
resources:
  - type: postgres
    name: emit-db
    export: PG_URL
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
	// In --emit-env mode, stdout must be ONLY export lines (eval-safe).
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "export ") {
			t.Errorf("up --emit-env: non-export line on stdout: %q", line)
		}
	}
	if !strings.Contains(stdout, "export PG_URL=") {
		t.Errorf("up --emit-env: expected export PG_URL, stdout=%q", stdout)
	}
}

// TestIntegration_UpMissingManifest asserts a missing file is a clean error
// (exit code 1 path), not a panic.
func TestIntegration_UpMissingManifest(t *testing.T) {
	newITContext(t)
	resetUpFlags()
	_, _, err := run("up", "--file", filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("up with missing manifest: expected error, got nil")
	}
}

// TestIntegration_UpUnknownResourceType asserts an unknown type in the
// manifest fails reconciliation (the `validate` path).
func TestIntegration_UpUnknownResourceType(t *testing.T) {
	c := newITContext(t)
	manifest := writeManifest(t, `
resources:
  - type: bogus
    name: weird
`)
	resetUpFlags()
	_, stderr := captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest)
		if err == nil {
			t.Fatal("up with unknown type: expected error, got nil")
		}
	})
	if !strings.Contains(stderr, "ERROR") {
		t.Errorf("up unknown type: expected ERROR line, stderr=%q", stderr)
	}
	if c.mock.count() != 0 {
		t.Error("up unknown type must not provision anything")
	}
}

// TestIntegration_UpNonProdEnvRequiresAuth asserts the local auth pre-check:
// a non-production env without a token fails fast with a helpful message and
// never calls the API.
func TestIntegration_UpNonProdEnvRequiresAuth(t *testing.T) {
	c := newITContext(t)
	manifest := writeManifest(t, `
resources:
  - type: postgres
    name: staging-db
`)
	resetUpFlags()
	_, _, err := run("up", "--file", manifest, "--env", "staging")
	if err == nil {
		t.Fatal("up --env=staging without auth: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "INSTANT_TOKEN") {
		t.Errorf("up non-prod: error should mention INSTANT_TOKEN, got %q", err.Error())
	}
	if c.mock.count() != 0 {
		t.Error("up non-prod without auth must not call the API")
	}
}

// TestIntegration_UpWebhookReceiveURL asserts the webhook special case:
// /webhook/new returns receive_url (no connection_url) and `up` emits it.
func TestIntegration_UpWebhookReceiveURL(t *testing.T) {
	c := newITContext(t)
	manifest := writeManifest(t, `
resources:
  - type: webhook
    name: hook
    export: HOOK_URL
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
			t.Fatalf("up webhook failed: %v", err)
		}
	})
	if !strings.Contains(stdout, "export HOOK_URL=") {
		t.Errorf("up webhook: expected export HOOK_URL, stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "hooks.instanode.dev") {
		t.Errorf("up webhook: expected receive_url value, stdout=%q", stdout)
	}
}

// ── resources (API list) ────────────────────────────────────────────────────

func TestIntegration_ResourcesLists(t *testing.T) {
	c := newITContext(t)
	// Provision two resources first, with mandatory teardown.
	resetProvisionFlags()
	_, tok1 := c.provisionViaCLI("db", "list-db")
	resetProvisionFlags()
	_, tok2 := c.provisionViaCLI("cache", "list-cache")

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resources")
		if err != nil {
			t.Fatalf("resources failed: %v", err)
		}
	})
	if !strings.Contains(stdout, "TYPE") || !strings.Contains(stdout, "NAME") {
		t.Errorf("resources: expected table header, got %q", stdout)
	}
	if !strings.Contains(stdout, "list-db") || !strings.Contains(stdout, "list-cache") {
		t.Errorf("resources: expected both resource names, got %q", stdout)
	}
	_, _ = tok1, tok2
}

func TestIntegration_ResourcesEmpty(t *testing.T) {
	newITContext(t)
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resources")
		if err != nil {
			t.Fatalf("resources (empty) failed: %v", err)
		}
	})
	if !strings.Contains(stdout, "No resources") {
		t.Errorf("resources empty: expected 'No resources', got %q", stdout)
	}
}

// TestIntegration_ResourcesUnauthorized — T16 P1-2 fix.
// Anonymous caller (no auth) on a 401 prints a friendly hint AND exits with
// ExitAuthRequired (3) so an agent can branch on the code. The previous
// behaviour was exit 0, which silently masked a stale-token situation.
func TestIntegration_ResourcesUnauthorized(t *testing.T) {
	c := newITContext(t)
	c.mock.mu.Lock()
	c.mock.requireAuth = true
	c.mock.mu.Unlock()

	var gotErr error
	_, stderr := captureStdout(t, func() {
		_, _, err := run("resources")
		gotErr = err
	})
	if gotErr == nil {
		t.Fatal("resources 401 must now return a non-nil error (auth required)")
	}
	if code := ExitCodeFor(gotErr); code != ExitAuthRequired {
		t.Errorf("resources 401: expected exit code %d (ExitAuthRequired), got %d (err: %v)",
			ExitAuthRequired, code, gotErr)
	}
	if !strings.Contains(strings.ToLower(stderr+gotErr.Error()), "log") {
		t.Errorf("resources 401: expected a login-related hint, stderr=%q err=%q",
			stderr, gotErr)
	}
}

// ── status (local token store) ──────────────────────────────────────────────

func TestIntegration_StatusEmpty(t *testing.T) {
	newITContext(t)
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("status")
		if err != nil {
			t.Fatalf("status (empty) failed: %v", err)
		}
	})
	if !strings.Contains(stdout, "No resources") {
		t.Errorf("status empty: expected 'No resources', got %q", stdout)
	}
}

// TestIntegration_StatusShowsProvisioned asserts a provisioned resource is
// persisted locally and surfaced by `status`.
func TestIntegration_StatusShowsProvisioned(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	_, _ = c.provisionViaCLI("db", "tracked-db")

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("status")
		if err != nil {
			t.Fatalf("status failed: %v", err)
		}
	})
	if !strings.Contains(stdout, "tracked-db") {
		t.Errorf("status: expected provisioned resource 'tracked-db', got %q", stdout)
	}
	if !strings.Contains(stdout, "TOKEN") {
		t.Errorf("status: expected table header, got %q", stdout)
	}
}

// ── auth surface: whoami / login / logout / upgrade ─────────────────────────

func TestIntegration_WhoamiAnonymous(t *testing.T) {
	newITContext(t)
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami")
		if err != nil {
			t.Fatalf("whoami failed: %v", err)
		}
	})
	if !strings.Contains(strings.ToLower(stdout), "not logged in") {
		t.Errorf("whoami anonymous: expected 'Not logged in', got %q", stdout)
	}
}

func TestIntegration_LogoutWhenAnonymous(t *testing.T) {
	newITContext(t)
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("logout")
		if err != nil {
			t.Fatalf("logout failed: %v", err)
		}
	})
	if !strings.Contains(strings.ToLower(stdout), "not logged in") {
		t.Errorf("logout anonymous: expected 'Not logged in', got %q", stdout)
	}
}

// TestIntegration_LoginAndWhoami exercises the full login session flow against
// the mock (POST /auth/cli -> poll GET /auth/cli/:id -> save config) and then
// asserts whoami reflects the saved identity. The mock completes auth
// immediately so the poll succeeds on the first iteration.
func TestIntegration_LoginAndWhoami(t *testing.T) {
	c := newITContext(t)
	c.mock.mu.Lock()
	c.mock.authComplete = true
	c.mock.mu.Unlock()

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("login")
		if err != nil {
			t.Fatalf("login failed: %v", err)
		}
	})
	if !strings.Contains(strings.ToLower(stdout), "logged in") {
		t.Errorf("login: expected 'Logged in' confirmation, got %q", stdout)
	}

	// whoami must now show the authenticated identity.
	stdout2, _ := captureStdout(t, func() {
		_, _, err := run("whoami")
		if err != nil {
			t.Fatalf("whoami after login failed: %v", err)
		}
	})
	if !strings.Contains(stdout2, "tester@instanode.dev") {
		t.Errorf("whoami after login: expected account email, got %q", stdout2)
	}

	// logout must clear the config.
	stdout3, _ := captureStdout(t, func() {
		_, _, err := run("logout")
		if err != nil {
			t.Fatalf("logout after login failed: %v", err)
		}
	})
	if !strings.Contains(strings.ToLower(stdout3), "logged out") {
		t.Errorf("logout: expected 'Logged out', got %q", stdout3)
	}
}

// TestIntegration_UnknownCommand asserts an unknown subcommand exits non-zero.
func TestIntegration_UnknownCommand(t *testing.T) {
	newITContext(t)
	_, _, err := run("frobnicate")
	if err == nil {
		t.Fatal("unknown command: expected error, got nil")
	}
}

// TestIntegration_HelpExitsZero asserts --help is a clean, zero-exit path for
// the root command and every group.
func TestIntegration_HelpExitsZero(t *testing.T) {
	newITContext(t)
	for _, args := range [][]string{
		{"--help"},
		{"db", "--help"},
		{"db", "new", "--help"},
		{"up", "--help"},
		{"resources", "--help"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, _, err := run(args...)
			if err != nil {
				t.Errorf("%v: --help should exit 0, got %v", args, err)
			}
		})
	}
}
