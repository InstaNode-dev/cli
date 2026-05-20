package cmd

// bughunt_p2_test.go — regression tests for the P2 findings from BugHunt
// 2026-05-20 (T16), shipped as Wave 3 of the BugBash. Each test pins one
// audit-confirmed fix; future regressions would re-introduce a documented
// bug. Tests are part of the hermetic suite (no network), runnable with
// `go test ./... -race`.
//
//   TestBugHunt_T16_P2_1_ErrorEnvelopeParsedNotDumped       — T16 P2-1
//   TestBugHunt_T16_P2_1_402_TierWallSurfacesAgentAction    — T16 P2-1
//   TestBugHunt_T16_P2_1_429_RateLimitedShowsRetry          — T16 P2-1
//   TestBugHunt_T16_P2_2_ProvisionTimeoutAtLeast60s         — T16 P2-2
//   TestBugHunt_T16_P2_3_UpDefaultsToDevelopmentNotProd     — T16 P2-3
//   TestBugHunt_T16_P2_3_UpExplicitProductionStillNeedsAuth — T16 P2-3
//   TestBugHunt_T16_P2_4_WebhookReuseEmitsExportLine        — T16 P2-4
//   TestBugHunt_T16_P2_5_CredentialsKeyedByTokenNotID       — T16 P2-5
//   TestBugHunt_T16_P3_ResourcesJSONOutput                   — T16 P3
//   TestBugHunt_T16_P3_StatusJSONOutput                      — T16 P3
//   TestBugHunt_T16_P3_WhoamiJSONOutputNeverLeaksToken       — T16 P3

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/instant-dev/cli/internal/cliconfig"
	"github.com/instant-dev/cli/internal/secretstore"
)

// ── T16 P2-1 — structured error envelope parsing.

// TestBugHunt_T16_P2_1_ErrorEnvelopeParsedNotDumped asserts a 402 quota wall
// surfaces the envelope's message + agent_action + upgrade_url cleanly,
// not as a raw JSON dump.
func TestBugHunt_T16_P2_1_402_TierWallSurfacesAgentAction(t *testing.T) {
	c := newITContext(t)
	// Mint a 402 quota_exceeded envelope with the canonical W7G shape.
	c.mock.injectErrorOnProvision(402,
		"quota_exceeded",
		"You've hit your hobby-tier postgres limit (1 / 1 resources).",
		"Upgrade to Pro to raise the limit, or delete an unused resource.",
		"https://instanode.dev/billing",
	)

	resetProvisionFlags()
	_, _, err := run("db", "new", "--name", "tiered-out")
	if err == nil {
		t.Fatal("402 must surface as a non-nil error")
	}
	msg := err.Error()
	// The structured fields must be present.
	for _, want := range []string{
		"402",
		"quota_exceeded",
		"hobby-tier postgres limit",
		"Upgrade to Pro",
		"instanode.dev/billing",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("402 error message must contain %q, got %q", want, msg)
		}
	}
	// And the raw envelope JSON must NOT appear in the message (was the
	// pre-fix behaviour).
	if strings.Contains(msg, `"error":`) || strings.Contains(msg, `"agent_action":`) {
		t.Errorf("error must not dump raw JSON envelope; got %q", msg)
	}
}

