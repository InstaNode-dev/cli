package cmd

// operate_test.go — contract coverage for the Wave-2 A4 operate verbs
// (operate.go + the resourceCmd dispatch additions in extras.go).
//
// Every test drives the real cobra tree via run() against a hermetic
// httptest server that ASSERTS method + path + body — the flag→payload→
// endpoint mapping is the contract being pinned (MCP parity: the endpoints
// must match mcp/src/client.ts exactly).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InstaNode-dev/cli/internal/cliconfig"
	"github.com/InstaNode-dev/cli/internal/secretstore"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// resetOperateFlags clears every operate-verb flag binding + the cobra
// Changed markers so per-test flag state never leaks (the same discipline
// resetProvisionFlags applies to the provisioning groups).
func resetOperateFlags() {
	operateJSON = false
	vaultValue = ""
	presignKey = ""
	presignOperation = "GET"
	presignExpiresIn = 0
	deployEventsLimit = 0
	resourceDetailJSON = false
	for _, c := range []*cobra.Command{
		vaultSetCmd, vaultRotateCmd, deployEnvCmd, deployWakeCmd,
		deployEventsCmd, stackEnvCmd, storagePresignCmd, capabilitiesCmd,
		resourceCmd,
	} {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
}

// operateServer points the package-global CLI client at a hermetic handler.
//
// Because cobra.OnInitialize(initConfig) rebuilds HTTPClient on EVERY
// Execute(), an injected transport would be stomped before RunE fires. So
// authed=true persists a PAT via cliconfig (the same seam authSetupForTest
// uses) and lets initConfig wire it; authed=false guarantees no saved
// credentials so the client stays anonymous.
func operateServer(t *testing.T, authed bool, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	prevURL, prevClient := APIBaseURL, HTTPClient
	APIBaseURL = srv.URL
	if authed {
		t.Cleanup(authSetupForTest(t, srv.URL))
	} else {
		// Make sure a previous test's saved login can't leak in.
		_ = cliconfig.Clear()
		_ = secretstore.Delete()
		initConfig()
		APIBaseURL = srv.URL
	}
	resetOperateFlags()
	t.Cleanup(func() {
		APIBaseURL, HTTPClient = prevURL, prevClient
		resetOperateFlags()
	})
	return srv
}

// errReader fails every Read — drives the stdin-read error branch.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("stdin exploded") }

// ── group help ───────────────────────────────────────────────────────────────

func TestOperate_BareGroupsPrintHelp(t *testing.T) {
	for _, group := range []string{"vault", "stack"} {
		stdout, _, err := run(group)
		if err != nil {
			t.Errorf("bare `instant %s` should print help and exit 0, got %v", group, err)
		}
		if !strings.Contains(stdout, "Usage:") {
			t.Errorf("bare `instant %s` help missing Usage block: %q", group, stdout)
		}
	}
}

// ── vault set / rotate ───────────────────────────────────────────────────────

func TestOperate_VaultSet(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"ok":true,"key":"STRIPE_KEY","env":"production","version":1}`)
	})

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("vault", "set", "production", "STRIPE_KEY", "--value", "sk_live_1")
		if err != nil {
			t.Fatalf("vault set: %v", err)
		}
	})
	if gotMethod != http.MethodPut || gotPath != "/api/v1/vault/production/STRIPE_KEY" {
		t.Errorf("vault set must PUT /api/v1/vault/:env/:key, got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"value":"sk_live_1"`) {
		t.Errorf("body must carry {value}, got %q", gotBody)
	}
	if !strings.Contains(stdout, "vault set") || !strings.Contains(stdout, "version 1") {
		t.Errorf("missing confirmation line: %q", stdout)
	}
	if !strings.Contains(stdout, "vault://production/STRIPE_KEY") {
		t.Errorf("missing vault:// ref hint: %q", stdout)
	}
}

func TestOperate_VaultSet_StdinValue(t *testing.T) {
	var gotBody string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_, _ = fmt.Fprint(w, `{"ok":true,"key":"K","env":"e","version":1}`)
	})
	rootCmd.SetIn(strings.NewReader("piped-secret\n"))
	t.Cleanup(func() { rootCmd.SetIn(nil) })

	_, _ = captureStdout(t, func() {
		_, _, err := run("vault", "set", "e", "K")
		if err != nil {
			t.Fatalf("vault set via stdin: %v", err)
		}
	})
	if !strings.Contains(gotBody, `"value":"piped-secret"`) {
		t.Errorf("stdin value must be trimmed + forwarded, got %q", gotBody)
	}
}

