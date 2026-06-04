package cmd

// contract_gaps_test.go — fills the contract-coverage gaps the done-bar
// (donebar_command_coverage_test.go) maps to:
//
//   TestContract_StorageVectorProvisionEndToEnd
//     The integration suite drives `db/cache/nosql/queue new` end-to-end
//     (TestIntegration_ProvisionAllTypes) but NOT `storage new` / `vector new`
//     through the real cobra tree. This asserts both: correct endpoint hit
//     (/storage/new, /vector/new via the resource_type the mock records),
//     flag→payload mapping (--name forwarded), and the resolved-env default.
//
//   TestContract_LoginUsesCanonicalAuthURL
//     K1 / A12 (USER-FLOW matrix): `instant login` must open the auth_url the
//     server returns from POST /auth/cli — the CANONICAL host — verbatim, not
//     a CLI-synthesised one. This pins the device-flow contract: the CLI is a
//     dumb relay of the server-provided canonical URL.
//
// Both use the existing hermetic stateful mock (testapi_test.go) and the
// integration harness (integration_test.go) so they run with zero network and
// obey the mandatory resource-cleanup sweep.

import (
	"strings"
	"testing"
)

// TestContract_StorageVectorProvisionEndToEnd provisions storage and vector
// resources through the REAL command tree and asserts the mock recorded the
// correct resource_type for each — proving `instant storage new` hits
// /storage/new and `instant vector new` hits /vector/new (the mock keys
// resource_type off the endpoint path in endpointResourceType).
func TestContract_StorageVectorProvisionEndToEnd(t *testing.T) {
	c := newITContext(t)

	cases := []struct {
		group    string // CLI group: `instant <group> new`
		wantType string // resource_type the mock records (== endpoint mapping)
	}{
		{"storage", "storage"},
		{"vector", "vector"},
	}

	for _, tc := range cases {
		t.Run(tc.group, func(t *testing.T) {
			resetProvisionFlags()
			name := "contract-" + tc.group
			out, token := c.provisionViaCLI(tc.group, name)

			// flag→payload: the name we passed must round-trip into the stored
			// resource (the mock requires a non-empty name or it 400s).
			c.mock.mu.Lock()
			res := c.mock.resources[token]
			c.mock.mu.Unlock()
			if res == nil {
				t.Fatalf("%s: no resource recorded for token %q", tc.group, token)
			}

			// endpoint mapping: storage new -> /storage/new (resource_type
			// "storage"); vector new -> /vector/new (resource_type "vector").
			if res.ResourceType != tc.wantType {
				t.Errorf("%s: wrong endpoint — recorded resource_type=%q, want %q (CLI hit the wrong /%s/new path?)",
					tc.group, res.ResourceType, tc.wantType, tc.group)
			}
			if res.Name != name {
				t.Errorf("%s: --name not forwarded — recorded name=%q, want %q", tc.group, res.Name, name)
			}

			// resolved-env default (CLAUDE.md rule 11): no --env => development.
			if res.Env != "development" {
				t.Errorf("%s: resolved-env default want %q, got %q", tc.group, "development", res.Env)
			}

			// user-visible output carries the token + an ok line.
			if !strings.Contains(out, token) {
				t.Errorf("%s: provision output missing token %q: %q", tc.group, token, out)
			}
		})
	}
}

// browserOpenLine extracts the URL line that runLogin prints immediately
// after the "Opening browser to:" label. runLogin emits:
//
//	Opening browser to:
//	  <auth_url>
//
// so the URL is the next non-empty line. Returns "" if the label is absent.
func browserOpenLine(stdout string) string {
	lines := strings.Split(stdout, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "Opening browser to:") {
			for _, next := range lines[i+1:] {
				if strings.TrimSpace(next) != "" {
					return strings.TrimSpace(next)
				}
			}
		}
	}
	return ""
}

// TestContract_LoginUsesCanonicalAuthURL pins the device-flow contract: the
// URL `instant login` opens is the auth_url the server returns from
// POST /auth/cli (the canonical instanode.dev host), passed through verbatim.
//
// The mock's /auth/cli returns auth_url="https://instanode.dev/cli-auth?s=test".
// We capture login's stdout (it prints "Opening browser to:\n  <auth_url>")
// and assert the canonical URL is what surfaces — so a future refactor that
// synthesises the URL CLI-side instead of relaying the server's would fail.
func TestContract_LoginUsesCanonicalAuthURL(t *testing.T) {
	c := newITContext(t)
	// Complete auth immediately so poll succeeds on the first iteration and the
	// command returns without hitting the long production poll window.
	c.mock.mu.Lock()
	c.mock.authComplete = true
	c.mock.mu.Unlock()

	const canonical = "https://instanode.dev/cli-auth?s=test"

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("login")
		if err != nil {
			t.Fatalf("login failed: %v", err)
		}
	})

	if !strings.Contains(stdout, canonical) {
		t.Errorf("login must open the server-provided canonical auth_url %q (relayed from POST /auth/cli), got stdout=%q",
			canonical, stdout)
	}

	// Defense-in-depth, scoped to the browser-open line only: the URL the CLI
	// tells the user it is opening must be the canonical auth_url, NOT the API
	// base URL the CLI happens to be pointed at (the httptest server). Other
	// lines (e.g. the "Upgrade for higher limits: <api>/pricing" footer)
	// legitimately reference the API host, so we isolate the open line.
	openLine := browserOpenLine(stdout)
	if openLine == "" {
		t.Fatalf("login did not print a 'Opening browser to:' line; stdout=%q", stdout)
	}
	if !strings.Contains(openLine, canonical) {
		t.Errorf("browser-open line must carry the canonical auth_url %q, got %q", canonical, openLine)
	}
	if strings.Contains(openLine, c.srv) {
		t.Errorf("browser-open line leaked the API host %q — it must relay the server's canonical auth_url, not the API base URL. line=%q",
			c.srv, openLine)
	}
}
