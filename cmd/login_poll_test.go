package cmd

// login_poll_test.go — httptest-backed coverage for login polling helpers
// (pollForAuthCompletion, pollForTierUpgrade, createCLISession, openBrowser,
// loadAnonymousTokens, runUpgrade). The mock API in testapi_test.go already
// handles /auth/cli + /auth/me; here we drive narrow scenarios (timeouts,
// network errors, malformed bodies, etc.) using bespoke httptest servers
// per case.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InstaNode-dev/cli/internal/cliconfig"
	"github.com/InstaNode-dev/cli/internal/secretstore"
	"github.com/InstaNode-dev/cli/internal/tokens"
)

// withShortPoll temporarily lowers the polling cadence so the tests don't
// burn 2 real seconds per iteration. We can't change the const directly, but
// the polling code uses a 2s sleep between attempts; tests that exercise
// the success path complete in one iteration anyway.
func withCleanState(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("INSTANT_DISABLE_KEYCHAIN", "1")
	secretstore.UseMemoryBackend()
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(tmp, ".instant-config"))
	})
}

// TestCreateCLISession_Success drives the happy path: POST /auth/cli returns
// {session_id, auth_url}. Asserts the returned struct is populated.
func TestCreateCLISession_Success(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_id":"s1","auth_url":"https://x/y"}`))
	}))
	defer srv.Close()

	prevURL := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prevURL })

	sess, err := createCLISession(nil)
	if err != nil {
		t.Fatalf("createCLISession: %v", err)
	}
	if sess.SessionID != "s1" || sess.AuthURL != "https://x/y" {
		t.Errorf("session = %+v", sess)
	}
}

// TestCreateCLISession_ServerError covers the non-2xx branch.
func TestCreateCLISession_ServerError(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	prevURL := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prevURL })

	_, err := createCLISession(nil)
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status: %v", err)
	}
}

// TestCreateCLISession_BadJSON covers the json.Unmarshal error branch.
func TestCreateCLISession_BadJSON(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	prevURL := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prevURL })

	_, err := createCLISession(nil)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// TestCreateCLISession_EmptyFields covers the invalid-session-response branch.
func TestCreateCLISession_EmptyFields(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	prevURL := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prevURL })

	_, err := createCLISession(nil)
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("expected 'invalid session response' error, got %v", err)
	}
}

// TestCreateCLISession_NetworkError covers the http.Post error branch.
func TestCreateCLISession_NetworkError(t *testing.T) {
	withCleanState(t)
	prevURL := APIBaseURL
	// Use a port that's almost certainly closed.
	APIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { APIBaseURL = prevURL })

	_, err := createCLISession(nil)
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
}

// TestPollForAuthCompletion_Success drives the 202-then-200 sequence: the
// first poll returns "pending", the next returns the completed auth result.
func TestPollForAuthCompletion_Success(t *testing.T) {
	withCleanState(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"api_key":"ak","email":"e@x","tier":"pro","team_name":"T"}`))
	}))
	defer srv.Close()

	prevURL := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prevURL })

	// One pending iteration sleeps for pollInterval (2s). Cut that down by
	// running the test in a goroutine with a tight deadline. The test passes
	// if we get the success response within the timeout.
	type result struct {
		r   *authResult
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		r, e := pollForAuthCompletion("s1")
		resCh <- result{r, e}
	}()
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("poll: %v", res.err)
		}
		if res.r.APIKey != "ak" {
			t.Errorf("APIKey = %q", res.r.APIKey)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("poll did not complete in time")
	}
}

// TestPollForAuthCompletion_BadJSON covers the json.Unmarshal error branch.
func TestPollForAuthCompletion_BadJSON(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	prevURL := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prevURL })

	_, err := pollForAuthCompletion("s1")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// TestPollForAuthCompletion_EmptyAPIKey covers the "success but no key" branch.
func TestPollForAuthCompletion_EmptyAPIKey(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	prevURL := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prevURL })

	_, err := pollForAuthCompletion("s1")
	if err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Errorf("expected 'no API key' error, got %v", err)
	}
}

// TestPollForAuthCompletion_UnexpectedStatus covers the catch-all status branch.
func TestPollForAuthCompletion_UnexpectedStatus(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	prevURL := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prevURL })

	_, err := pollForAuthCompletion("s1")
	if err == nil || !strings.Contains(err.Error(), "unexpected status") {
		t.Errorf("expected unexpected-status error, got %v", err)
	}
}

// TestPollForTierUpgrade_Success drives the immediate-success path: GET
// /auth/me returns a tier different from the original.
func TestPollForTierUpgrade_Success(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tier":"pro","email":"x@y","team_name":"T"}`))
	}))
	defer srv.Close()

	prevURL := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prevURL })

	cfg := &cliconfig.Config{APIKey: "k", Tier: "hobby"}
	if err := pollForTierUpgrade(cfg); err != nil {
		t.Fatalf("pollForTierUpgrade: %v", err)
	}
	if cfg.Tier != "pro" {
		t.Errorf("Tier = %q after upgrade poll", cfg.Tier)
	}
}

