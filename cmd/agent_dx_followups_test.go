package cmd

// agent_dx_followups_test.go — coverage for the four agent-DX follow-ups
// shipped in fix/cli-creds-verb-webhook-url-401-token (cohort dogfood round 2,
// follow-ups to #32):
//
//   F1  `instant resource creds <token>` (alias credentials) re-fetches the
//       connection URL via GET …/credentials — the recovery path for a
//       provision whose `new` call timed out before printing the URL.
//   F2  `webhook new` prints the receive_url (it was printing a blank `url`
//       because the human path read creds.ConnectionURL only).
//   F3  a 401 with INSTANT_TOKEN set advises fixing/unsetting INSTANT_TOKEN
//       (which shadows any saved login), not `instant login`.
//   F4  `instant resources` prints the FULL token (un-copyable truncation
//       fixed) and truncates the NAME column instead when long.
//
// Tests follow the established httptest-mock conventions (operateServer /
// newITContext + captureStdout) used by operate_test.go and integration_test.go.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// ── F1: instant resource creds <token> ───────────────────────────────────────

// TestF1_ResourceCreds_RefetchesConnectionURL is the headline F1 regression:
// after a `db new` that timed out client-side, the connection URL is only
// recoverable via GET /api/v1/resources/:token/credentials. `instant resource
// creds <token>` must GET exactly that path and print the connection_url.
func TestF1_ResourceCreds_RefetchesConnectionURL(t *testing.T) {
	var gotMethod, gotPath string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = fmt.Fprint(w, `{"ok":true,"id":"res-9","token":"tok-1","resource_type":"postgres","env":"production","connection_url":"postgres://u:p@host/db"}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "creds", "tok-1")
		if err != nil {
			t.Fatalf("resource creds: %v", err)
		}
	})
	if gotMethod != http.MethodGet || gotPath != "/api/v1/resources/tok-1/credentials" {
		t.Errorf("must GET /api/v1/resources/:token/credentials, got %s %s", gotMethod, gotPath)
	}
	for _, want := range []string{"tok-1", "postgres://u:p@host/db", "postgres", "production"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("creds output missing %q: %q", want, stdout)
		}
	}
}

// TestF1_ResourceCredentialsAlias asserts the `credentials` alias hits the same
// path as `creds`.
func TestF1_ResourceCredentialsAlias(t *testing.T) {
	var gotPath string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = fmt.Fprint(w, `{"ok":true,"token":"tok-2","connection_url":"redis://h:6379"}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "credentials", "tok-2")
		if err != nil {
			t.Fatalf("resource credentials: %v", err)
		}
	})
	if gotPath != "/api/v1/resources/tok-2/credentials" {
		t.Errorf("credentials alias must GET …/credentials, got %s", gotPath)
	}
	if !strings.Contains(stdout, "redis://h:6379") {
		t.Errorf("alias output missing url: %q", stdout)
	}
}

// TestF1_ResourceCreds_JSON asserts --json emits the full structured response.
func TestF1_ResourceCreds_JSON(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"id":"res-1","token":"tok-1","resource_type":"postgres","env":"production","connection_url":"postgres://u:p@host/db"}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "creds", "tok-1", "--json")
		if err != nil {
			t.Fatalf("resource creds --json: %v", err)
		}
	})
	var res resourceCredentialsResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%q", err, stdout)
	}
	if res.ConnectionURL != "postgres://u:p@host/db" || res.ResourceType != "postgres" || res.Env != "production" {
		t.Errorf("unexpected JSON payload: %+v", res)
	}
}

// TestF1_ResourceCreds_WebhookReceiveURLFallback asserts a credentials response
// with no connection_url (a webhook) falls back to receive_url in the human path.
func TestF1_ResourceCreds_WebhookReceiveURLFallback(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"token":"tok-h","resource_type":"webhook","receive_url":"https://hooks.instanode.dev/tok-h"}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "creds", "tok-h")
		if err != nil {
			t.Fatalf("resource creds (webhook): %v", err)
		}
	})
	if !strings.Contains(stdout, "https://hooks.instanode.dev/tok-h") {
		t.Errorf("webhook creds must fall back to receive_url, got %q", stdout)
	}
}