func TestOperate_VaultSet_StdinReadError(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {})
	rootCmd.SetIn(errReader{})
	t.Cleanup(func() { rootCmd.SetIn(nil) })

	_, _, err := run("vault", "set", "e", "K")
	if err == nil || !strings.Contains(err.Error(), "reading secret value from stdin") {
		t.Errorf("expected stdin read error, got %v", err)
	}
}

func TestOperate_VaultSet_EmptyValue(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {})
	rootCmd.SetIn(strings.NewReader("\n"))
	t.Cleanup(func() { rootCmd.SetIn(nil) })

	_, _, err := run("vault", "set", "e", "K")
	if err == nil || !strings.Contains(err.Error(), "secret value required") {
		t.Errorf("expected empty-value error, got %v", err)
	}
}

func TestOperate_VaultRotate(t *testing.T) {
	var gotMethod, gotPath string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = fmt.Fprint(w, `{"ok":true,"key":"K","env":"production","version":2}`)
	})

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("vault", "rotate", "production", "K", "--value", "new-secret")
		if err != nil {
			t.Fatalf("vault rotate: %v", err)
		}
	})
	if gotMethod != http.MethodPost || gotPath != "/api/v1/vault/production/K/rotate" {
		t.Errorf("vault rotate must POST …/rotate, got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(stdout, "rotated") || !strings.Contains(stdout, "version 2") {
		t.Errorf("missing rotation confirmation: %q", stdout)
	}
	if !strings.Contains(stdout, "redeploy any app") {
		t.Errorf("missing redeploy reminder: %q", stdout)
	}
}

func TestOperate_VaultRotate_JSON(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"key":"K","env":"e","version":3}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("vault", "rotate", "e", "K", "--value", "v", "--json")
		if err != nil {
			t.Fatalf("vault rotate --json: %v", err)
		}
	})
	var res vaultWriteResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%q", err, stdout)
	}
	if !res.OK || res.Version != 3 {
		t.Errorf("unexpected JSON payload: %+v", res)
	}
}

// ── shared error contract (doOperate) ────────────────────────────────────────

func TestOperate_Unauthenticated_Exit3(t *testing.T) {
	called := false
	operateServer(t, false, func(w http.ResponseWriter, r *http.Request) { called = true })

	_, _, err := run("vault", "set", "e", "K", "--value", "v")
	if err == nil || !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("expected auth-required error, got %v", err)
	}
	if ExitCodeFor(err) != ExitAuthRequired {
		t.Errorf("exit code = %d, want %d", ExitCodeFor(err), ExitAuthRequired)
	}
	if called {
		t.Error("anonymous operate verb must short-circuit BEFORE the API round trip")
	}
}

func TestOperate_SessionExpired_On401(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, _, err := run("deploy", "wake", "my-app")
	if err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Errorf("expected session-expired on 401-with-auth, got %v", err)
	}
}

func TestOperate_APIErrorEnvelope(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = fmt.Fprint(w, `{"ok":false,"error":"tier_gated","message":"Pause requires Pro.","agent_action":"upgrade"}`)
	})
	_, _, err := run("resource", "pause", "tok-1")
	if err == nil || !strings.Contains(err.Error(), "Pause requires Pro.") {
		t.Errorf("expected structured 402 envelope surfaced, got %v", err)
	}
}

func TestOperate_BadBaseURL_RequestBuildError(t *testing.T) {
	operateServer(t, false, func(w http.ResponseWriter, r *http.Request) {})
	// initConfig (cobra.OnInitialize) re-resolves APIBaseURL on every
	// Execute, so the override must come through the env seam.
	t.Setenv("INSTANT_API_URL", ":") // unparseable → http.NewRequest fails
	_, _, err := run("capabilities")
	if err == nil || !strings.Contains(err.Error(), "missing protocol scheme") {
		t.Errorf("expected URL-parse error, got %v", err)
	}
}

func TestOperate_TransportError(t *testing.T) {
	srv := operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {})
	srv.Close() // connection refused from here on
	_, _, err := run("capabilities")
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("expected wrapped transport error, got %v", err)
	}
}

