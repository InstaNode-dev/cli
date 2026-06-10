package cmd

// agent_dx_fixes_test.go — coverage for the three agent-DX fixes shipped in
// fix/cli-agent-dx-whoami-json-headless:
//
//   B1  whoami validates the bearer token against GET /auth/me instead of
//       reflecting local config (a bogus token now reports NOT authenticated).
//   B2  every provisioning `new` verb honors --json (machine-readable token +
//       connection_url + environment).
//   U2  login honors $BROWSER and adds --no-browser for headless/agent boxes.
//
// Tests follow the existing httptest-mock convention (newITContext): a stateful
// fake API the CLI talks to with zero network access.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InstaNode-dev/cli/internal/cliconfig"
	"github.com/InstaNode-dev/cli/internal/secretstore"
)

// ── B1: whoami real server validation ───────────────────────────────────────

// TestWhoami_BogusToken_ReportsNotAuthenticated is the headline B1 regression:
// an agent gating on `whoami --json | jq .authenticated` must NOT get a false
// positive for a token the server rejects. The mock returns 401 on /auth/me,
// so the bogus token reports authenticated:false with a non-zero exit.
func TestWhoami_BogusToken_ReportsNotAuthenticated(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()

	// Mock rejects every /auth/me with 401 for this test.
	c.mock.mu.Lock()
	c.mock.rejectAuthMe = true
	c.mock.mu.Unlock()

	t.Setenv("INSTANT_TOKEN", "inst_bogus_garbage_token")

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami", "--json")
		if err == nil {
			t.Fatal("bogus token must produce a non-nil error (non-zero exit)")
		}
		if got := ExitCodeFor(err); got != ExitAuthRequired {
			t.Errorf("bogus token exit code = %d, want %d", got, ExitAuthRequired)
		}
	})

	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("whoami --json must emit a single JSON object; got %q (err %v)", stdout, err)
	}
	if out["authenticated"] != false {
		t.Errorf("bogus token: authenticated must be false, got %v", out["authenticated"])
	}
	if note, _ := out["error"].(string); note == "" {
		t.Error("bogus token: error note must be populated so an agent can branch")
	}
	// Exactly ONE JSON object on stdout (no duplicate error envelope).
	if strings.Count(strings.TrimSpace(stdout), "\n}") != 1 {
		t.Errorf("whoami --json must emit exactly one envelope; got %q", stdout)
	}
}

// TestWhoami_ValidToken_PopulatesFromServer asserts a server-validated token
// reports authenticated:true with tier/email sourced from /auth/me (not local
// config), exit 0.
func TestWhoami_ValidToken_PopulatesFromServer(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()
	_ = c

	t.Setenv("INSTANT_TOKEN", "inst_valid_token")

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami", "--json")
		if err != nil {
			t.Fatalf("valid token must exit 0, got: %v", err)
		}
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("whoami --json invalid JSON: %q (%v)", stdout, err)
	}
	if out["authenticated"] != true {
		t.Errorf("valid token: authenticated must be true, got %v", out["authenticated"])
	}
	// Mock /auth/me returns tier=pro, email=tester@instanode.dev.
	if got, _ := out["tier"].(string); got != "pro" {
		t.Errorf("tier must come from /auth/me, got %q", got)
	}
	if got, _ := out["email"].(string); got != "tester@instanode.dev" {
		t.Errorf("email must come from /auth/me, got %q", got)
	}
}

// TestWhoami_ValidToken_HumanOutput covers the non-JSON validated branch.
func TestWhoami_ValidToken_HumanOutput(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()
	_ = c

	t.Setenv("INSTANT_TOKEN", "inst_valid_token_human")
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami")
		if err != nil {
			t.Fatalf("valid token (human) must exit 0, got %v", err)
		}
	})
	for _, want := range []string{"Email:", "Plan:", "Team:", "pro", "API URL:", "Key:", "Stored:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human whoami missing %q; got:\n%s", want, stdout)
		}
	}
}

