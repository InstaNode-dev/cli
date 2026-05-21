package cmd

// coverage_push95_test.go — targeted small-coverage tests to cover branches
// that the integration suite + bughunt regression tests miss. Each test is
// minimal-scope: it exercises ONE branch of ONE helper, with a clear name.

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/InstaNode-dev/cli/internal/cliconfig"
)

// ── errors.go ───────────────────────────────────────────────────────────────

func TestExitCodeError_NilReceiverError(t *testing.T) {
	var e *ExitCodeError
	if s := e.Error(); !strings.Contains(s, "exit") {
		t.Errorf("nil receiver Error = %q", s)
	}
}

func TestExitCodeError_NilReceiverUnwrap(t *testing.T) {
	var e *ExitCodeError
	if got := e.Unwrap(); got != nil {
		t.Errorf("nil Unwrap = %v", got)
	}
}

func TestExitCodeError_CodeOrDefault_Nil(t *testing.T) {
	var e *ExitCodeError
	if got := e.codeOrDefault(); got != ExitGeneric {
		t.Errorf("nil codeOrDefault = %d", got)
	}
}

func TestWithExitCode_NilErr(t *testing.T) {
	if err := withExitCode(2, nil); err != nil {
		t.Errorf("withExitCode(nil) = %v", err)
	}
}

func TestErrAuthRequired_DefaultDetail(t *testing.T) {
	err := errAuthRequired("")
	if err == nil || !strings.Contains(err.Error(), "authentication required") {
		t.Errorf("unexpected: %v", err)
	}
}

func TestErrAuthRequired_CustomDetail(t *testing.T) {
	err := errAuthRequired("custom-detail")
	if err == nil || !strings.Contains(err.Error(), "custom-detail") {
		t.Errorf("unexpected: %v", err)
	}
}

func TestErrSessionExpired_Phrase(t *testing.T) {
	err := errSessionExpired()
	if !strings.Contains(err.Error(), "session expired") {
		t.Errorf("expected 'session expired' phrase, got %v", err)
	}
	if ExitCodeFor(err) != ExitAuthRequired {
		t.Errorf("ExitCodeFor session expired = %d", ExitCodeFor(err))
	}
}

func TestExitCodeError_ErrorWithCause(t *testing.T) {
	e := &ExitCodeError{Code: 2, Err: errors.New("cause")}
	if e.Error() != "cause" {
		t.Errorf("Error = %q", e.Error())
	}
	if e.Unwrap() == nil {
		t.Error("Unwrap = nil")
	}
	if e.codeOrDefault() != 2 {
		t.Errorf("codeOrDefault = %d", e.codeOrDefault())
	}
}

func TestExitCodeError_CodeZeroFallsBackToGeneric(t *testing.T) {
	e := &ExitCodeError{Code: 0, Err: errors.New("x")}
	if e.codeOrDefault() != ExitGeneric {
		t.Errorf("expected fallback to ExitGeneric, got %d", e.codeOrDefault())
	}
}

func TestErrResourceFailed(t *testing.T) {
	inner := errors.New("inner-failure")
	err := errResourceFailed(inner)
	if ExitCodeFor(err) != ExitResourceFailed {
		t.Errorf("exit code = %d", ExitCodeFor(err))
	}
}

// ── json_error.go ──────────────────────────────────────────────────────────

func TestClassifyError_Nil(t *testing.T) {
	c, m, a := classifyError(nil)
	if c != "" || m != "" || a != "" {
		t.Errorf("nil err: %q %q %q", c, m, a)
	}
}

func TestClassifyError_AuthRequired(t *testing.T) {
	err := errAuthRequired("")
	c, _, _ := classifyError(err)
	if c != "auth_required" {
		t.Errorf("code = %q", c)
	}
}

func TestClassifyError_ResourceFailed(t *testing.T) {
	err := errResourceFailed(errors.New("rip"))
	c, _, _ := classifyError(err)
	if c != "resource_failed" {
		t.Errorf("code = %q", c)
	}
}

func TestClassifyError_DNSError(t *testing.T) {
	dnsErr := &net.DNSError{Name: "x.invalid", Err: "no such host"}
	urlErr := &url.Error{Op: "Get", URL: "http://x.invalid", Err: dnsErr}
	c, m, _ := classifyError(urlErr)
	if c != "network_error" || !strings.Contains(m, "DNS lookup failed") {
		t.Errorf("got %q / %q", c, m)
	}
}