// TestOperate_ServerErrorPropagation pins the doOperate error propagation in
// EVERY runner — a 503 must surface as an error from each command, never a
// zero-value success print.
func TestOperate_ServerErrorPropagation(t *testing.T) {
	cases := [][]string{
		{"deploy", "env", "app-1", "A=1"},
		{"deploy", "events", "app-1"},
		{"storage", "presign", "tok-1", "--key", "a.txt"},
		{"resource", "backups", "tok-1"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprint(w, `{"ok":false,"error":"backend_down","message":"backend down"}`)
			})
			var err error
			_, _ = captureStdout(t, func() { _, _, err = run(args...) })
			if err == nil || !strings.Contains(err.Error(), "backend down") {
				t.Errorf("%v: expected 503 envelope surfaced, got %v", args, err)
			}
		})
	}
}

// TestOperate_ParseResponseErrors drives the per-runner "parsing response"
// branch for EVERY operate verb — a malformed success body must surface as a
// parse error, never a zero-value success print.
func TestOperate_ParseResponseErrors(t *testing.T) {
	cases := [][]string{
		{"vault", "set", "e", "K", "--value", "v"},
		{"deploy", "env", "app-1", "A=1"},
		{"deploy", "wake", "app-1"},
		{"deploy", "events", "app-1"},
		{"stack", "env", "stk-1", "A=1"},
		{"storage", "presign", "tok-1", "--key", "a.txt"},
		{"capabilities"},
		{"capabilities", "--json"},
		{"resource", "pause", "tok-1"},
		{"resource", "backups", "tok-1"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, `{not-json`)
			})
			var err error
			_, _ = captureStdout(t, func() { _, _, err = run(args...) })
			if err == nil || !strings.Contains(err.Error(), "parsing response") {
				t.Errorf("%v: expected parse error, got %v", args, err)
			}
		})
	}
}

// ── env-patch (deploy + stack) ───────────────────────────────────────────────

func TestOperate_DeployEnvPatch(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody struct {
		Env map[string]string `json:"env"`
	}
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = fmt.Fprint(w, `{"ok":true,"env":{"B":"two","A":"1"},"note":"redeploy to apply"}`)
	})

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("deploy", "env", "my-app", "A=1", "B=two")
		if err != nil {
			t.Fatalf("deploy env: %v", err)
		}
	})
	if gotMethod != http.MethodPatch || gotPath != "/deploy/my-app/env" {
		t.Errorf("must PATCH /deploy/:id/env, got %s %s", gotMethod, gotPath)
	}
	if gotBody.Env["A"] != "1" || gotBody.Env["B"] != "two" {
		t.Errorf("body env map wrong: %+v", gotBody.Env)
	}
	// Merged env renders sorted; note is surfaced.
	iA, iB := strings.Index(stdout, "A=1"), strings.Index(stdout, "B=two")
	if iA < 0 || iB < 0 || iA > iB {
		t.Errorf("merged env must render sorted, got %q", stdout)
	}
	if !strings.Contains(stdout, "redeploy to apply") {
		t.Errorf("note missing: %q", stdout)
	}
}

func TestOperate_DeployEnvPatch_JSON(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"env":{"A":"1"}}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("deploy", "env", "my-app", "A=1", "--json")
		if err != nil {
			t.Fatalf("deploy env --json: %v", err)
		}
	})
	var res envPatchResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%q", err, stdout)
	}
	if res.Env["A"] != "1" {
		t.Errorf("unexpected JSON payload: %+v", res)
	}
}

func TestOperate_EnvPatch_InvalidPair(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid pair must fail before any API call")
	})
	for _, bad := range []string{"NOEQUALS", "=value"} {
		_, _, err := run("deploy", "env", "my-app", bad)
		if err == nil || !strings.Contains(err.Error(), "invalid env pair") {
			t.Errorf("pair %q: expected usage error, got %v", bad, err)
		}
	}
}

func TestOperate_StackEnvPatch(t *testing.T) {
	var gotPath string
	var gotBody struct {
		Env map[string]string `json:"env"`
	}
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = fmt.Fprint(w, `{"ok":true,"env":{"KEEP":"yes"},"message":"stack env merged"}`)
	})

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("stack", "env", "my-stack", "KEEP=yes", "OLD=")
		if err != nil {
			t.Fatalf("stack env: %v", err)
		}
	})
	if gotPath != "/stacks/my-stack/env" {
		t.Errorf("must PATCH /stacks/:slug/env, got %s", gotPath)
	}
	// Empty value forwarded verbatim (deletes the key server-side).
	if v, ok := gotBody.Env["OLD"]; !ok || v != "" {
		t.Errorf("empty value must be forwarded for deletion, got %+v", gotBody.Env)
	}
	if !strings.Contains(stdout, "stack env merged") {
		t.Errorf("message missing from output: %q", stdout)
	}
}