// TestWhoami_BogusToken_HumanOutput covers the non-JSON server-rejected branch.
func TestWhoami_BogusToken_HumanOutput(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()

	c.mock.mu.Lock()
	c.mock.rejectAuthMe = true
	c.mock.mu.Unlock()

	t.Setenv("INSTANT_TOKEN", "inst_bogus_human")
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami")
		if err == nil {
			t.Fatal("bogus token (human) must produce a non-nil error")
		}
	})
	if !strings.Contains(stdout, "Not authenticated") {
		t.Errorf("human reject path must say 'Not authenticated'; got:\n%s", stdout)
	}
}

// TestWhoami_Offline_DistinctFromLoggedOut asserts that a transport failure
// (server unreachable) reports authenticated:false WITH an error note and a
// generic (exit 1) code — distinct from a clean logout — so a flaky network
// never masquerades as "logged out".
func TestWhoami_Offline_DistinctFromLoggedOut(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()

	// Point the CLI at a dead server so the next request fails at the
	// transport layer (drives the offline path).
	_ = c
	pointAtDeadServer()

	t.Setenv("INSTANT_TOKEN", "inst_token_but_server_down")
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami", "--json")
		if err == nil {
			t.Fatal("offline check must produce a non-nil error")
		}
		if got := ExitCodeFor(err); got != ExitGeneric {
			t.Errorf("offline exit code = %d, want %d (generic, not auth)", got, ExitGeneric)
		}
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("offline whoami --json must still be valid JSON; got %q (%v)", stdout, err)
	}
	if out["authenticated"] != false {
		t.Errorf("offline: authenticated must be false, got %v", out["authenticated"])
	}
	note, _ := out["error"].(string)
	if !strings.Contains(note, "could not reach the server") {
		t.Errorf("offline error note must explain unreachable server, got %q", note)
	}
}

// TestWhoami_Offline_HumanOutput covers the non-JSON offline branch.
func TestWhoami_Offline_HumanOutput(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()
	_ = c
	pointAtDeadServer()

	t.Setenv("INSTANT_TOKEN", "inst_token_server_down_human")
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami")
		if err == nil {
			t.Fatal("offline (human) must produce a non-nil error")
		}
	})
	if !strings.Contains(stdout, "Could not verify credentials") {
		t.Errorf("human offline path must surface a 'Could not verify' line; got:\n%s", stdout)
	}
}

// TestWhoami_NoToken_JSON covers the anonymous --json branch.
func TestWhoami_NoToken_JSON(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()
	_ = c

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami", "--json")
		if err != nil {
			t.Fatalf("no-token whoami --json must exit 0, got %v", err)
		}
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("no-token whoami --json invalid JSON: %q (%v)", stdout, err)
	}
	if out["authenticated"] != false {
		t.Errorf("no token: authenticated must be false, got %v", out["authenticated"])
	}
	if api, _ := out["api_url"].(string); api == "" {
		t.Error("no token: api_url must still resolve to a non-empty string")
	}
}

// TestWhoami_ServerError_TreatedAsOffline drives the default switch arm in
// validateTokenWithServer: a 500 from /auth/me is "couldn't confirm" (offline),
// not "rejected".
func TestWhoami_ServerError_TreatedAsOffline(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()

	c.mock.mu.Lock()
	c.mock.authMeStatus = http.StatusInternalServerError
	c.mock.mu.Unlock()

	t.Setenv("INSTANT_TOKEN", "inst_token_5xx")
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami", "--json")
		if err == nil {
			t.Fatal("5xx /auth/me must produce a non-nil error")
		}
		if got := ExitCodeFor(err); got != ExitGeneric {
			t.Errorf("5xx exit code = %d, want %d (offline)", got, ExitGeneric)
		}
	})
	var out map[string]any
	_ = json.Unmarshal([]byte(stdout), &out)
	if note, _ := out["error"].(string); !strings.Contains(note, "could not reach the server") {
		t.Errorf("5xx must classify as offline; error note = %q", note)
	}
}

