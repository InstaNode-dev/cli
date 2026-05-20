package cmd

// bughunt_b15_test.go — regression tests for the four P0 findings from
// BugBash B15 (2026-05-20). Each test pins one fix; reverting any fix
// would re-introduce a confirmed bug.
//
//   TestWhoami_RespectsInstantToken      — B15-P0 (1) whoami INSTANT_TOKEN
//   TestRoot_VersionStamped              — B15-P0 (2) --version stamping
//   TestUnknownSubcommand_ExitNonZero    — B15-P0 (3) sub-sub-command errs
//   TestJSONMode_ErrorEnvelope           — B15-P0 (4) --json error wrapper
//
// Plus smoke coverage for the new commands the scope-gap section added:
//
//   TestExtras_StorageWebhookVector      — provisioning all 3
//   TestExtras_ResourceDetail             — `instant resource <token>`
//   TestExtras_ResourceDeleteRequiresYes  — confirmation gate
//   TestExtras_ResourceDeleteWithYes      — happy-path delete

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/instant-dev/cli/internal/cliconfig"
	"github.com/instant-dev/cli/internal/secretstore"
	"github.com/instant-dev/cli/internal/tokens"
)

// ── B15-P0 (1): whoami respects INSTANT_TOKEN ───────────────────────────────

// TestWhoami_RespectsInstantToken pins the fix that `instant whoami` reads
// INSTANT_TOKEN from the environment when no `instant login` config is on
// disk. Before the fix the env-token user appeared anonymous in `whoami`'s
// output and `whoami --json` reported authenticated=false.
func TestWhoami_RespectsInstantToken(t *testing.T) {
	c := newITContext(t)

	// Drop any saved config so we test the pure env-token code path.
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()

	const envSecret = "inst_live_env_token_b15"
	t.Setenv("INSTANT_TOKEN", envSecret)

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami", "--json")
		if err != nil {
			t.Fatalf("whoami --json: %v", err)
		}
	})

	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("whoami --json must emit JSON; got %q (err: %v)", stdout, err)
	}
	if auth, _ := out["authenticated"].(bool); !auth {
		t.Errorf("INSTANT_TOKEN must mark whoami as authenticated, got: %v", out)
	}
	if api, _ := out["api_url"].(string); api == "" {
		t.Errorf("whoami --json: api_url must resolve to a non-empty string, got %q", api)
	}
	// The bearer token MUST NOT appear anywhere in JSON output (T16 P1-1
	// regression still holds for the env-token path).
	for cut := 9; cut <= len(envSecret); cut++ {
		if strings.Contains(stdout, envSecret[:cut]) {
			t.Fatalf("INSTANT_TOKEN leaked >=%d chars of the secret: %q", cut, stdout)
		}
	}
	_ = c
}

// TestWhoami_RespectsInstantToken_TrimsWhitespace pins the B15-P1 fix —
// INSTANT_TOKEN values that contain a trailing newline (typical for
// `INSTANT_TOKEN=$(cat .pat)`) get TrimSpace'd. The previous behaviour
// emitted the raw newline into the Authorization header, which the server
// rejected with 401.
func TestWhoami_RespectsInstantToken_TrimsWhitespace(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()

	// Padded with newline + spaces, mimicking $(cat .pat).
	t.Setenv("INSTANT_TOKEN", "  inst_live_padded_b15  \n")
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami", "--json")
		if err != nil {
			t.Fatalf("whoami --json: %v", err)
		}
	})
	var out map[string]any
	_ = json.Unmarshal([]byte(stdout), &out)
	if auth, _ := out["authenticated"].(bool); !auth {
		t.Errorf("INSTANT_TOKEN with whitespace must still authenticate: %v", out)
	}
	// The display form should be the trimmed token's prefix, not start with
	// whitespace.
	if disp, _ := out["key_display"].(string); strings.HasPrefix(disp, " ") {
		t.Errorf("key_display must reflect the TRIMMED token, got %q", disp)
	}
	_ = c
}

// ── B15-P0 (2): --version is stamped from ldflags ───────────────────────────

// TestRoot_VersionStamped asserts the cobra root carries a Version string
// and that SetBuildInfo() correctly composes it from ldflag values. The
// satisfies CLAUDE.md rule 14 (build-SHA gate) — a deploy operator can
// `instant --version | grep <sha>` to verify the live binary matches HEAD.
func TestRoot_VersionStamped(t *testing.T) {
	// Snapshot + restore so the test is reentrant.
	prev := rootCmd.Version
	t.Cleanup(func() { rootCmd.Version = prev })

	// Stamping with real values produces the canonical format.
	SetBuildInfo("1.2.3", "abc1234", "2026-05-20T00:00:00Z")
	if got := rootCmd.Version; got != "1.2.3 (abc1234, 2026-05-20T00:00:00Z)" {
		t.Errorf("rootCmd.Version: want %q, got %q",
			"1.2.3 (abc1234, 2026-05-20T00:00:00Z)", got)
	}

	// Empty values must fall back to the sentinel defaults so an unflagged
	// `go build` still produces a runnable binary (CLAUDE.md `go test`
	// + `go run` ergonomics).
	SetBuildInfo("", "", "")
	if got := rootCmd.Version; got != "dev (unknown, unknown)" {
		t.Errorf("SetBuildInfo(empty): want sentinel %q, got %q",
			"dev (unknown, unknown)", got)
	}

	// And `instant --version` actually emits the stamped string.
	// Cobra prints --version to the command's OutBuf, which `run` captures
	// into the returned stdout string.
	SetBuildInfo("9.9.9", "deadbee", "2026-05-20T00:00:01Z")
	stdout, _, err := run("--version")
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if !strings.Contains(stdout, "9.9.9") || !strings.Contains(stdout, "deadbee") {
		t.Errorf("--version must print stamped values; got %q", stdout)
	}
}