// TestBugHunt_T16_P2_1_429_RateLimitedShowsRetry asserts a 429 envelope's
// retry_after_seconds is surfaced as a human-readable hint.
func TestBugHunt_T16_P2_1_429_RateLimitedShowsRetry(t *testing.T) {
	c := newITContext(t)
	c.mock.injectErrorOnProvisionWithRetry(429,
		"rate_limited",
		"Too many requests from this IP/24+ASN combination.",
		"Wait 30 seconds then retry.",
		30,
	)

	resetProvisionFlags()
	_, _, err := run("db", "new", "--name", "ratelimited")
	if err == nil {
		t.Fatal("429 must surface as a non-nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "429") {
		t.Errorf("429 message must include the status code, got %q", msg)
	}
	if !strings.Contains(msg, "rate limited") {
		t.Errorf("429 message must include 'rate limited', got %q", msg)
	}
	if !strings.Contains(msg, "30") {
		t.Errorf("429 message must include retry seconds 30, got %q", msg)
	}
}

// TestBugHunt_T16_P2_1_5xxFallsBackToRawBodyOnNonJSON asserts a non-JSON
// 5xx body still produces *some* truncated message rather than crashing.
func TestBugHunt_T16_P2_1_5xxFallsBackToRawBodyOnNonJSON(t *testing.T) {
	c := newITContext(t)
	c.mock.injectRawErrorOnProvision(500, "<html>upstream borked</html>")

	resetProvisionFlags()
	_, _, err := run("db", "new", "--name", "five-hundred")
	if err == nil {
		t.Fatal("5xx with non-JSON body must still error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Errorf("5xx message must mention status, got %q", msg)
	}
	// Truncation guard — the raw body is at most ~200 chars so the full
	// `<html>...</html>` should be present, but a 10MB stack trace would
	// not be. The current input is short and present in full.
	if !strings.Contains(msg, "upstream borked") {
		t.Errorf("5xx message must include truncated raw body, got %q", msg)
	}
}

// ── T16 P2-2 — provisioning HTTP client timeout raised to ≥60s.

// TestBugHunt_T16_P2_2_ProvisionTimeoutAtLeast60s pins the package-global
// HTTPClient timeout so a regression that drops it back to 10s is caught.
// Provisioning is synchronous on the api and can legitimately exceed 10s.
func TestBugHunt_T16_P2_2_ProvisionTimeoutAtLeast60s(t *testing.T) {
	newITContext(t)
	// initConfig() set HTTPClient.Timeout in root.go. Re-init to pick up
	// the documented production default (we may be running after a test
	// that overrode it).
	prevClient := HTTPClient
	t.Cleanup(func() { HTTPClient = prevClient })
	initConfig()

	want := 60 * time.Second
	if HTTPClient.Timeout < want {
		t.Errorf("HTTPClient.Timeout = %v, want >= %v (T16 P2-2: 10s caused orphan resources under load)",
			HTTPClient.Timeout, want)
	}
}

// ── T16 P2-3 — `up` env default is "development", not "production".

// TestBugHunt_T16_P2_3_UpDefaultsToDevelopmentNotProd asserts an `up` run with
// no `env:` in the manifest and no --env flag lands resources in development
// (matches CLAUDE.md rule 11 / migration 026), NOT in production.
func TestBugHunt_T16_P2_3_UpDefaultsToDevelopmentNotProd(t *testing.T) {
	c := newITContext(t)
	manifest := writeManifest(t, `
resources:
  - type: postgres
    name: env-default-up
    export: DB_URL
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
	captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest)
		if err != nil {
			t.Fatalf("up (anonymous, default env) must succeed: %v", err)
		}
	})
	if c.mock.count() != 1 {
		t.Fatalf("up: expected 1 resource provisioned, got %d", c.mock.count())
	}
	c.mock.mu.Lock()
	var gotEnv string
	for _, r := range c.mock.resources {
		gotEnv = r.Env
	}
	c.mock.mu.Unlock()
	if gotEnv != "development" {
		t.Errorf("up env default: want %q, got %q (T16 P2-3 contract)", "development", gotEnv)
	}
}

// TestBugHunt_T16_P2_3_UpExplicitProductionStillNeedsAuth asserts that an
// explicit --env=production WITHOUT auth fails at the local pre-check with
// ExitAuthRequired. (Pre-fix, anonymous + production was allowed — the
// auth gate now applies to any non-default env.)
func TestBugHunt_T16_P2_3_UpExplicitProductionStillNeedsAuth(t *testing.T) {
	newITContext(t)
	manifest := writeManifest(t, `
resources:
  - type: postgres
    name: prod-needs-auth
`)
	resetUpFlags()
	_, _, err := run("up", "--file", manifest, "--env", "production")
	if err == nil {
		t.Fatal("up --env=production anonymous: expected error, got nil")
	}
	if code := ExitCodeFor(err); code != ExitAuthRequired {
		t.Errorf("--env=production no-auth: expected ExitAuthRequired (%d), got %d (err: %v)",
			ExitAuthRequired, code, err)
	}
}

// ── T16 P2-4 — webhook REUSE preserves the --emit-env export line.

// TestBugHunt_T16_P2_4_WebhookReuseEmitsExportLine asserts that a SECOND
// `up --emit-env` run against a manifest with a webhook resource still
// emits `export NAME=...` for the webhook. Pre-fix, REUSE dropped the line
// because /credentials returns 404 for webhooks and the code path warned
// instead of emitting.
func TestBugHunt_T16_P2_4_WebhookReuseEmitsExportLine(t *testing.T) {
	c := newITContext(t)
	manifest := writeManifest(t, `
resources:
  - type: webhook
    name: idempotent-hook
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

	// First run — PROVISION.
	resetUpFlags()
	stdout1, _ := captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest, "--emit-env")
		if err != nil {
			t.Fatalf("up run 1 failed: %v", err)
		}
	})
	if !strings.Contains(stdout1, "export HOOK_URL=") {
		t.Fatalf("run 1: expected export HOOK_URL line, stdout=%q", stdout1)
	}

	// Second run — REUSE. The export line MUST still appear (T16 P2-4).
	resetUpFlags()
	stdout2, _ := captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest, "--emit-env")
		if err != nil {
			t.Fatalf("up run 2 failed: %v", err)
		}
	})
	if !strings.Contains(stdout2, "export HOOK_URL=") {
		t.Errorf("REUSE webhook MUST emit export line (T16 P2-4); run-2 stdout=%q", stdout2)
	}
	// And the URL value should still look like a webhook receive URL.
	if !strings.Contains(stdout2, "/webhook/receive/") {
		t.Errorf("REUSE webhook export URL must be a receive URL; got %q", stdout2)
	}
}

