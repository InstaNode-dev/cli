package cmd

// login_timeout_test.go — exercises the timeout + retry branches in
// pollForAuthCompletion and pollForTierUpgrade, made tractable by the
// fact that pollInterval/pollTimeout/tierUpgradeTimeout are vars (not
// consts) so tests can compress them to milliseconds.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InstaNode-dev/cli/internal/cliconfig"
)

// withShortPolls sets the polling vars to test-friendly values.
func withShortPolls(t *testing.T) {
	t.Helper()
	prevI, prevT, prevU := pollInterval, pollTimeout, tierUpgradeTimeout
	pollInterval = 5 * time.Millisecond
	pollTimeout = 80 * time.Millisecond
	tierUpgradeTimeout = 80 * time.Millisecond
	t.Cleanup(func() {
		pollInterval = prevI
		pollTimeout = prevT
		tierUpgradeTimeout = prevU
	})
}

// TestPollForAuthCompletion_TimeoutExpires covers the loop-deadline branch.
// The server returns 202 forever; pollForAuthCompletion eventually returns
// the "timed out" error.
func TestPollForAuthCompletion_Timeout(t *testing.T) {
	withCleanState(t)
	withShortPolls(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	_, err := pollForAuthCompletion("s1")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout, got %v", err)
	}
}

// TestPollForAuthCompletion_NetworkRetry covers the "network error -> retry"
// branch by serving on a port that's reachable then closing the server
// mid-flight. The simpler path: keep the server returning a network-level
// connection refused via a non-routable URL while the loop ticks.
func TestPollForAuthCompletion_NetworkRetryThenTimeout(t *testing.T) {
	withCleanState(t)
	withShortPolls(t)

	prev := APIBaseURL
	APIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { APIBaseURL = prev })

	_, err := pollForAuthCompletion("s1")
	if err == nil {
		t.Fatal("expected timeout after retries")
	}
}

// TestPollForTierUpgrade_Timeout covers the timeout branch.
func TestPollForTierUpgrade_Timeout(t *testing.T) {
	withCleanState(t)
	withShortPolls(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always return the SAME tier so the change-detection branch never fires.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tier":"hobby","email":"u@x","team_name":"T"}`))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	cfg := &cliconfig.Config{APIKey: "k", Tier: "hobby"}
	err := pollForTierUpgrade(cfg)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout, got %v", err)
	}
}

// TestPollForTierUpgrade_NetworkRetry covers the "Do error -> retry" branch.
func TestPollForTierUpgrade_NetworkRetry(t *testing.T) {
	withCleanState(t)
	withShortPolls(t)

	prev := APIBaseURL
	APIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { APIBaseURL = prev })

	cfg := &cliconfig.Config{APIKey: "k", Tier: "hobby"}
	err := pollForTierUpgrade(cfg)
	if err == nil {
		t.Fatal("expected timeout")
	}
}

// TestPollForTierUpgrade_BadJSONRetry covers the unmarshal-failed retry branch.
func TestPollForTierUpgrade_BadJSONRetry(t *testing.T) {
	withCleanState(t)
	withShortPolls(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	cfg := &cliconfig.Config{APIKey: "k", Tier: "hobby"}
	err := pollForTierUpgrade(cfg)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout after bad-json retries, got %v", err)
	}
}

// TestPollForAuthCompletion_PendingThenSuccess pins the multi-iteration path
// where the server returns 202 a few times before the final 200. Already
// covered by the success-on-first-call test, but here we explicitly drive
// the 202 path at least twice to exercise the dots-counter branch.
func TestPollForAuthCompletion_MultiplePendingDots(t *testing.T) {
	withCleanState(t)
	withShortPolls(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 6 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_, _ = w.Write([]byte(`{"api_key":"k","email":"e@x","tier":"hobby"}`))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	// Allow more time for this test.
	pollTimeout = 500 * time.Millisecond
	r, err := pollForAuthCompletion("s1")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if r.APIKey != "k" {
		t.Errorf("APIKey = %q", r.APIKey)
	}
}