// ── B15-P0 (3): unknown sub-sub-commands exit non-zero ──────────────────────

// TestUnknownSubcommand_ExitNonZero pins the fix that `instant <group>
// <unknown-verb> ...` returns a non-zero exit code with a clear
// "unknown command" message — instead of silently printing help and
// exiting 0. Covers every resource-group parent the CLI exposes.
func TestUnknownSubcommand_ExitNonZero(t *testing.T) {
	newITContext(t)
	groups := []string{"db", "cache", "nosql", "queue", "storage", "webhook", "vector"}
	for _, g := range groups {
		t.Run(g, func(t *testing.T) {
			_, _, err := run(g, "delete", "some-token")
			if err == nil {
				t.Fatalf("`instant %s delete some-token` MUST return non-nil err; got nil", g)
			}
			lower := strings.ToLower(err.Error())
			if !strings.Contains(lower, "unknown command") &&
				!strings.Contains(lower, "delete") {
				t.Errorf("`instant %s delete`: error should mention unknown command, got %q", g, err)
			}
		})
	}
}

// TestUnknownSubcommand_BareGroupPrintsHelp asserts the no-args path still
// exits 0 and prints help (the legacy behaviour for `instant db`). Only
// the "args were provided but didn't match a subcommand" path errors.
func TestUnknownSubcommand_BareGroupPrintsHelp(t *testing.T) {
	newITContext(t)
	for _, g := range []string{"db", "cache", "nosql", "queue", "storage", "webhook", "vector"} {
		t.Run(g, func(t *testing.T) {
			_, _, err := run(g)
			if err != nil {
				t.Errorf("`instant %s` (no args) should exit 0 with help; got %v", g, err)
			}
		})
	}
}

// ── B15-P0 (4): --json mode wraps errors in a JSON envelope ─────────────────

// TestJSONMode_ErrorEnvelope pins the fix that ANY error emitted by a
// command invoked with --json is rendered as a JSON envelope on stdout,
// not as cobra's "Error: …\nUsage:\n…" block. Agents piping `--json` into
// `jq` were crashing on every 4xx/5xx before this fix.
func TestJSONMode_ErrorEnvelope(t *testing.T) {
	c := newITContext(t)

	// Drive a 402 quota_exceeded envelope through `instant resources --json`
	// by arming the mock to require auth — anonymous resources --json hits
	// the 401 path which is also a structured-error code path.
	c.mock.mu.Lock()
	c.mock.requireAuth = true
	c.mock.mu.Unlock()

	resetJSONFlags()
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resources", "--json")
		if err == nil {
			t.Fatal("resources --json with 401 should return non-nil err")
		}
	})

	// Stdout MUST be valid JSON; cobra's "Usage: …" block must NOT appear.
	if strings.Contains(stdout, "Usage:") {
		t.Errorf("--json error must not include cobra usage block, got: %q", stdout)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("--json error must emit valid JSON; got %q (err: %v)", stdout, err)
	}
	for _, k := range []string{"ok", "error", "message", "exit_code"} {
		if _, ok := env[k]; !ok {
			t.Errorf("--json envelope must include %q; got %v", k, env)
		}
	}
	if ok, _ := env["ok"].(bool); ok {
		t.Errorf("--json error envelope must have ok=false, got %v", env)
	}
	if code, _ := env["error"].(string); code == "" {
		t.Errorf("--json envelope: error code must be non-empty, got %v", env)
	}
}

// TestJSONMode_NetworkErrorWrapped pins the B15-P1 fix — a raw network
// error (DNS lookup failure / connection refused) emitted via --json is
// wrapped as a "network_error" envelope rather than a raw Go string.
func TestJSONMode_NetworkErrorWrapped(t *testing.T) {
	newITContext(t)

	// Point the CLI at a guaranteed-unreachable host so the HTTP client
	// fails with a connection-refused error.
	prev := APIBaseURL
	APIBaseURL = "http://127.0.0.1:1" // RFC-5737 says don't, but :1 is safer
	t.Cleanup(func() { APIBaseURL = prev })

	resetJSONFlags()
	stdout, _ := captureStdout(t, func() {
		_, _, _ = run("resources", "--json")
	})
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("network failure --json: stdout must start with JSON envelope, got %q", stdout)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("network failure --json must emit JSON: %v (out=%q)", err, stdout)
	}
	if code, _ := env["error"].(string); code != "network_error" && code != "cli_error" {
		// network_error is the preferred wrap; cli_error is acceptable
		// fallback for environments where the error doesn't classify
		// as a *net.OpError (e.g. some CI sandboxes).
		t.Errorf("network failure: want error=network_error|cli_error, got %v", code)
	}
}