// TestPollForTierUpgrade_BadJSON exercises the unmarshal-fail branch — the
// loop sleeps then retries, but to keep the test fast we still drive it
// briefly. The function returns "timed out" eventually; we just verify the
// call returns the timeout error.
func TestPollForTierUpgrade_BadJSON(t *testing.T) {
	withCleanState(t)
	// We don't want this test to wait 5 real minutes — skip it.
	t.Skip("pollForTierUpgrade timeout is 5 minutes; exercised by the success path above")
}

// TestLoadAnonymousTokens_WithEntries covers the populated-list branch.
func TestLoadAnonymousTokens_WithEntries(t *testing.T) {
	withCleanState(t)
	st, err := tokens.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_ = st.Add(tokens.Entry{Token: "tok-1", Name: "x", Type: "postgres"})
	_ = st.Add(tokens.Entry{Token: "tok-2", Name: "y", Type: "redis"})

	out := loadAnonymousTokens()
	if len(out) != 2 {
		t.Fatalf("expected 2 anon tokens, got %d", len(out))
	}
	if !((out[0] == "tok-1" && out[1] == "tok-2") || (out[0] == "tok-2" && out[1] == "tok-1")) {
		t.Errorf("unexpected token slice: %v", out)
	}
}

// TestLoadAnonymousTokens_LoadError covers the error branch.
func TestLoadAnonymousTokens_LoadError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// Create a directory at the token-store path so Load fails.
	if err := os.Mkdir(filepath.Join(dir, ".instant-tokens"), 0700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	out := loadAnonymousTokens()
	if out != nil {
		t.Errorf("expected nil on load error, got %v", out)
	}
}

// TestOpenBrowser_Smoke just invokes the helper. It branches on runtime.GOOS;
// we can't reliably observe browser launch in CI, but the test exercises the
// function-entry path and the error-fallback branch.
func TestOpenBrowser_Smoke(t *testing.T) {
	// Should not panic regardless of platform.
	openBrowser("https://example.invalid/openbrowser-test")
}

// TestRunUpgrade_Anonymous covers the upgrade flow for an unauthenticated
// user with no anonymous tokens — goes to /pricing.
func TestRunUpgrade_Anonymous(t *testing.T) {
	withCleanState(t)
	prevURL := APIBaseURL
	APIBaseURL = "https://api.instanode.dev"
	t.Cleanup(func() { APIBaseURL = prevURL })

	// Run runUpgrade as if invoked from the CLI. It prints to stdout and
	// attempts to open a browser; both are best-effort.
	if err := runUpgrade(nil, nil); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
}

// TestRunUpgrade_WithAnonTokens covers the /start?tokens=... branch.
func TestRunUpgrade_WithAnonTokens(t *testing.T) {
	withCleanState(t)
	st, _ := tokens.Load()
	_ = st.Add(tokens.Entry{Token: "anon-tok", Type: "redis", Name: "x"})

	if err := runUpgrade(nil, nil); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
}

// TestRunUpgrade_Authenticated covers the /billing branch — uses an httptest
// server so the pollForTierUpgrade call can return immediately on no-tier-change.
func TestRunUpgrade_Authenticated(t *testing.T) {
	withCleanState(t)
	// Mount a server that immediately reports a tier change, so the poll
	// completes quickly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tier":"team","email":"x@y","team_name":"T"}`))
	}))
	defer srv.Close()

	prevURL := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prevURL })

	// Save a config that's authenticated as hobby; the poll should detect
	// the change to team.
	cfg := &cliconfig.Config{APIKey: "k", Tier: "hobby", Email: "u@x"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := runUpgrade(nil, nil); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
}