// TestWhoami_BadJSONFromServer covers the json.Unmarshal error branch in
// validateTokenWithServer (200 with a non-JSON body → treated as offline).
func TestWhoami_BadJSONFromServer(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()

	c.mock.mu.Lock()
	c.mock.authMeBadBody = true
	c.mock.mu.Unlock()

	t.Setenv("INSTANT_TOKEN", "inst_token_badjson")
	_, _ = captureStdout(t, func() {
		_, _, err := run("whoami", "--json")
		if err == nil {
			t.Fatal("garbage /auth/me body must produce a non-nil error")
		}
	})
}

// TestWhoami_TokenFlagPath covers the --token precedence branch in runWhoami
// (the global --token flag wins over a saved login / env).
func TestWhoami_TokenFlagPath(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()
	_ = c

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("--token", "inst_flag_token", "whoami", "--json")
		if err != nil {
			t.Fatalf("whoami --token --json must exit 0 (mock validates), got %v", err)
		}
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("invalid JSON: %q (%v)", stdout, err)
	}
	if out["authenticated"] != true {
		t.Errorf("--token path: authenticated must be true, got %v", out["authenticated"])
	}
}

// TestWhoami_APIURLDefaultFallback covers the api_url resolution falling all
// the way through to defaultAPIBaseURL: clear config + no INSTANT_API_URL +
// empty package APIBaseURL forces the final fallback branch. No token, so it
// never touches the network.
func TestWhoami_APIURLDefaultFallback(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()
	_ = c

	prev := APIBaseURL
	APIBaseURL = "" // force the defaultAPIBaseURL branch
	t.Cleanup(func() { APIBaseURL = prev })

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("whoami", "--json")
		if err != nil {
			t.Fatalf("no-token whoami --json must exit 0, got %v", err)
		}
	})
	var out map[string]any
	_ = json.Unmarshal([]byte(stdout), &out)
	if api, _ := out["api_url"].(string); api != defaultAPIBaseURL {
		t.Errorf("api_url must fall back to %q, got %q", defaultAPIBaseURL, api)
	}
}

// TestWhoamiErrorNote_OfflineNoErr covers the offline branch of whoamiErrorNote
// where v.err is nil (the generic "could not reach the server" string).
func TestWhoamiErrorNote_OfflineNoErr(t *testing.T) {
	got := whoamiErrorNote(whoamiValidation{offline: true})
	if got != "could not reach the server to validate the token" {
		t.Errorf("offline-no-err note = %q", got)
	}
	withErr := whoamiErrorNote(whoamiValidation{offline: true, err: errSessionExpired()})
	if !strings.Contains(withErr, "could not reach the server") {
		t.Errorf("offline-with-err note = %q", withErr)
	}
	rejected := whoamiErrorNote(whoamiValidation{})
	if rejected != "the server rejected the presented token" {
		t.Errorf("rejected note = %q", rejected)
	}
}

// TestWhoami_ConfigLoadError covers the cliconfig.Load() error branch in
// runWhoami: a malformed ~/.instant-config makes Load return an error, which
// whoami funnels through wrapJSONErr.
func TestWhoami_ConfigLoadError(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()
	_ = c

	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".instant-config")
	if err := os.WriteFile(cfgPath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(cfgPath) })

	_, _ = captureStdout(t, func() {
		_, _, err := run("whoami", "--json")
		if err == nil {
			t.Fatal("malformed config must surface a load error")
		}
	})
}

// TestValidateTokenWithServer_EmptyToken covers the early-return guard and the
// request-build error path is exercised by passing an invalid URL.
func TestValidateTokenWithServer_EmptyToken(t *testing.T) {
	if v := validateTokenWithServer("https://x.example", "   "); v.validated || v.offline {
		t.Errorf("empty token: want validated=false offline=false, got %+v", v)
	}
	// Invalid URL → http.NewRequest fails → offline with err.
	if v := validateTokenWithServer("http://\x7f", "tok"); !v.offline || v.err == nil {
		t.Errorf("bad URL: want offline with err, got %+v", v)
	}
}

