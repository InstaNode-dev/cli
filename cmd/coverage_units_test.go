package cmd

// coverage_units_test.go — table-driven unit tests for the small pure
// helpers whose error/edge branches the integration suite doesn't exercise.
// These push the package over the ≥95% patch-coverage mandate by hitting the
// nil-receiver, empty-input, and default-arm branches directly rather than
// through a full command invocation.

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
)

// ── errors.go ───────────────────────────────────────────────────────────────

func TestExitCodeError_NilAndZeroPaths(t *testing.T) {
	var nilEC *ExitCodeError

	// Nil receiver Error() falls through to the "exit N" default path.
	if got := nilEC.Error(); got != fmt.Sprintf("exit %d", ExitGeneric) {
		t.Errorf("nil.Error() = %q", got)
	}
	// Nil receiver Unwrap() is nil.
	if nilEC.Unwrap() != nil {
		t.Error("nil.Unwrap() should be nil")
	}
	// Nil receiver codeOrDefault() defaults to ExitGeneric.
	if got := nilEC.codeOrDefault(); got != ExitGeneric {
		t.Errorf("nil.codeOrDefault() = %d", got)
	}

	// Non-nil but Err==nil also falls to the "exit N" default with its code.
	ecNoErr := &ExitCodeError{Code: ExitResourceFailed}
	if got := ecNoErr.Error(); got != fmt.Sprintf("exit %d", ExitResourceFailed) {
		t.Errorf("Error() with nil inner = %q", got)
	}
	if ecNoErr.Unwrap() != nil {
		t.Error("Unwrap() with nil inner should be nil")
	}

	// Code==0 defaults to ExitGeneric.
	if got := (&ExitCodeError{}).codeOrDefault(); got != ExitGeneric {
		t.Errorf("zero-code codeOrDefault() = %d", got)
	}
	// Explicit code is preserved.
	if got := (&ExitCodeError{Code: ExitAuthRequired}).codeOrDefault(); got != ExitAuthRequired {
		t.Errorf("codeOrDefault() = %d", got)
	}
}

func TestWithExitCode_NilPassThrough(t *testing.T) {
	if withExitCode(ExitResourceFailed, nil) != nil {
		t.Error("withExitCode(_, nil) should return nil")
	}
	err := withExitCode(ExitResourceFailed, errors.New("boom"))
	if ExitCodeFor(err) != ExitResourceFailed {
		t.Errorf("ExitCodeFor = %d", ExitCodeFor(err))
	}
}

func TestErrHelpers(t *testing.T) {
	// errResourceFailed wraps with code 2.
	if ExitCodeFor(errResourceFailed(errors.New("x"))) != ExitResourceFailed {
		t.Error("errResourceFailed code mismatch")
	}
	// errAuthRequired with empty detail uses the uniform default string.
	def := errAuthRequired("")
	if !strings.Contains(def.Error(), "authentication required") {
		t.Errorf("default auth msg = %q", def.Error())
	}
	if ExitCodeFor(def) != ExitAuthRequired {
		t.Error("errAuthRequired code mismatch")
	}
	// errAuthRequired with a custom detail preserves it.
	custom := errAuthRequired("custom detail")
	if !strings.Contains(custom.Error(), "custom detail") {
		t.Errorf("custom auth msg = %q", custom.Error())
	}
	// errSessionExpired keeps the literal phrase the suite greps for.
	if !strings.Contains(errSessionExpired().Error(), "session expired") {
		t.Error("errSessionExpired must contain 'session expired'")
	}
}

func TestExitCodeFor_Defaults(t *testing.T) {
	if ExitCodeFor(nil) != ExitOK {
		t.Error("nil should be ExitOK")
	}
	// A plain (non-ExitCodeError) error defaults to ExitGeneric.
	if ExitCodeFor(errors.New("plain")) != ExitGeneric {
		t.Error("plain error should be ExitGeneric")
	}
}

// ── apierror.go ──────────────────────────────────────────────────────────────

func TestCodeOrDefault(t *testing.T) {
	if got := codeOrDefault("", "fallback"); got != "fallback" {
		t.Errorf("empty -> %q", got)
	}
	if got := codeOrDefault("present", "fallback"); got != "present" {
		t.Errorf("present -> %q", got)
	}
}

func TestEnvelopeCode(t *testing.T) {
	if got := (&apiErrorEnvelope{ErrorCode: "ec", Error: "e"}).code(); got != "ec" {
		t.Errorf("error_code preferred: %q", got)
	}
	if got := (&apiErrorEnvelope{Error: "e"}).code(); got != "e" {
		t.Errorf("error fallback: %q", got)
	}
}

