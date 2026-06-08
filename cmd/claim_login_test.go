package cmd

// claim_login_test.go — integration coverage for the CLI "claim" path.
//
// The CLI has NO standalone `claim` command. Anonymous resources are claimed
// as a side effect of the login device-flow: `runLogin` does
//   POST /auth/cli      → {session_id, auth_url}
//   GET  /auth/cli/{id} → 202 (pending) … then 200 {api_key, …, claimed_tokens}
// and on success surfaces the count of newly-claimed anonymous tokens to the
// user ("N anonymous resource token(s) claimed to your account.").
//
// The existing login tests (login_poll_test.go, login_timeout_test.go,
// coverage_login_test.go, coverage_push95_test.go) already exercise the poll
// HELPERS and the runLogin happy/error BRANCHES — but every runLogin test
// asserts only `err == nil` and discards stdout. None of them prove the
// user-visible claim output is correct. Per CLAUDE.md rule 12 ("Shipped ≠
// Verified": the verification surface MUST match the failure surface), a green
// `runLogin` is not proof the user sees their claimed-token count. This file
// closes that gap: it captures stdout and asserts the rendered claim line for
// the multi-token, zero-token, and pending-then-success (multi-poll) cases,
// and verifies credentials are persisted to the local config.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/InstaNode-dev/cli/internal/cliconfig"
)

const (
	// claimLineSuffix is the trailing, count-agnostic portion of the line
	// runLogin prints when one or more anonymous tokens are claimed (login.go
	// ~line 116). Asserting against this fragment keeps the test resilient to
	// count changes while still pinning the user-visible claim message.
	claimLineSuffix = "anonymous resource token(s) claimed to your account."
	// loggedInPrefix is the success banner runLogin always prints.
	loggedInPrefix = "Logged in as"
	// claimTestEmail / claimTestAPIKey are the synthetic identity the stub
	// returns on a completed login.
	claimTestEmail  = "claimer@instanode.dev"
	claimTestAPIKey = "inst_claim_test_key"
	claimTestTier   = "pro"
	claimTestTeam   = "Claim Team"
)

// claimStubBody returns the JSON body the stub /auth/cli/{id} endpoint serves
// on completion, with the given claimed-token list. A nil/empty list models a
// login that claimed nothing (the user had no local anonymous tokens, or they
// were already associated).
func claimStubBody(t *testing.T, claimed []string) string {
	t.Helper()
	tokensJSON := "[]"
	if len(claimed) > 0 {
		quoted := make([]string, len(claimed))
		for i, tok := range claimed {
			quoted[i] = fmt.Sprintf("%q", tok)
		}
		tokensJSON = "[" + strings.Join(quoted, ",") + "]"
	}
	return fmt.Sprintf(
		`{"api_key":%q,"email":%q,"tier":%q,"team_name":%q,"claimed_tokens":%s}`,
		claimTestAPIKey, claimTestEmail, claimTestTier, claimTestTeam, tokensJSON,
	)
}