// ── Smokes for the new commands (B15 scope gap) ─────────────────────────────

// TestExtras_StorageWebhookVector exercises the three new provisioning
// commands shipped this PR. Each call MUST land a resource on the mock
// (proves the endpoint is wired) and the local tokens store MUST record it.
func TestExtras_StorageWebhookVector(t *testing.T) {
	c := newITContext(t)
	cases := []struct {
		group string
		want  string
	}{
		{"storage", "storage"},
		{"webhook", "webhook"},
		{"vector", "vector"},
	}
	for _, tc := range cases {
		t.Run(tc.group, func(t *testing.T) {
			resetProvisionFlags()
			stdout, _ := captureStdout(t, func() {
				_, _, err := run(tc.group, "new", "--name", tc.group+"-smoke")
				if err != nil {
					t.Fatalf("%s new: %v", tc.group, err)
				}
			})
			if !strings.Contains(stdout, "ok") {
				t.Errorf("%s new: expected 'ok' line, got %q", tc.group, stdout)
			}
			tok := lastSavedToken(t)
			if tok == "" {
				t.Fatalf("%s new: no token persisted", tc.group)
			}
			t.Cleanup(func() { c.deleteResource(tok) })
		})
	}
}

// TestExtras_ResourceDetail asserts `instant resource <token>` GETs the
// detail endpoint and renders the connection URL + resource type.
func TestExtras_ResourceDetail(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	_, token := c.provisionViaCLI("db", "detail-db")

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", token)
		if err != nil {
			t.Fatalf("resource <token>: %v", err)
		}
	})
	if !strings.Contains(stdout, token) {
		t.Errorf("resource detail must echo token, got %q", stdout)
	}
	if !strings.Contains(stdout, "postgres") {
		t.Errorf("resource detail must surface resource_type=postgres, got %q", stdout)
	}
	if !strings.Contains(stdout, "URL") {
		t.Errorf("resource detail must surface connection URL line, got %q", stdout)
	}
}

// TestExtras_ResourceDeleteRequiresYes asserts the destructive command
// REFUSES to delete without --yes when stdin is not a TTY. Agents that
// pipe input would otherwise risk accidental deletes.
func TestExtras_ResourceDeleteRequiresYes(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	_, token := c.provisionViaCLI("db", "guarded-db")

	// Pipe stdin to /dev/null so the "not a TTY" branch fires.
	origStdin := os.Stdin
	r, _, _ := os.Pipe()
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = origStdin
		_ = r.Close()
	})

	_, _, err := run("resource", "delete", token)
	if err == nil {
		t.Fatalf("delete without --yes (non-TTY) must error; resource %s would be lost", token)
	}
	// The mock must still have the resource.
	if c.mock.count() == 0 {
		t.Errorf("delete without --yes must NOT actually delete; mock is now empty")
	}
}

// TestExtras_ResourceDeleteWithYes happy-path: --yes skips the prompt and
// the resource is actually removed from the server + local store.
func TestExtras_ResourceDeleteWithYes(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	_, token := c.provisionViaCLI("db", "doomed-db")

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "delete", token, "--yes")
		if err != nil {
			t.Fatalf("delete --yes: %v", err)
		}
	})
	if !strings.Contains(stdout, "deleted") {
		t.Errorf("delete --yes: expected 'deleted' confirmation, got %q", stdout)
	}
	if c.mock.count() != 0 {
		t.Errorf("delete --yes: server must show 0 resources, got %d", c.mock.count())
	}
	// Local token store must be cleaned too — `instant status` should not
	// show the deleted entry.
	if store, err := tokens.Load(); err == nil {
		if store.Find(token) != nil {
			t.Errorf("delete --yes: local tokens store still has the entry for %s", token)
		}
	}
}

// TestExtras_ResourceDelete_JSON asserts --json mode on the destructive
// path also emits a JSON envelope on the success line (machine-readable
// confirmation), and that --json errors get wrapped through wrapJSONErr.
func TestExtras_ResourceDelete_JSON(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	_, token := c.provisionViaCLI("db", "doomed-json-db")

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "delete", token, "--yes", "--json")
		if err != nil {
			t.Fatalf("delete --yes --json: %v", err)
		}
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("delete --json must emit JSON, got %q (err: %v)", stdout, err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Errorf("delete --yes --json: want ok=true, got %v", out)
	}
	if del, _ := out["deleted"].(string); del != token {
		t.Errorf("delete --yes --json: want deleted=%s, got %v", token, out)
	}
	if c.mock.count() != 0 {
		t.Errorf("delete --yes --json: server must be empty, got %d", c.mock.count())
	}
}

// http.StatusXXX import is needed elsewhere; this stub keeps the linter
// quiet if the rest of the file later drops its sole http reference.
var _ = http.StatusOK