func TestParseAPIError_AllBranches(t *testing.T) {
	// Empty body.
	if e := parseAPIError(500, []byte("  ")); !strings.Contains(e.Error(), "no body") {
		t.Errorf("empty body: %q", e.Error())
	}
	// Non-JSON body falls back to truncated raw.
	if e := parseAPIError(503, []byte("<html>down</html>")); !strings.Contains(e.Error(), "down") {
		t.Errorf("non-json: %q", e.Error())
	}
	// JSON envelope but all interesting fields empty -> raw-body fallback.
	if e := parseAPIError(400, []byte(`{"ok":false}`)); !strings.Contains(e.Error(), `{"ok":false}`) {
		t.Errorf("empty envelope: %q", e.Error())
	}
	// 402 tier wall with code + agent_action + upgrade_url + request_id.
	e402 := parseAPIError(402, []byte(`{"error":"quota_exceeded","message":"hit limit","agent_action":"upgrade now","upgrade_url":"https://x/billing","request_id":"req_1"}`))
	for _, want := range []string{"402", "quota_exceeded", "hit limit", "→ upgrade now", "upgrade: https://x/billing", "request_id=req_1"} {
		if !strings.Contains(e402.Error(), want) {
			t.Errorf("402 missing %q in %q", want, e402.Error())
		}
	}
	// 402 with no code uses the default label.
	if e := parseAPIError(402, []byte(`{"message":"m"}`)); !strings.Contains(e.Error(), "tier limit reached") {
		t.Errorf("402 default: %q", e.Error())
	}
	// 429 with retry-after.
	if e := parseAPIError(429, []byte(`{"error":"rate","retry_after_seconds":30}`)); !strings.Contains(e.Error(), "retry in 30s") {
		t.Errorf("429 retry: %q", e.Error())
	}
	// 429 without retry-after.
	if e := parseAPIError(429, []byte(`{"error":"rate"}`)); !strings.Contains(e.Error(), "429 rate limited") {
		t.Errorf("429 no-retry: %q", e.Error())
	}
	// 5xx default label when code empty.
	if e := parseAPIError(502, []byte(`{"message":"m"}`)); !strings.Contains(e.Error(), "server error, retry later") {
		t.Errorf("5xx default: %q", e.Error())
	}
	// 4xx default label + legacy "upgrade" field.
	e4 := parseAPIError(403, []byte(`{"message":"m","upgrade":"https://legacy"}`))
	if !strings.Contains(e4.Error(), "request rejected") || !strings.Contains(e4.Error(), "upgrade: https://legacy") {
		t.Errorf("4xx default/legacy upgrade: %q", e4.Error())
	}
	// agent_action equal to message is not duplicated.
	dup := parseAPIError(400, []byte(`{"error":"c","message":"same","agent_action":"same"}`))
	if strings.Contains(dup.Error(), "→ same") {
		t.Errorf("agent_action should not duplicate message: %q", dup.Error())
	}
}

// ── json_error.go : classifyError ────────────────────────────────────────────

func TestClassifyError_AllBranches(t *testing.T) {
	if c, _, _ := classifyError(nil); c != "" {
		t.Errorf("nil -> %q", c)
	}
	// ExitCodeError auth.
	if c, _, _ := classifyError(errAuthRequired("")); c != "auth_required" {
		t.Errorf("auth -> %q", c)
	}
	// ExitCodeError resource_failed.
	if c, _, _ := classifyError(errResourceFailed(errors.New("x"))); c != "resource_failed" {
		t.Errorf("resource -> %q", c)
	}
	// errSessionExpired is an *ExitCodeError with Code==ExitAuthRequired, so it
	// classifies as auth_required (the switch matches the code before the
	// message phrase). The dedicated "session_expired" branch is only reached
	// for a *plain* error whose message contains the phrase.
	if c, _, _ := classifyError(errSessionExpired()); c != "auth_required" {
		t.Errorf("session-as-exitcode -> %q", c)
	}
	if c, _, _ := classifyError(errors.New("the session expired, sorry")); c != "session_expired" {
		t.Errorf("session phrase -> %q", c)
	}
	// plain error -> cli_error.
	if c, _, _ := classifyError(errors.New("whatever")); c != "cli_error" {
		t.Errorf("plain -> %q", c)
	}
	// DNS error wrapped in url.Error.
	dns := &url.Error{Op: "Get", URL: "http://x", Err: &net.DNSError{Name: "x", Err: "no such host"}}
	if c, m, _ := classifyError(dns); c != "network_error" || !strings.Contains(m, "DNS lookup failed") {
		t.Errorf("dns -> %q / %q", c, m)
	}
	// net.OpError wrapped in url.Error.
	op := &url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}
	if c, m, _ := classifyError(op); c != "network_error" || !strings.Contains(m, "network error reaching") {
		t.Errorf("op -> %q / %q", c, m)
	}
	// generic url.Error (neither DNS nor OpError).
	generic := &url.Error{Op: "Get", URL: "http://x", Err: errors.New("some tls thing")}
	if c, _, _ := classifyError(generic); c != "network_error" {
		t.Errorf("generic url -> %q", c)
	}
}