func TestOperate_EnvPatch_DefaultNote(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"env":{}}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("stack", "env", "my-stack", "A=1")
		if err != nil {
			t.Fatalf("stack env: %v", err)
		}
	})
	if !strings.Contains(stdout, "redeploy to apply") {
		t.Errorf("default redeploy note missing: %q", stdout)
	}
}

// ── deploy wake / events ─────────────────────────────────────────────────────

func TestOperate_DeployWake(t *testing.T) {
	var gotMethod, gotPath string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = fmt.Fprint(w, `{"ok":true,"message":"waking my-app"}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("deploy", "wake", "my-app")
		if err != nil {
			t.Fatalf("deploy wake: %v", err)
		}
	})
	if gotMethod != http.MethodPost || gotPath != "/deploy/my-app/wake" {
		t.Errorf("must POST /deploy/:id/wake, got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(stdout, "waking my-app") {
		t.Errorf("server message missing: %q", stdout)
	}
}

func TestOperate_DeployWake_DefaultMessageAndJSON(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("deploy", "wake", "my-app")
		if err != nil {
			t.Fatalf("deploy wake: %v", err)
		}
	})
	if !strings.Contains(stdout, "wake requested") {
		t.Errorf("default message missing: %q", stdout)
	}

	resetOperateFlags()
	stdout, _ = captureStdout(t, func() {
		_, _, err := run("deploy", "wake", "my-app", "--json")
		if err != nil {
			t.Fatalf("deploy wake --json: %v", err)
		}
	})
	var res wakeResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%q", err, stdout)
	}
}

func TestOperate_DeployEvents(t *testing.T) {
	var gotPath, gotQuery string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = fmt.Fprint(w, `{"ok":true,"count":2,"events":[
			{"kind":"failure_autopsy","reason":"OOMKilled","exit_code":137,"hint":"raise memory","last_lines":"fatal: out of memory","created_at":"2026-06-11T00:00:00Z"},
			{"kind":"failure_autopsy","reason":"ProgressDeadlineExceeded","exit_code":null,"created_at":"2026-06-10T00:00:00Z"}
		]}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("deploy", "events", "my-app", "--limit", "10")
		if err != nil {
			t.Fatalf("deploy events: %v", err)
		}
	})
	if gotPath != "/api/v1/deployments/my-app/events" {
		t.Errorf("must GET /api/v1/deployments/:id/events, got %s", gotPath)
	}
	if gotQuery != "limit=10" {
		t.Errorf("--limit must forward as query, got %q", gotQuery)
	}
	for _, want := range []string{"OOMKilled", "exit=137", "raise memory", "out of memory", "exit=-", "count 2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("events output missing %q: %q", want, stdout)
		}
	}
}

func TestOperate_DeployEvents_EmptyAndJSON(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("no --limit → no query param, got %q", r.URL.RawQuery)
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"count":0,"events":[]}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("deploy", "events", "my-app")
		if err != nil {
			t.Fatalf("deploy events: %v", err)
		}
	})
	if !strings.Contains(stdout, "no events") {
		t.Errorf("empty-events message missing: %q", stdout)
	}

	resetOperateFlags()
	stdout, _ = captureStdout(t, func() {
		_, _, err := run("deploy", "events", "my-app", "--json")
		if err != nil {
			t.Fatalf("deploy events --json: %v", err)
		}
	})
	var res deployEventsResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%q", err, stdout)
	}
}

// ── storage presign ──────────────────────────────────────────────────────────

func TestOperate_StoragePresign(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	var sawAuthHeader bool
	operateServer(t, false /* ANONYMOUS — broker mode, token in URL */, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		sawAuthHeader = r.Header.Get("Authorization") != ""
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = fmt.Fprint(w, `{"ok":true,"url":"https://s3.instanode.dev/x?sig=1","method":"PUT","key":"a/b.txt","object_key":"t1/a/b.txt","expires_at":"2026-06-11T01:00:00Z"}`)
	})

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("storage", "presign", "tok-1", "--key", "a/b.txt", "--operation", "PUT", "--expires-in", "900")
		if err != nil {
			t.Fatalf("storage presign: %v", err)
		}
	})
	if gotMethod != http.MethodPost || gotPath != "/storage/tok-1/presign" {
		t.Errorf("must POST /storage/:token/presign, got %s %s", gotMethod, gotPath)
	}
	if sawAuthHeader {
		t.Error("anonymous presign must not send an Authorization header")
	}
	if gotBody["operation"] != "PUT" || gotBody["key"] != "a/b.txt" || gotBody["expires_in"] != float64(900) {
		t.Errorf("presign body wrong: %+v", gotBody)
	}
	for _, want := range []string{"https://s3.instanode.dev/x?sig=1", "t1/a/b.txt", "2026-06-11T01:00:00Z"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("presign output missing %q: %q", want, stdout)
		}
	}
}