// TestF1_ResourceCreds_TokenFallback asserts that when the credentials
// response omits `token` (older API shape), the CLI falls back to the
// argument token in the printed `ok` line.
func TestF1_ResourceCreds_TokenFallback(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		// No `token` field in the response body.
		_, _ = fmt.Fprint(w, `{"ok":true,"connection_url":"postgres://u:p@host/db"}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "creds", "tok-arg")
		if err != nil {
			t.Fatalf("resource creds (no token in body): %v", err)
		}
	})
	if !strings.Contains(stdout, "ok    creds  tok-arg") {
		t.Errorf("must fall back to the argument token when the body omits it, got %q", stdout)
	}
}

// TestF1_ResourceCreds_Unauthenticated asserts the verb short-circuits with
// exit 3 BEFORE any API round trip when anonymous (it's auth-required: the
// token in the URL identifies the resource, not the caller).
func TestF1_ResourceCreds_Unauthenticated(t *testing.T) {
	called := false
	operateServer(t, false, func(w http.ResponseWriter, r *http.Request) { called = true })
	_, _, err := run("resource", "creds", "tok-1")
	if err == nil || !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("expected auth-required error, got %v", err)
	}
	if ExitCodeFor(err) != ExitAuthRequired {
		t.Errorf("exit code = %d, want %d", ExitCodeFor(err), ExitAuthRequired)
	}
	if called {
		t.Error("anonymous creds must short-circuit BEFORE the API round trip")
	}
}

// TestF1_ResourceCreds_MissingToken asserts a missing token is a usage error.
func TestF1_ResourceCreds_MissingToken(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		t.Error("missing token must fail before any API call")
	})
	for _, verb := range []string{"creds", "credentials"} {
		_, _, err := run("resource", verb)
		if err == nil || !strings.Contains(err.Error(), "token argument is required") {
			t.Errorf("resource %s (no token): expected usage error, got %v", verb, err)
		}
	}
}

// TestF1_ResourceCreds_EmptyToken drives the trimmed-empty-token guard.
func TestF1_ResourceCreds_EmptyToken(t *testing.T) {
	err := runResourceCredentials("   ")
	if err == nil || !strings.Contains(err.Error(), "token is required") {
		t.Errorf("expected token-required error, got %v", err)
	}
}

// TestF1_ResourceCreds_ParseError drives the malformed-body branch.
func TestF1_ResourceCreds_ParseError(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{not-json`)
	})
	var err error
	_, _ = captureStdout(t, func() { _, _, err = run("resource", "creds", "tok-1") })
	if err == nil || !strings.Contains(err.Error(), "parsing response") {
		t.Errorf("expected parse error, got %v", err)
	}
}

