package cmd

// extras_test.go — coverage for runResourceDetail and runResourceDelete.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// osPipe returns os.Pipe so the test file doesn't import os directly twice.
func osPipe() (*os.File, *os.File, error) { return os.Pipe() }

// stdinSwap replaces os.Stdin and returns the previous value.
func stdinSwap(f *os.File) *os.File {
	prev := os.Stdin
	os.Stdin = f
	return prev
}

func TestRunResourceDetail_EmptyToken(t *testing.T) {
	err := runResourceDetail(nil, "")
	if err == nil || !strings.Contains(err.Error(), "token is required") {
		t.Errorf("expected token required, got %v", err)
	}
}

func TestRunResourceDetail_Unauthenticated401(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no auth", http.StatusUnauthorized)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	// Use a fresh HTTPClient without an authTransport.
	prevClient := HTTPClient
	HTTPClient = &http.Client{}
	t.Cleanup(func() { HTTPClient = prevClient })

	err := runResourceDetail(nil, "tok")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "authentication required") {
		t.Errorf("got %v", err)
	}
}

func TestRunResourceDetail_SessionExpired(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no auth", http.StatusUnauthorized)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	// Wire up an authTransport with an apiKey so haveAuth() returns true.
	prevClient := HTTPClient
	HTTPClient = &http.Client{Transport: &authTransport{base: http.DefaultTransport, apiKey: "x"}}
	t.Cleanup(func() { HTTPClient = prevClient })

	err := runResourceDetail(nil, "tok")
	if err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Errorf("expected session expired, got %v", err)
	}
}

func TestRunResourceDetail_NotFound(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	err := runResourceDetail(nil, "tok")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRunResourceDetail_Success_HumanOutput(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"token":"tok","id":"id-1","resource_type":"postgres","name":"app",
			"env":"production","tier":"pro","status":"active",
			"connection_url":"postgres://u:p@x/db",
			"receive_url":"https://hooks.instanode.dev/tok",
			"created_at":"2026-01-01","expires_at":"2026-12-31"
		}`))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	if err := runResourceDetail(nil, "tok"); err != nil {
		t.Fatalf("runResourceDetail: %v", err)
	}
}

func TestRunResourceDetail_Success_JSON(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"resource":{"token":"tok","name":"x","receive_url":"https://h/tok"}}`))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	resourceDetailJSON = true
	t.Cleanup(func() { resourceDetailJSON = false })

	if err := runResourceDetail(nil, "tok"); err != nil {
		t.Fatalf("runResourceDetail JSON: %v", err)
	}
}

// TestRunResourceDetail_ItemEnvelopeRenders is the BUG 1 regression: prod
// GET /api/v1/resources/:token wraps the object under "item"
// ({"item":{...},"ok":true}). Before the fix the detail decoder only knew
// the bare-object and "resource"-envelope shapes, so it rendered an all-empty
// object with exit 0 (silent). This asserts the "item" key is unwrapped and
// the fields actually render to stdout — mirroring how discover.go's list
// path keys off "items".
func TestRunResourceDetail_ItemEnvelopeRenders(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"item":{
			"token":"tok-item","id":"id-item","resource_type":"postgres","name":"app-item",
			"env":"production","tier":"pro","status":"active",
			"connection_url":"postgres://u:p@x/db"
		}}`))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	// runResourceDetail short-circuits with errAuthRequired unless haveAuth()
	// is true — wire an authTransport so the request actually reaches the mock.
	prevClient := HTTPClient
	HTTPClient = &http.Client{Transport: &authTransport{base: http.DefaultTransport, apiKey: "x"}}
	t.Cleanup(func() { HTTPClient = prevClient })

	stdout, _ := captureStdout(t, func() {
		if err := runResourceDetail(nil, "tok-item"); err != nil {
			t.Fatalf("runResourceDetail (item envelope): %v", err)
		}
	})

	// The fields nested under "item" must render — an all-empty object is the
	// pre-fix bug.
	for _, want := range []string{
		"tok-item",            // TOKEN
		"id-item",             // ID
		"postgres",            // TYPE
		"app-item",            // NAME
		"production",          // ENV
		"pro",                 // TIER
		"active",              // STATUS
		"postgres://u:p@x/db", // URL
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("item-envelope detail must render %q; stdout=%q", want, stdout)
		}
	}
}

func TestRunResourceDelete_EmptyToken(t *testing.T) {
	err := runResourceDelete(nil, "")
	if err == nil || !strings.Contains(err.Error(), "token is required") {
		t.Errorf("got %v", err)
	}
}

func TestRunResourceDelete_NoYesAndNoTTY(t *testing.T) {
	// Drive the interactive-prompt path: replace stdin with a pipe that
	// returns "n" so the prompt aborts.
	resourceDeleteYes = false
	t.Cleanup(func() { resourceDeleteYes = false })

	r, w, err := osPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prevStdin := stdinSwap(r)
	t.Cleanup(func() { stdinSwap(prevStdin); _ = w.Close() })
	go func() {
		_, _ = w.Write([]byte("n\n"))
		_ = w.Close()
	}()

	err = runResourceDelete(nil, "tok")
	// The path either errors with "aborted" (if pipe is treated as TTY)
	// or with "--yes" (if pipe is treated as non-TTY). Either is acceptable.
	if err == nil {
		t.Errorf("expected error from prompt, got nil")
	} else if !strings.Contains(err.Error(), "aborted") && !strings.Contains(err.Error(), "--yes") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunResourceDelete_Success(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"deleted":"tok"}`))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	resourceDeleteYes = true
	t.Cleanup(func() { resourceDeleteYes = false })

	if err := runResourceDelete(&cobra.Command{}, "tok"); err != nil {
		t.Fatalf("runResourceDelete: %v", err)
	}
}

func TestRunResourceDelete_404(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	resourceDeleteYes = true
	t.Cleanup(func() { resourceDeleteYes = false })

	err := runResourceDelete(nil, "tok")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("got %v", err)
	}
}

func TestRunResourceDelete_Unauthorized_NoAuth(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	// Anonymous client.
	prevClient := HTTPClient
	HTTPClient = &http.Client{}
	t.Cleanup(func() { HTTPClient = prevClient })

	resourceDeleteYes = true
	t.Cleanup(func() { resourceDeleteYes = false })

	err := runResourceDelete(nil, "tok")
	if err == nil || !strings.Contains(err.Error(), "authentication required") {
		t.Errorf("got %v", err)
	}
}

func TestRunResourceDelete_Unauthorized_WithAuth(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	prevClient := HTTPClient
	HTTPClient = &http.Client{Transport: &authTransport{base: http.DefaultTransport, apiKey: "x"}}
	t.Cleanup(func() { HTTPClient = prevClient })

	resourceDeleteYes = true
	t.Cleanup(func() { resourceDeleteYes = false })

	err := runResourceDelete(nil, "tok")
	if err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Errorf("got %v", err)
	}
}

func TestRunResourceDelete_OtherError(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	resourceDeleteYes = true
	t.Cleanup(func() { resourceDeleteYes = false })

	err := runResourceDelete(nil, "tok")
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestRunResourceDelete_JSONMode(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	resourceDeleteYes = true
	resourceDetailJSON = true
	t.Cleanup(func() {
		resourceDeleteYes = false
		resourceDetailJSON = false
	})

	if err := runResourceDelete(nil, "tok-json"); err != nil {
		t.Fatalf("runResourceDelete JSON: %v", err)
	}
}
