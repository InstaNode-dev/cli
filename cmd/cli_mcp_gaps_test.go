package cmd

// cli_mcp_gaps_test.go — regression tests for the BugBash QA round-2
// CLI-MCP gaps closed in the `fix/cli-hygiene-env-passthrough` PR.
//
//   CLI-MCP-8   — `--env` flag is parsed, forwarded in the request body,
//                 and the resolved env is surfaced in the human output.
//   CLI-MCP-9   — `instant deploy` parent Short text is explicitly labeled
//                 as a stub so the root help row carries the pointer.
//   CLI-MCP-11  — `instant resource <token>` and `instant resource delete
//                 <token>` exit 3 (ExitAuthRequired) when the caller is
//                 unauthenticated, BEFORE any side effects, matching the
//                 contract `instant resources` (list) already honors.
//
// Each test pins the fix — reverting it would re-introduce a documented
// QA-found gap.

import (
	"strings"
	"testing"
)

// ── CLI-MCP-8: --env flag plumbing ───────────────────────────────────────────

// TestCLI_MCP_8_EnvFlagForwarded asserts that `instant db new --name X --env Y`
// includes "env":"Y" in the request body — the mock echoes the resolved env
// back so we can assert on the wire shape via the mock's recorded resource.
//
// Why this matters: until CLI-MCP-8 the CLI dropped --env entirely, forcing
// agents that needed a non-default environment to fall back to curl
// (CLAUDE.md rule 11 — empty `env` lands in "development").
func TestCLI_MCP_8_EnvFlagForwarded(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("db", "new", "--name", "env-fwd-db", "--env", "production")
		if err != nil {
			t.Fatalf("db new --env: %v", err)
		}
	})
	// Resolved env line surfaces in the human output (CLI-MCP-8 acceptance).
	if !strings.Contains(stdout, "env   production") {
		t.Errorf("expected `env   production` line in output, got %q", stdout)
	}

	// The mock parses and echoes body.Env back as the resource's env. List
	// it to confirm the value reached the server.
	tok := lastSavedToken(t)
	if tok == "" {
		t.Fatalf("no token persisted after provision")
	}
	t.Cleanup(func() { c.deleteResource(tok) })
	c.mock.mu.Lock()
	defer c.mock.mu.Unlock()
	res, ok := c.mock.resources[tok]
	if !ok {
		t.Fatalf("mock has no record of token %s", tok)
	}
	if res.Env != "production" {
		t.Errorf("server received env=%q, want %q", res.Env, "production")
	}
}

// TestCLI_MCP_8_EnvFlagOmittedKeepsServerDefault asserts that omitting --env
// does NOT inject an env field — the server sees an absent key and applies
// its documented default (development). The mock mirrors this: empty body.Env
// → resolves to "development".
func TestCLI_MCP_8_EnvFlagOmittedKeepsServerDefault(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("cache", "new", "--name", "env-default-cache")
		if err != nil {
			t.Fatalf("cache new (no --env): %v", err)
		}
	})
	if !strings.Contains(stdout, "env   development") {
		t.Errorf("expected resolved env=development line, got %q", stdout)
	}
	tok := lastSavedToken(t)
	if tok == "" {
		t.Fatalf("no token persisted")
	}
	t.Cleanup(func() { c.deleteResource(tok) })
}

// TestCLI_MCP_8_EnvFallbackOnLegacyAPI asserts the CLI surfaces
// `env   development` (not the empty string) when the server response omits
// the `env` field entirely — the documented behavior against an API build
// that predates migration 026.
func TestCLI_MCP_8_EnvFallbackOnLegacyAPI(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	c.mock.mu.Lock()
	c.mock.omitEnvInProvision = true
	c.mock.mu.Unlock()

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("db", "new", "--name", "env-legacy-db")
		if err != nil {
			t.Fatalf("db new (legacy API): %v", err)
		}
	})
	if !strings.Contains(stdout, "env   development") {
		t.Errorf("expected legacy fallback `env   development`, got %q", stdout)
	}
	tok := lastSavedToken(t)
	t.Cleanup(func() { c.deleteResource(tok) })
}

// TestCLI_MCP_8_EnvOverrideReasonSurfaced asserts that when the server
// returns env_override_reason (e.g. anon caller asked for production and
// got demoted), the CLI prints that reason on its own line.
func TestCLI_MCP_8_EnvOverrideReasonSurfaced(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	c.mock.mu.Lock()
	c.mock.envOverrideReason = "anonymous tier cannot target production; downgraded to development"
	c.mock.mu.Unlock()

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("db", "new", "--name", "env-override-db", "--env", "production")
		if err != nil {
			t.Fatalf("db new --env production: %v", err)
		}
	})
	if !strings.Contains(stdout, "env_override_reason") {
		t.Errorf("expected env_override_reason line in output, got %q", stdout)
	}
	if !strings.Contains(stdout, "anonymous tier cannot target production") {
		t.Errorf("expected reason text in output, got %q", stdout)
	}
	tok := lastSavedToken(t)
	t.Cleanup(func() { c.deleteResource(tok) })
}