func TestOperate_StoragePresign_DefaultsAndJSON(t *testing.T) {
	var gotBody map[string]any
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = fmt.Fprint(w, `{"ok":true,"url":"u","method":"GET","key":"k","object_key":"p/k","expires_at":"e"}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("storage", "presign", "tok-1", "--key", "k", "--json")
		if err != nil {
			t.Fatalf("storage presign --json: %v", err)
		}
	})
	if gotBody["operation"] != "GET" {
		t.Errorf("default operation must be GET, got %+v", gotBody)
	}
	if _, present := gotBody["expires_in"]; present {
		t.Errorf("omitted --expires-in must not send expires_in, got %+v", gotBody)
	}
	var res presignResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%q", err, stdout)
	}
}

func TestOperate_StoragePresign_KeyRequired(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		t.Error("missing --key must fail before any API call")
	})
	_, _, err := run("storage", "presign", "tok-1")
	if err == nil || !strings.Contains(err.Error(), `"key"`) {
		t.Errorf("expected required-flag error for --key, got %v", err)
	}
}

// ── capabilities ─────────────────────────────────────────────────────────────

func TestOperate_Capabilities(t *testing.T) {
	var gotPath string
	var sawAuthHeader bool
	operateServer(t, false /* anonymous — capabilities is public */, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		sawAuthHeader = r.Header.Get("Authorization") != ""
		_, _ = fmt.Fprint(w, `{"ok":true,"docs":"https://instanode.dev/llms-full.txt","contact":"mailto:x@instanode.dev","tiers":[
			{"tier":"hobby","display_name":"Hobby","price_usd_monthly":9,"deployments_apps":1,"backup_retention_days":7,"manual_backups_per_day":1},
			{"tier":"team","display_name":"Team","price_usd_monthly":199,"deployments_apps":-1,"backup_retention_days":90,"manual_backups_per_day":1000}
		]}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("capabilities")
		if err != nil {
			t.Fatalf("capabilities: %v", err)
		}
	})
	if gotPath != "/api/v1/capabilities" {
		t.Errorf("must GET /api/v1/capabilities, got %s", gotPath)
	}
	if sawAuthHeader {
		t.Error("anonymous capabilities read must work without auth")
	}
	for _, want := range []string{"hobby", "$9", "unlimited", "90d", "llms-full.txt"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("capabilities table missing %q: %q", want, stdout)
		}
	}
}

func TestOperate_Capabilities_JSONVerbatim(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"tiers":[],"contact":"mailto:x@instanode.dev"}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("capabilities", "--json")
		if err != nil {
			t.Fatalf("capabilities --json: %v", err)
		}
	})
	// --json must pass the full response through (fields the table drops,
	// like `contact`, survive).
	if !strings.Contains(stdout, "mailto:x@instanode.dev") {
		t.Errorf("--json must emit the raw response verbatim: %q", stdout)
	}
}

// ── resource operate verbs (pause / resume / rotate / backup / backups) ─────