// ── B2: provision verbs honor --json ────────────────────────────────────────

// TestProvision_JSON_AllVerbs asserts every provisioning `new` verb accepts
// --json and emits the full structured response (token + connection_url +
// environment). The webhook verb returns receive_url instead.
func TestProvision_JSON_AllVerbs(t *testing.T) {
	cases := []struct {
		group   string
		urlKey  string // which URL field carries the connection
		wantEnv string
	}{
		{"db", "connection_url", "development"},
		{"cache", "connection_url", "development"},
		{"nosql", "connection_url", "development"},
		{"queue", "connection_url", "development"},
		{"storage", "connection_url", "development"},
		{"vector", "connection_url", "development"},
		{"webhook", "receive_url", "development"},
	}
	for _, tc := range cases {
		t.Run(tc.group, func(t *testing.T) {
			c := newITContext(t)
			resetProvisionFlags()

			var token string
			stdout, _ := captureStdout(t, func() {
				_, _, err := run(tc.group, "new", "--name", tc.group+"-json", "--json")
				if err != nil {
					t.Fatalf("%s new --json: %v", tc.group, err)
				}
			})
			var out map[string]any
			if err := json.Unmarshal([]byte(stdout), &out); err != nil {
				t.Fatalf("%s new --json must emit valid JSON; got %q (%v)", tc.group, stdout, err)
			}
			if out["ok"] != true {
				t.Errorf("%s: ok must be true, got %v", tc.group, out["ok"])
			}
			tk, _ := out["token"].(string)
			if tk == "" {
				t.Errorf("%s: token must be present in JSON output", tc.group)
			}
			token = tk
			if u, _ := out[tc.urlKey].(string); u == "" {
				t.Errorf("%s: %s must be present, got %v", tc.group, tc.urlKey, out)
			}
			if env, _ := out["environment"].(string); env != tc.wantEnv {
				t.Errorf("%s: environment = %q, want %q", tc.group, env, tc.wantEnv)
			}
			if env, _ := out["env"].(string); env != tc.wantEnv {
				t.Errorf("%s: env = %q, want %q", tc.group, env, tc.wantEnv)
			}
			// Clean up so the mandatory leak sweep stays green.
			c.deleteResource(token)
		})
	}
}

// TestProvision_JSON_WithEnvAndOverride covers the env_override_reason field on
// the JSON output (non-empty env + server-supplied override reason).
func TestProvision_JSON_WithEnvAndOverride(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()

	c.mock.mu.Lock()
	c.mock.envOverrideReason = "anonymous callers can't target production"
	c.mock.mu.Unlock()

	var token string
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("db", "new", "--name", "ovr-db", "--env", "production", "--json")
		if err != nil {
			t.Fatalf("db new --env production --json: %v", err)
		}
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("invalid JSON: %q (%v)", stdout, err)
	}
	token, _ = out["token"].(string)
	if reason, _ := out["env_override_reason"].(string); reason == "" {
		t.Error("env_override_reason must surface in JSON output when the server set it")
	}
	c.deleteResource(token)
}

// TestProvision_JSON_ErrorEnvelope asserts a 402/limit error in --json mode
// funnels through the shared envelope (so an agent piping to jq never crashes).
func TestProvision_JSON_ErrorEnvelope(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()

	c.mock.injectErrorOnProvision(http.StatusPaymentRequired, "limit_reached",
		"deployment limit reached", "upgrade to a paid plan", "https://instanode.dev/pricing")

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("db", "new", "--name", "over-limit", "--json")
		if err == nil {
			t.Fatal("402 provision must produce a non-nil error")
		}
	})
	var env map[string]any
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("--json provision error must emit a JSON envelope; got %q (%v)", stdout, err)
	}
	if env["ok"] != false {
		t.Errorf("error envelope ok must be false, got %v", env["ok"])
	}
	if c.mock.count() != 0 {
		t.Error("failed provision must not leave a resource")
	}
}