// TestF1_ResourceCreds_ServerError asserts a non-2xx surfaces the envelope.
func TestF1_ResourceCreds_ServerError(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"ok":false,"error":"no_connection_url","message":"This resource does not have a connection URL"}`)
	})
	var err error
	_, _ = captureStdout(t, func() { _, _, err = run("resource", "creds", "tok-1") })
	if err == nil || !strings.Contains(err.Error(), "does not have a connection URL") {
		t.Errorf("expected 400 envelope surfaced, got %v", err)
	}
}

// ── F2: webhook new prints receive_url ───────────────────────────────────────

// TestF2_WebhookNew_PrintsReceiveURL is the headline F2 regression: a webhook
// provision returns receive_url (NOT connection_url), and the human print path
// must show it instead of a blank `url` line.
func TestF2_WebhookNew_PrintsReceiveURL(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()
	out, _ := c.provisionViaCLI("webhook", "app-hook")

	// The mock mints receive_url = https://hooks.instanode.dev/<token>.
	if !strings.Contains(out, "https://hooks.instanode.dev/") {
		t.Errorf("webhook new must print the receive URL, got %q", out)
	}
	// Regression pin: the url line must NOT be blank.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "url") && strings.TrimSpace(strings.TrimPrefix(line, "url")) == "" {
			t.Errorf("webhook new printed a BLANK url line: %q", out)
		}
	}
}

// TestF2_WebhookNew_JSONHasReceiveURL verifies the #32 --json provision output
// already carries receive_url for webhooks (rule 12: verify, don't assume).
func TestF2_WebhookNew_JSONHasReceiveURL(t *testing.T) {
	c := newITContext(t)
	resetProvisionFlags()

	var token string
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("webhook", "new", "--name", "json-hook", "--json")
		if err != nil {
			t.Fatalf("webhook new --json: %v", err)
		}
	})
	var out provisionJSONOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("webhook new --json output is not JSON: %v\n%q", err, stdout)
	}
	if out.ReceiveURL == "" {
		t.Errorf("webhook new --json must include receive_url, got %+v", out)
	}
	if out.ConnectionURL != "" {
		t.Errorf("webhook new --json must NOT carry a connection_url, got %q", out.ConnectionURL)
	}
	token = out.Token
	t.Cleanup(func() { c.deleteResource(token) })
}

// ── F3: 401 with INSTANT_TOKEN set gives env-aware advice ────────────────────

// TestF3_SessionExpired_EnvTokenAdvice asserts that when the rejected token
// came from INSTANT_TOKEN, the message tells the user to fix/unset
// INSTANT_TOKEN (which shadows any saved login) instead of `instant login`.
func TestF3_SessionExpired_EnvTokenAdvice(t *testing.T) {
	operateServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	// INSTANT_TOKEN is the token source here (operateServer(false) cleared any
	// saved login). initConfig fires on Execute and wires it via authTransport.
	t.Setenv("INSTANT_TOKEN", "inst_bogus_env_token")

	_, _, err := run("resource", "creds", "tok-1")
	if err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Fatalf("expected session-expired on 401-with-env-token, got %v", err)
	}
	if !strings.Contains(err.Error(), "INSTANT_TOKEN") {
		t.Errorf("env-token 401 must advise fixing/unsetting INSTANT_TOKEN, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "run `instant login`") {
		t.Errorf("env-token 401 must NOT advise `instant login` (shadowed), got %q", err.Error())
	}
	if ExitCodeFor(err) != ExitSessionExpired {
		t.Errorf("exit code = %d, want %d", ExitCodeFor(err), ExitSessionExpired)
	}
}

// TestF3_SessionExpired_SavedLoginAdvice asserts the ORIGINAL `instant login`
// advice still fires when the rejected token came from a saved login (no env).
func TestF3_SessionExpired_SavedLoginAdvice(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, _, err := run("resource", "creds", "tok-1")
	if err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Fatalf("expected session-expired on 401-with-saved-login, got %v", err)
	}
	if !strings.Contains(err.Error(), "run `instant login`") {
		t.Errorf("saved-login 401 must advise `instant login`, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "INSTANT_TOKEN is set") {
		t.Errorf("saved-login 401 must NOT mention INSTANT_TOKEN shadowing, got %q", err.Error())
	}
}

// TestF3_SessionExpired_EnvTokenJSONAction asserts the --json envelope's
// agent_action branches on the env-token source too.
func TestF3_SessionExpired_EnvTokenJSONAction(t *testing.T) {
	operateServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	resetJSONFlags() // clear any --json toggle leaked by a prior test
	t.Setenv("INSTANT_TOKEN", "inst_bogus_env_token")

	stdout, _ := captureStdout(t, func() {
		_, _, _ = run("resources", "--json")
	})
	var env jsonErrorEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("--json error must be a JSON envelope: %v\n%q", err, stdout)
	}
	if env.Error != "session_expired" {
		t.Errorf("error code = %q, want session_expired", env.Error)
	}
	if !strings.Contains(env.AgentAction, "INSTANT_TOKEN") {
		t.Errorf("env-token agent_action must mention INSTANT_TOKEN, got %q", env.AgentAction)
	}
}

// TestF3_AuthFromEnvToken_FlagOverridesEnv asserts a --token flag wins over
// INSTANT_TOKEN, so authFromEnvToken() reports false (the flag, not env, is the
// source).
func TestF3_AuthFromEnvToken_FlagOverridesEnv(t *testing.T) {
	t.Setenv("INSTANT_TOKEN", "env-tok")
	prev := adHocToken
	adHocToken = "flag-tok"
	t.Cleanup(func() { adHocToken = prev })
	if authFromEnvToken() {
		t.Error("authFromEnvToken must be false when --token overrides INSTANT_TOKEN")
	}
}

// ── F4: resources table prints the full token ────────────────────────────────

// TestF4_ResourcesTable_FullToken asserts the `instant resources` table prints
// the FULL token (the exact argument every other command needs), never the old
// `<prefix>…` truncation.
func TestF4_ResourcesTable_FullToken(t *testing.T) {
	const fullToken = "d3cef90f-a75e-4c1c-9b2e-0123456789ab"
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"total":1,"items":[
			{"token":%q,"resource_type":"postgres","name":"app-db","tier":"hobby","status":"active"}
		]}`, fullToken)
	})
	resetJSONFlags() // clear any --json toggle leaked by a prior test
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resources")
		if err != nil {
			t.Fatalf("resources: %v", err)
		}
	})
	if !strings.Contains(stdout, fullToken) {
		t.Errorf("resources table must print the FULL token %q, got %q", fullToken, stdout)
	}
	if strings.Contains(stdout, fullToken[:12]+"…") {
		t.Errorf("resources table must NOT truncate the token, got %q", stdout)
	}
}