// TestCLI_MCP_8_EnvFlagAllProvisioningVerbs asserts every provisioning verb
// (db, cache, nosql, queue, storage, webhook, vector) accepts --env. A typo
// in init() that forgot to bind --env on one group would be caught here.
func TestCLI_MCP_8_EnvFlagAllProvisioningVerbs(t *testing.T) {
	c := newITContext(t)
	for _, tc := range []struct{ group, name string }{
		{"db", "env-all-db"},
		{"cache", "env-all-cache"},
		{"nosql", "env-all-nosql"},
		{"queue", "env-all-queue"},
		{"storage", "env-all-storage"},
		{"webhook", "env-all-webhook"},
		{"vector", "env-all-vector"},
	} {
		t.Run(tc.group, func(t *testing.T) {
			resetProvisionFlags()
			_, _ = captureStdout(t, func() {
				_, _, err := run(tc.group, "new", "--name", tc.name, "--env", "staging")
				if err != nil {
					t.Fatalf("%s new --env: %v", tc.group, err)
				}
			})
			tok := lastSavedToken(t)
			if tok == "" {
				t.Fatalf("%s: no token persisted", tc.group)
			}
			c.mock.mu.Lock()
			res, ok := c.mock.resources[tok]
			c.mock.mu.Unlock()
			if !ok {
				t.Fatalf("%s: mock missing token", tc.group)
			}
			if res.Env != "staging" {
				t.Errorf("%s: server saw env=%q, want %q", tc.group, res.Env, "staging")
			}
			t.Cleanup(func() { c.deleteResource(tok) })
		})
	}
}

// ── CLI-MCP-9: deploy parent help text labels as stub ────────────────────────

// TestCLI_MCP_9_DeployShortLabelsStub asserts the cobra Short for the
// `instant deploy` parent contains the literal "[stub" marker AND points at
// the canonical alternative path (MCP create_deploy / POST /deploy/new).
// The Short string is what surfaces in `instant --help` one-liners — an
// agent's first contact with this command MUST carry the pointer.
func TestCLI_MCP_9_DeployShortLabelsStub(t *testing.T) {
	if deployCmd.Short == "" {
		t.Fatal("deploy command has no Short text")
	}
	if !strings.Contains(deployCmd.Short, "[stub") {
		t.Errorf("deploy.Short missing `[stub` marker: %q", deployCmd.Short)
	}
	if !strings.Contains(deployCmd.Short, "create_deploy") &&
		!strings.Contains(deployCmd.Short, "/deploy/new") {
		t.Errorf("deploy.Short must point at MCP `create_deploy` or `POST /deploy/new`: %q",
			deployCmd.Short)
	}
}

// ── CLI-MCP-11: resource detail/delete unauth → exit 3 ──────────────────────

// TestCLI_MCP_11_ResourceDetail_Unauth_ExitsAuthRequired asserts that
// calling `instant resource <token>` without auth exits with
// ExitAuthRequired (3), BEFORE any API call. Reverting the haveAuth()
// short-circuit would let an anonymous caller reach the API and either
// succeed (token-as-bearer pattern) or 404 (exit 1) — neither matches the
// documented contract that read commands require auth.
func TestCLI_MCP_11_ResourceDetail_Unauth_ExitsAuthRequired(t *testing.T) {
	newITContext(t) // anonymous: no authSetupForTest call

	_, _, err := run("resource", "some-token")
	if err == nil {
		t.Fatal("resource <token> (unauth) must error, got nil")
	}
	if code := ExitCodeFor(err); code != ExitAuthRequired {
		t.Errorf("resource <token> (unauth) exit code = %d, want %d (ExitAuthRequired)",
			code, ExitAuthRequired)
	}
	if !strings.Contains(err.Error(), "authentication required") {
		t.Errorf("expected `authentication required` message, got %q", err.Error())
	}
}

// TestCLI_MCP_11_ResourceDelete_Unauth_ExitsAuthRequired asserts the
// destructive path also exits 3 on unauth, BEFORE the --yes confirmation
// prompt. An agent that ran `instant resource delete X --yes` without auth
// previously had no deterministic exit code to branch on.
func TestCLI_MCP_11_ResourceDelete_Unauth_ExitsAuthRequired(t *testing.T) {
	newITContext(t) // anonymous

	_, _, err := run("resource", "delete", "some-token", "--yes")
	if err == nil {
		t.Fatal("resource delete (unauth) must error, got nil")
	}
	if code := ExitCodeFor(err); code != ExitAuthRequired {
		t.Errorf("resource delete (unauth) exit code = %d, want %d (ExitAuthRequired)",
			code, ExitAuthRequired)
	}
}