// TestProvision_JSON_OmittedEnvDefaults covers the env-empty → "development"
// fallback feeding into the JSON output (older-API shape, no env field).
func TestProvision_JSON_OmittedEnvDefaults(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()

	c.mock.mu.Lock()
	c.mock.omitEnvInProvision = true
	c.mock.mu.Unlock()

	var token string
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("cache", "new", "--name", "noenv-cache", "--json")
		if err != nil {
			t.Fatalf("cache new --json: %v", err)
		}
	})
	var out map[string]any
	_ = json.Unmarshal([]byte(stdout), &out)
	token, _ = out["token"].(string)
	if env, _ := out["environment"].(string); env != "development" {
		t.Errorf("omitted env must default to development in JSON, got %q", env)
	}
	c.deleteResource(token)
}

// ── U2: $BROWSER + --no-browser ─────────────────────────────────────────────

// TestBrowserLauncher_HonorsBROWSER asserts $BROWSER overrides the per-GOOS
// default on every platform.
func TestBrowserLauncher_HonorsBROWSER(t *testing.T) {
	t.Setenv(browserEnvVar, "my-custom-opener")
	for _, goos := range []string{"darwin", "linux", "windows", "plan9", ""} {
		name, args := browserLauncherForGOOS(goos, "https://instanode.dev/x")
		if name != "my-custom-opener" {
			t.Errorf("goos=%q: $BROWSER must win, got launcher %q", goos, name)
		}
		if len(args) != 1 || args[0] != "https://instanode.dev/x" {
			t.Errorf("goos=%q: $BROWSER launcher must receive the URL as a single arg, got %v", goos, args)
		}
	}
}

// TestBrowserLauncher_BROWSERWhitespaceIgnored asserts a whitespace-only
// $BROWSER is treated as unset (falls through to the per-GOOS default).
func TestBrowserLauncher_BROWSERWhitespaceIgnored(t *testing.T) {
	t.Setenv(browserEnvVar, "   ")
	name, _ := browserLauncherForGOOS("linux", "https://instanode.dev/x")
	if name != "xdg-open" {
		t.Errorf("whitespace $BROWSER must fall through to xdg-open, got %q", name)
	}
}

// TestLogin_NoBrowser_PrintsURLAndPolls covers the --no-browser path: the auth
// URL + session id are printed and login completes via polling WITHOUT ever
// invoking openBrowser. The mock completes auth immediately.
func TestLogin_NoBrowser_PrintsURLAndPolls(t *testing.T) {
	c := newITContext(t)
	_ = cliconfig.Clear()
	_ = secretstore.Delete()
	resetJSONFlags()

	// Make the device-flow poll resolve on the first tick.
	c.mock.mu.Lock()
	c.mock.authComplete = true
	c.mock.mu.Unlock()

	// Speed the poll up so the test is fast.
	prevInterval := pollInterval
	pollInterval = 1
	t.Cleanup(func() { pollInterval = prevInterval })

	loginNoBrowser = true
	t.Cleanup(func() { loginNoBrowser = false })

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("login", "--no-browser")
		if err != nil {
			t.Fatalf("login --no-browser: %v", err)
		}
	})
	if !strings.Contains(stdout, "Open this URL to sign in:") {
		t.Errorf("--no-browser must print the auth URL; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Session:") {
		t.Errorf("--no-browser must print the session id; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Logged in as") {
		t.Errorf("--no-browser must still complete login via polling; got:\n%s", stdout)
	}
}

// pointAtDeadServer re-points the package-global APIBaseURL at an immediately
// closed httptest server so the next CLI request fails at the transport layer
// (drives whoami's offline path). initConfig leaves APIBaseURL untouched when
// neither INSTANT_API_URL nor a saved config is set, so this value survives the
// run() → OnInitialize cycle.
func pointAtDeadServer() {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()
	APIBaseURL = dead.URL
}