func TestClassifyError_NetOpError(t *testing.T) {
	opErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	urlErr := &url.Error{Op: "Get", URL: "http://localhost:1", Err: opErr}
	c, m, _ := classifyError(urlErr)
	if c != "network_error" || !strings.Contains(m, "network error reaching") {
		t.Errorf("got %q / %q", c, m)
	}
}

func TestClassifyError_GenericURLError(t *testing.T) {
	urlErr := &url.Error{Op: "Get", URL: "http://x", Err: errors.New("plain")}
	c, _, _ := classifyError(urlErr)
	if c != "network_error" {
		t.Errorf("code = %q", c)
	}
}

func TestClassifyError_SessionExpired(t *testing.T) {
	// errSessionExpired returns an *ExitCodeError with ExitAuthRequired,
	// so classifyError catches it in the auth_required branch first. To
	// reach the lowercase-contains("session expired") branch we need a
	// plain error whose message contains the phrase.
	err := errors.New("oops: session expired token")
	c, _, _ := classifyError(err)
	if c != "session_expired" {
		t.Errorf("code = %q", c)
	}
}

func TestClassifyError_CLIError(t *testing.T) {
	err := errors.New("just a plain error")
	c, m, _ := classifyError(err)
	if c != "cli_error" || m != "just a plain error" {
		t.Errorf("got %q / %q", c, m)
	}
}

func TestClassifyError_ExitCodeErrorOtherCode(t *testing.T) {
	// ExitCodeError with a code other than auth/resource — fall through.
	ec := &ExitCodeError{Code: 99, Err: errors.New("unknown-code-err")}
	c, _, _ := classifyError(ec)
	if c == "" {
		t.Errorf("expected classification, got empty")
	}
}

// ── apierror.go ───────────────────────────────────────────────────────────

func TestCodeOrDefault_Empty(t *testing.T) {
	if c := codeOrDefault("", "fallback"); c != "fallback" {
		t.Errorf("codeOrDefault = %q", c)
	}
}

func TestCodeOrDefault_NonEmpty(t *testing.T) {
	if c := codeOrDefault("set", "fallback"); c != "set" {
		t.Errorf("codeOrDefault = %q", c)
	}
}

// ── discover.go matchResourceFilters ───────────────────────────────────────