// ── discover.go : filter helpers ─────────────────────────────────────────────

func TestParseResourceFilters(t *testing.T) {
	// Empty input -> nil, nil.
	if m, err := parseResourceFilters(nil); m != nil || err != nil {
		t.Errorf("empty -> %v / %v", m, err)
	}
	// Valid pairs, key lowercased.
	m, err := parseResourceFilters([]string{"Type=postgres", "env=prod"})
	if err != nil || m["type"] != "postgres" || m["env"] != "prod" {
		t.Errorf("valid -> %v / %v", m, err)
	}
	// Malformed (no '=').
	if _, err := parseResourceFilters([]string{"bogus"}); err == nil {
		t.Error("missing '=' should error")
	}
	// Leading '=' (empty key).
	if _, err := parseResourceFilters([]string{"=v"}); err == nil {
		t.Error("empty key should error")
	}
	// Trailing '=' (empty value).
	if _, err := parseResourceFilters([]string{"type="}); err == nil {
		t.Error("empty value should error")
	}
	// Unknown key.
	if _, err := parseResourceFilters([]string{"color=red"}); err == nil {
		t.Error("unknown key should error")
	}
}

func TestMatchResourceFilters(t *testing.T) {
	// No filters -> always matches.
	if !matchResourceFilters(nil, "postgres", "prod", "active", "pro", "db1") {
		t.Error("nil filters should match")
	}
	// All match (case-insensitive on value).
	if !matchResourceFilters(map[string]string{"type": "POSTGRES", "name": "DB1"},
		"postgres", "prod", "active", "pro", "db1") {
		t.Error("case-insensitive match expected")
	}
	// One mismatch fails the whole row.
	if matchResourceFilters(map[string]string{"env": "staging"},
		"postgres", "prod", "active", "pro", "db1") {
		t.Error("env mismatch should fail")
	}
	// Each key arm.
	for _, k := range []string{"status", "tier", "type", "env", "name"} {
		if !matchResourceFilters(map[string]string{k: vals(k)},
			"postgres", "prod", "active", "pro", "db1") {
			t.Errorf("arm %q should match", k)
		}
	}
}

func vals(k string) string {
	switch k {
	case "type":
		return "postgres"
	case "env":
		return "prod"
	case "status":
		return "active"
	case "tier":
		return "pro"
	default:
		return "db1"
	}
}

func TestLowerAndEqFold(t *testing.T) {
	if lower("AbC123_-") != "abc123_-" {
		t.Errorf("lower = %q", lower("AbC123_-"))
	}
	if !eqFold("Foo", "fOO") {
		t.Error("eqFold should be case-insensitive")
	}
	if eqFold("a", "b") {
		t.Error("eqFold a/b should differ")
	}
}

// ── deploy_stub.go : default arms ────────────────────────────────────────────

func TestMcpAliasFor(t *testing.T) {
	cases := map[string]string{
		"new":      "create_deploy",
		"list":     "list_deployments",
		"get":      "get_deployment",
		"logs":     "get_deployment",
		"redeploy": "redeploy",
		"delete":   "delete_deployment",
		"unknown":  "<deploy MCP tools>",
	}
	for verb, want := range cases {
		if got := mcpAliasFor(verb); got != want {
			t.Errorf("mcpAliasFor(%q) = %q, want %q", verb, got, want)
		}
	}
}

func TestCurlHintFor(t *testing.T) {
	// Each known verb renders a curl line; the default arm covers unknown.
	for _, verb := range []string{"new", "list", "get", "logs", "redeploy", "delete", "unknown"} {
		got := curlHintFor(verb, nil, "")
		if !strings.HasPrefix(got, "curl") {
			t.Errorf("curlHintFor(%q) = %q (no curl prefix)", verb, got)
		}
	}
	// An explicit id arg is interpolated for id-bearing verbs.
	if got := curlHintFor("get", []string{"dep_42"}, ""); !strings.Contains(got, "dep_42") {
		t.Errorf("id not interpolated: %q", got)
	}
}