func TestOperate_ResourcePauseResume(t *testing.T) {
	var gotMethod, gotPath string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		status := "paused"
		if strings.HasSuffix(r.URL.Path, resourceResumeSuffix) {
			status = "active"
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"token":"tok-1","status":%q,"message":"done"}`, status)
	})

	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "pause", "tok-1")
		if err != nil {
			t.Fatalf("resource pause: %v", err)
		}
	})
	if gotMethod != http.MethodPost || gotPath != "/api/v1/resources/tok-1/pause" {
		t.Errorf("must POST /api/v1/resources/:token/pause, got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(stdout, "status  paused") || !strings.Contains(stdout, "done") {
		t.Errorf("pause output wrong: %q", stdout)
	}

	stdout, _ = captureStdout(t, func() {
		_, _, err := run("resource", "resume", "tok-1")
		if err != nil {
			t.Fatalf("resource resume: %v", err)
		}
	})
	if gotPath != "/api/v1/resources/tok-1/resume" {
		t.Errorf("must POST …/resume, got %s", gotPath)
	}
	if !strings.Contains(stdout, "status  active") {
		t.Errorf("resume output wrong: %q", stdout)
	}
}

func TestOperate_ResourceRotateCredentials(t *testing.T) {
	var gotPath string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = fmt.Fprint(w, `{"ok":true,"connection_url":"postgres://u:NEW@host/db"}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "rotate", "tok-1")
		if err != nil {
			t.Fatalf("resource rotate: %v", err)
		}
	})
	if gotPath != "/api/v1/resources/tok-1/rotate-credentials" {
		t.Errorf("must POST …/rotate-credentials, got %s", gotPath)
	}
	if !strings.Contains(stdout, "postgres://u:NEW@host/db") {
		t.Errorf("new connection URL missing: %q", stdout)
	}
	if !strings.Contains(stdout, "old credentials are revoked") {
		t.Errorf("rotation warning missing: %q", stdout)
	}
}

func TestOperate_ResourceBackupCreate(t *testing.T) {
	var gotPath string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = fmt.Fprint(w, `{"ok":true,"backup_id":"b-123","status":"pending","message":"Backup queued."}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "backup", "tok-1")
		if err != nil {
			t.Fatalf("resource backup: %v", err)
		}
	})
	if gotPath != "/api/v1/resources/tok-1/backup" {
		t.Errorf("must POST …/backup, got %s", gotPath)
	}
	for _, want := range []string{"backup_id  b-123", "status  pending", "Backup queued."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("backup output missing %q: %q", want, stdout)
		}
	}
}

func TestOperate_ResourceActionJSON(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"status":"paused"}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "pause", "tok-1", "--json")
		if err != nil {
			t.Fatalf("resource pause --json: %v", err)
		}
	})
	var res resourceActionResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%q", err, stdout)
	}
	if res.Status != "paused" {
		t.Errorf("unexpected JSON payload: %+v", res)
	}
}

func TestOperate_ResourceBackupsList(t *testing.T) {
	var gotMethod, gotPath string
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = fmt.Fprint(w, `{"ok":true,"total":2,"items":[
			{"backup_id":"b-1","status":"completed","backup_kind":"manual","size_bytes":4096,"created_at":"2026-06-10T00:00:00Z","error_summary":null},
			{"backup_id":"b-2","status":"failed","backup_kind":"scheduled","size_bytes":null,"created_at":"2026-06-11T00:00:00Z","error_summary":"pg_dump: boom"}
		]}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "backups", "tok-1")
		if err != nil {
			t.Fatalf("resource backups: %v", err)
		}
	})
	if gotMethod != http.MethodGet || gotPath != "/api/v1/resources/tok-1/backups" {
		t.Errorf("must GET …/backups, got %s %s", gotMethod, gotPath)
	}
	for _, want := range []string{"b-1", "4096", "b-2", "pg_dump: boom", "total 2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("backups table missing %q: %q", want, stdout)
		}
	}
}

func TestOperate_ResourceBackupsList_EmptyAndJSON(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"total":0,"items":[]}`)
	})
	stdout, _ := captureStdout(t, func() {
		_, _, err := run("resource", "backups", "tok-1")
		if err != nil {
			t.Fatalf("resource backups: %v", err)
		}
	})
	if !strings.Contains(stdout, "no backups") {
		t.Errorf("empty-list hint missing: %q", stdout)
	}

	resetOperateFlags()
	stdout, _ = captureStdout(t, func() {
		_, _, err := run("resource", "backups", "tok-1", "--json")
		if err != nil {
			t.Fatalf("resource backups --json: %v", err)
		}
	})
	var res backupListResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%q", err, stdout)
	}
}

func TestOperate_ResourceVerbMissingToken(t *testing.T) {
	operateServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		t.Error("missing token must fail before any API call")
	})
	for _, verb := range []string{"delete", "pause", "resume", "rotate", "backup", "backups"} {
		_, _, err := run("resource", verb)
		if err == nil || !strings.Contains(err.Error(), "token argument is required") {
			t.Errorf("resource %s (no token): expected usage error, got %v", verb, err)
		}
	}
}

func TestOperate_ResourceOperateEmptyToken(t *testing.T) {
	err := runResourceOperate("pause", "   ")
	if err == nil || !strings.Contains(err.Error(), "token is required") {
		t.Errorf("expected token-required error, got %v", err)
	}
}