// ── T16 P2-5 — fetchCredentials URL path is the resource TOKEN, not the id.

// TestBugHunt_T16_P2_5_CredentialsKeyedByTokenNotID drives the mock with an
// `id != token` scenario and asserts the CLI fetches credentials at
// /api/v1/resources/<TOKEN>/credentials, NOT /<id>/credentials.
//
// Pre-fix, the API expects the `:id` path parameter to be the resource's
// token (UUID); the mock obscured this by keying credentials by token AND
// the list response returned the same token in both fields. This test
// forces the mock to require a token-keyed lookup and a wholly different
// id, and asserts the CLI's REUSE path still works.
func TestBugHunt_T16_P2_5_CredentialsKeyedByTokenNotID(t *testing.T) {
	c := newITContext(t)
	c.mock.idDifferentFromToken = true
	manifest := writeManifest(t, `
resources:
  - type: postgres
    name: token-not-id
    export: DB_URL
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

	// First run provisions.
	resetUpFlags()
	captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest, "--emit-env")
		if err != nil {
			t.Fatalf("up run 1 failed: %v", err)
		}
	})
	// Second run MUST REUSE — which means it fetched credentials by token.
	// If the CLI sent `id` instead of `token` the mock returns 404 and the
	// run prints "credentials hidden" + omits the export line.
	resetUpFlags()
	stdout2, _ := captureStdout(t, func() {
		_, _, err := run("up", "--file", manifest, "--emit-env")
		if err != nil {
			t.Fatalf("up run 2 failed: %v", err)
		}
	})
	if !strings.Contains(stdout2, "export DB_URL=") {
		t.Errorf("REUSE: credentials lookup must use TOKEN (not id); export line missing in: %q", stdout2)
	}
}

// ── T16 P3 — --json output mode for resources, status, whoami.

// TestBugHunt_T16_P3_ResourcesJSONOutput asserts `instant resources --json`
// emits a parseable JSON array, with stable field names matching the
// documented schema.
func TestBugHunt_T16_P3_ResourcesJSONOutput(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	_, _ = c.provisionViaCLI("db", "json-list-db")

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resources", "--json")
		if err != nil {
			t.Fatalf("resources --json failed: %v", err)
		}
	})
	var items []map[string]any
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		t.Fatalf("resources --json must emit valid JSON; got %q (err: %v)", stdout, err)
	}
	if len(items) == 0 {
		t.Fatal("resources --json: expected at least one item")
	}
	for _, f := range []string{"token", "resource_type", "name", "tier", "status"} {
		if _, ok := items[0][f]; !ok {
			t.Errorf("resources --json: missing field %q in %v", f, items[0])
		}
	}
}

// TestBugHunt_T16_P3_StatusJSONOutput asserts `instant status --json` emits
// a parseable JSON array of local token entries.
func TestBugHunt_T16_P3_StatusJSONOutput(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	_, _ = c.provisionViaCLI("db", "json-status-db")

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("status", "--json")
		if err != nil {
			t.Fatalf("status --json failed: %v", err)
		}
	})
	var entries []map[string]any
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		t.Fatalf("status --json must emit valid JSON; got %q (err: %v)", stdout, err)
	}
	if len(entries) == 0 {
		t.Fatal("status --json: expected at least one entry")
	}
}

// TestBugHunt_T16_P3_WhoamiJSONOutputNeverLeaksToken asserts that even in
// --json mode the bearer token is NEVER serialized (T16 P1-1 must still
// hold for the machine-readable surface).
func TestBugHunt_T16_P3_WhoamiJSONOutputNeverLeaksToken(t *testing.T) {
	c := newITContext(t)

	secret := "inst_live_NEVER_SHOW_THIS_IN_JSON_xyz"
	cfg := &cliconfig.Config{}
	cfg.APIKey = secret
	cfg.Email = "json-leak-test@example.com"
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
		_, _, err := run("whoami", "--json")
		if err != nil {
			t.Fatalf("whoami --json failed: %v", err)
		}
	})
	if strings.Contains(stdout, secret) {
		t.Fatalf("whoami --json leaked full bearer token; stdout=%q", stdout)
	}
	// The output must still be parseable JSON.
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("whoami --json must emit valid JSON; got %q (err: %v)", stdout, err)
	}
	if out["authenticated"] != true {
		t.Errorf("whoami --json: authenticated must be true; got %v", out["authenticated"])
	}
	if got, _ := out["email"].(string); got != "json-leak-test@example.com" {
		t.Errorf("whoami --json email field: got %q", got)
	}
	// `key_display` must exist but only carry the truncated form.
	keyDisp, _ := out["key_display"].(string)
	if keyDisp == "" {
		t.Error("whoami --json: key_display must be present (truncated form)")
	}
	if strings.Contains(keyDisp, "NEVER_SHOW") {
		t.Errorf("whoami --json key_display leaked secret middle: %q", keyDisp)
	}
}

// silence unused-import warning when individual tests are run.
var _ = http.StatusOK