// TestF4_ResourcesTable_TruncatesLongName asserts a long NAME is truncated with
// an ellipsis (the column we trade off so the token stays full) while a short
// name renders verbatim and an empty name renders "-".
func TestF4_ResourcesTable_TruncatesLongName(t *testing.T) {
	longName := strings.Repeat("x", nameDisplayMaxLen+5)
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"total":3,"items":[
			{"token":"tok-long","resource_type":"postgres","name":%q,"tier":"hobby","status":"active"},
			{"token":"tok-short","resource_type":"redis","name":"short","tier":"hobby","status":"active"},
			{"token":"tok-empty","resource_type":"webhook","name":"","tier":"anonymous","status":"active"}
		]}`, longName)
	})
	resetJSONFlags() // clear any --json toggle leaked by a prior test
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resources")
		if err != nil {
			t.Fatalf("resources: %v", err)
		}
	})
	if !strings.Contains(stdout, "…") {
		t.Errorf("long name must be truncated with an ellipsis, got %q", stdout)
	}
	if strings.Contains(stdout, longName) {
		t.Errorf("the full long name must NOT appear, got %q", stdout)
	}
	if !strings.Contains(stdout, "short") {
		t.Errorf("a short name must render verbatim, got %q", stdout)
	}
	// Each full token must survive.
	for _, tok := range []string{"tok-long", "tok-short", "tok-empty"} {
		if !strings.Contains(stdout, tok) {
			t.Errorf("token %q must print in full, got %q", tok, stdout)
		}
	}
}

// TestF4_TruncateName_Unit drives truncateName directly across its branches —
// empty, within-cap, over-cap, and a multibyte name (rune-boundary safety).
func TestF4_TruncateName_Unit(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "-"},
		{"app-db", "app-db"},
		{strings.Repeat("a", nameDisplayMaxLen), strings.Repeat("a", nameDisplayMaxLen)},
		{strings.Repeat("a", nameDisplayMaxLen+1), strings.Repeat("a", nameDisplayMaxLen) + "…"},
		{strings.Repeat("é", nameDisplayMaxLen+2), strings.Repeat("é", nameDisplayMaxLen) + "…"},
	}
	for _, tc := range cases {
		if got := truncateName(tc.in); got != tc.want {
			t.Errorf("truncateName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