// claimLoginServer mounts a device-flow stub: POST /auth/cli yields a session,
// and GET /auth/cli/{id} returns 202 `pendingPolls` times before serving the
// completed body (with the supplied claimed tokens). pendingPolls=0 means the
// very first poll succeeds.
func claimLoginServer(t *testing.T, claimed []string, pendingPolls int32) *httptest.Server {
	t.Helper()
	var polls int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth/cli" && r.Method == http.MethodPost:
			writeJSON(w, http.StatusOK, map[string]string{
				"session_id": "sess_claim_test",
				"auth_url":   "https://instanode.dev/cli-auth?s=claim",
			})
		case strings.HasPrefix(r.URL.Path, "/auth/cli/") && r.Method == http.MethodGet:
			if atomic.AddInt32(&polls, 1) <= pendingPolls {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(claimStubBody(t, claimed)))
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestClaimOnLogin_SurfacesClaimedCount is the headline case: a successful
// device-flow login that claims two anonymous tokens MUST report the count to
// the user AND persist the new credentials. This is the assertion the existing
// runLogin tests are missing — they let the print branch run but never read it.
func TestClaimOnLogin_SurfacesClaimedCount(t *testing.T) {
	withCleanState(t)
	srv := claimLoginServer(t, []string{"tok_a", "tok_b"}, 0)
	defer srv.Close()
	withTestAPI(t, srv.URL)

	var runErr error
	stdout, _ := captureStdout(t, func() {
		runErr = runLogin(nil, nil)
	})
	if runErr != nil {
		t.Fatalf("runLogin: %v", runErr)
	}

	// User-visible claim line must report the exact count of claimed tokens.
	wantClaim := fmt.Sprintf("2 %s", claimLineSuffix)
	if !strings.Contains(stdout, wantClaim) {
		t.Errorf("stdout missing claim line %q; got:\n%s", wantClaim, stdout)
	}
	if !strings.Contains(stdout, loggedInPrefix) {
		t.Errorf("stdout missing login banner %q; got:\n%s", loggedInPrefix, stdout)
	}

	// Credentials from the claimed login must be persisted so subsequent
	// authenticated calls (and the claimed resources) are usable.
	cfg, err := cliconfig.Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.APIKey != claimTestAPIKey {
		t.Errorf("APIKey not persisted: got %q want %q", cfg.APIKey, claimTestAPIKey)
	}
	if cfg.Email != claimTestEmail {
		t.Errorf("Email not persisted: got %q want %q", cfg.Email, claimTestEmail)
	}
	if cfg.Tier != claimTestTier {
		t.Errorf("Tier not persisted: got %q want %q", cfg.Tier, claimTestTier)
	}
}

// TestClaimOnLogin_ZeroClaimedTokens pins the empty-claim contract: a login
// that claims nothing (`claimed_tokens: []`) must NOT print a claim line.
// Printing "0 ... claimed" would be a misleading regression — the existing
// suite's anonymous case never asserts the absence of this line.
func TestClaimOnLogin_ZeroClaimedTokens(t *testing.T) {
	withCleanState(t)
	srv := claimLoginServer(t, nil, 0)
	defer srv.Close()
	withTestAPI(t, srv.URL)

	var runErr error
	stdout, _ := captureStdout(t, func() {
		runErr = runLogin(nil, nil)
	})
	if runErr != nil {
		t.Fatalf("runLogin: %v", runErr)
	}

	if strings.Contains(stdout, claimLineSuffix) {
		t.Errorf("expected NO claim line for zero claimed tokens; got:\n%s", stdout)
	}
	// The login itself still succeeds and reports the account.
	if !strings.Contains(stdout, loggedInPrefix) {
		t.Errorf("stdout missing login banner %q; got:\n%s", loggedInPrefix, stdout)
	}
}

// TestClaimOnLogin_PendingThenClaimed is the realistic edge case: the user
// takes a few seconds to finish the browser flow, so the CLI sees several 202
// "pending" polls before the 200 that carries claimed_tokens. The claim count
// must survive the multi-poll path and still surface correctly. Uses the
// var-overridable poll cadence so the 202s don't burn real seconds.
func TestClaimOnLogin_PendingThenClaimed(t *testing.T) {
	withCleanState(t)
	withShortPolls(t)
	srv := claimLoginServer(t, []string{"tok_only"}, 3)
	defer srv.Close()
	withTestAPI(t, srv.URL)

	var runErr error
	stdout, _ := captureStdout(t, func() {
		runErr = runLogin(nil, nil)
	})
	if runErr != nil {
		t.Fatalf("runLogin after pending polls: %v", runErr)
	}

	wantClaim := fmt.Sprintf("1 %s", claimLineSuffix)
	if !strings.Contains(stdout, wantClaim) {
		t.Errorf("stdout missing single-token claim line %q; got:\n%s", wantClaim, stdout)
	}
}