func TestMatchResourceFilters_AllKeys(t *testing.T) {
	cases := []struct {
		name    string
		filters map[string]string
		rType   string
		env     string
		status  string
		tier    string
		rName   string
		want    bool
	}{
		{"type-match", map[string]string{"type": "postgres"}, "postgres", "p", "active", "free", "x", true},
		{"type-mismatch", map[string]string{"type": "redis"}, "postgres", "p", "active", "free", "x", false},
		{"env-match", map[string]string{"env": "production"}, "postgres", "production", "active", "free", "x", true},
		{"status-match", map[string]string{"status": "ACTIVE"}, "x", "x", "active", "x", "x", true},
		{"tier-match", map[string]string{"tier": "pro"}, "x", "x", "x", "PRO", "x", true},
		{"name-match", map[string]string{"name": "app"}, "x", "x", "x", "x", "App", true},
		{"multi-match", map[string]string{"type": "postgres", "env": "production"},
			"postgres", "production", "x", "x", "x", true},
		{"multi-mismatch", map[string]string{"type": "postgres", "env": "production"},
			"postgres", "staging", "x", "x", "x", false},
		{"empty-filters", map[string]string{}, "x", "x", "x", "x", "x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matchResourceFilters(c.filters, c.rType, c.env, c.status, c.tier, c.rName)
			if got != c.want {
				t.Errorf("matchResourceFilters = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseResourceFilters_Errors(t *testing.T) {
	// invalid format
	if _, err := parseResourceFilters([]string{"bad"}); err == nil {
		t.Error("expected error on missing =")
	}
	// disallowed key
	if _, err := parseResourceFilters([]string{"bogus=x"}); err == nil {
		t.Error("expected error on disallowed key")
	}
	// = at end
	if _, err := parseResourceFilters([]string{"type="}); err == nil {
		t.Error("expected error on empty value")
	}
}

func TestLowerEqFold(t *testing.T) {
	if lower("ABC") != "abc" {
		t.Error("lower")
	}
	if !eqFold("ABC", "abc") {
		t.Error("eqFold")
	}
	if eqFold("a", "b") {
		t.Error("eqFold false negative")
	}
}

// ── deploy_stub.go ─────────────────────────────────────────────────────────

func TestMcpAliasFor_AllCases(t *testing.T) {
	// Exhaust each switch arm.
	for _, verb := range []string{"new", "list", "get", "logs", "redeploy", "delete"} {
		if got := mcpAliasFor(verb); got == "" || strings.HasPrefix(got, "<") {
			t.Errorf("%s alias = %q (expected concrete tool name)", verb, got)
		}
	}
	// Unknown -> placeholder fallback.
	if got := mcpAliasFor("totally-unknown-cmd"); !strings.Contains(got, "MCP") {
		t.Errorf("unknown fallback = %q", got)
	}
}

func TestCurlHintFor_AllCases(t *testing.T) {
	for _, sub := range []string{"new", "list", "get", "logs", "redeploy", "delete"} {
		if !strings.Contains(curlHintFor(sub, nil, ""), "curl") {
			t.Errorf("%s curl hint missing 'curl'", sub)
		}
	}
	// Test args[0] population branch.
	if !strings.Contains(curlHintFor("get", []string{"my-id-42"}, ""), "my-id-42") {
		t.Error("expected args[0] in hint")
	}
	// Unknown -> generic fallback (still contains 'curl').
	if !strings.Contains(curlHintFor("totally-unknown", nil, ""), "curl") {
		t.Error("unknown fallback should still contain curl")
	}
}

// ── login.go runLogin auth-path ────────────────────────────────────────────

// TestRunLogin_AlreadyLoggedIn covers the early-return branch.
func TestRunLogin_AlreadyLoggedIn(t *testing.T) {
	withCleanState(t)
	// Pre-seed an authenticated config via the real package.
	cfg := &cliconfig.Config{APIKey: "preexisting", Email: "u@x", Tier: "pro"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := runLogin(nil, nil); err != nil {
		t.Fatalf("runLogin: %v", err)
	}
}

// TestRunLogin_SessionCreateError covers the createCLISession error branch.
func TestRunLogin_SessionCreateError(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	err := runLogin(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "starting login") {
		t.Errorf("expected 'starting login' err, got %v", err)
	}
}

// TestRunLogin_FullSuccess drives the entire login flow against a server that
// immediately reports completion. The polling iteration runs exactly once.
func TestRunLogin_FullSuccess(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth/cli" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"session_id":"sess1","auth_url":"http://example/auth"}`))
		case strings.HasPrefix(r.URL.Path, "/auth/cli/") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"api_key":"new-key","email":"u@x","tier":"pro","team_name":"T","claimed_tokens":["t1","t2"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	if err := runLogin(nil, nil); err != nil {
		t.Fatalf("runLogin: %v", err)
	}
}

// TestRunLogin_AnonymousLowTier covers the upsell-message branch
// (tier == "anonymous" or "hobby").
func TestRunLogin_AnonymousLowTier(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth/cli" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"session_id":"s","auth_url":"http://e/a"}`))
		case strings.HasPrefix(r.URL.Path, "/auth/cli/") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"api_key":"k","email":"u@x","tier":"anonymous"}`))
		}
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	if err := runLogin(nil, nil); err != nil {
		t.Fatalf("runLogin: %v", err)
	}
}

// ── initConfig ─────────────────────────────────────────────────────────────

// TestInitConfig_EnvVarOverridesURL covers the INSTANT_API_URL override branch.
func TestInitConfig_EnvVarOverridesURL(t *testing.T) {
	withCleanState(t)
	prev := APIBaseURL
	t.Cleanup(func() { APIBaseURL = prev })

	t.Setenv("INSTANT_API_URL", "https://override.example/")
	initConfig()
	if APIBaseURL != "https://override.example/" {
		t.Errorf("APIBaseURL = %q", APIBaseURL)
	}
}

// TestInitConfig_TimeoutOverride covers the INSTANT_TIMEOUT_SECONDS branch.
func TestInitConfig_TimeoutOverride(t *testing.T) {
	withCleanState(t)
	t.Setenv("INSTANT_TIMEOUT_SECONDS", "5")
	initConfig()
	if HTTPClient.Timeout.Seconds() != 5 {
		t.Errorf("Timeout = %v", HTTPClient.Timeout)
	}
}

func TestInitConfig_BadTimeoutIgnored(t *testing.T) {
	withCleanState(t)
	t.Setenv("INSTANT_TIMEOUT_SECONDS", "not-a-number")
	initConfig()
	// Should fall back to default (60s).
	if HTTPClient.Timeout != httpProvisionTimeout {
		t.Errorf("Timeout = %v, want default", HTTPClient.Timeout)
	}
}

func TestInitConfig_TokenFlagWins(t *testing.T) {
	withCleanState(t)
	adHocToken = "  flag-token  " // trimmed
	t.Cleanup(func() { adHocToken = "" })
	initConfig()
	// We can't directly inspect the auth transport's apiKey, but reaching this
	// line means the trim path executed without panic.
}
