package cmd

// coverage_login_test.go — drives the login HTTP helpers (createCLISession,
// pollForAuthCompletion) through an httptest server so their success, error,
// and malformed-response branches are covered without real network access.
// All servers respond immediately (HTTP 200), so no test waits on the 2s
// pollInterval — only the happy-path / error branches that return on the
// first iteration are exercised.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateCLISession(t *testing.T) {
	// Success: server returns a valid session.
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/auth/cli" {
				t.Errorf("path = %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"session_id":"sess_1","auth_url":"https://x/login"}`))
		}))
		defer srv.Close()
		withTestAPI(t, srv.URL)

		s, err := createCLISession([]string{"tok1"})
		if err != nil || s.SessionID != "sess_1" || s.AuthURL != "https://x/login" {
			t.Fatalf("createCLISession = %+v / %v", s, err)
		}
	})

	// Non-2xx status surfaces a server-returned error.
	t.Run("error_status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		}))
		defer srv.Close()
		withTestAPI(t, srv.URL)

		if _, err := createCLISession(nil); err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("expected 500 error, got %v", err)
		}
	})

	// Malformed JSON body.
	t.Run("bad_json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		withTestAPI(t, srv.URL)

		if _, err := createCLISession(nil); err == nil || !strings.Contains(err.Error(), "parsing session") {
			t.Fatalf("expected parse error, got %v", err)
		}
	})

	// Valid JSON but missing required fields.
	t.Run("incomplete", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"session_id":""}`))
		}))
		defer srv.Close()
		withTestAPI(t, srv.URL)

		if _, err := createCLISession(nil); err == nil || !strings.Contains(err.Error(), "invalid session") {
			t.Fatalf("expected invalid-session error, got %v", err)
		}
	})
}

func TestPollForAuthCompletion(t *testing.T) {
	// Immediate success on the first poll.
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"api_key":"sk_live","email":"a@b.c","tier":"hobby"}`))
		}))
		defer srv.Close()
		withTestAPI(t, srv.URL)

		res, err := pollForAuthCompletion("sess_1")
		if err != nil || res.APIKey != "sk_live" {
			t.Fatalf("poll = %+v / %v", res, err)
		}
	})

	// 200 but no API key -> error.
	t.Run("no_key", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"api_key":""}`))
		}))
		defer srv.Close()
		withTestAPI(t, srv.URL)

		if _, err := pollForAuthCompletion("s"); err == nil || !strings.Contains(err.Error(), "no API key") {
			t.Fatalf("expected no-key error, got %v", err)
		}
	})

	// 200 but malformed JSON -> parse error.
	t.Run("bad_json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("xx"))
		}))
		defer srv.Close()
		withTestAPI(t, srv.URL)

		if _, err := pollForAuthCompletion("s"); err == nil || !strings.Contains(err.Error(), "parsing auth result") {
			t.Fatalf("expected parse error, got %v", err)
		}
	})

	// Unexpected status (not 200/202) returns immediately with an error.
	t.Run("unexpected_status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("nope"))
		}))
		defer srv.Close()
		withTestAPI(t, srv.URL)

		if _, err := pollForAuthCompletion("s"); err == nil || !strings.Contains(err.Error(), "unexpected status 403") {
			t.Fatalf("expected 403 error, got %v", err)
		}
	})
}

// TestUpHelpers covers the small pure helpers in up.go that the integration
// flow doesn't fully exercise.
func TestUpHelpers(t *testing.T) {
	// truncate: under-limit returns unchanged, over-limit clamps + ellipsis.
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate under = %q", got)
	}
	if got := truncate("abcdefghij", 3); got != "abc…" {
		t.Errorf("truncate over = %q", got)
	}

	// apiResourceType: known types pass through lowercased; unknown also
	// returns lowercased trimmed (single-site mapping seam).
	if got := apiResourceType("  Postgres "); got != "postgres" {
		t.Errorf("known type = %q", got)
	}
	if got := apiResourceType("MONGODB"); got != "mongodb" {
		t.Errorf("mongodb = %q", got)
	}
	if got := apiResourceType("Custom"); got != "custom" {
		t.Errorf("unknown type = %q", got)
	}

	// shortToken + webhookReceiveURL exercise the token-derivation seams.
	if got := shortToken("abcdefghijkl"); got != "abcdefgh" {
		t.Errorf("shortToken = %q", got)
	}
	withTestAPI(t, "https://api.example.com/")
	if got := webhookReceiveURL("tok_9"); got != "https://api.example.com/webhook/receive/tok_9" {
		t.Errorf("webhookReceiveURL = %q", got)
	}
}
