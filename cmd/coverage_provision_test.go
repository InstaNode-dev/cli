package cmd

// coverage_provision_test.go — directly exercises provisionResource's error
// branches that the high-level integration tests don't always reach.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProvisionResource_NetworkError(t *testing.T) {
	prev := APIBaseURL
	APIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { APIBaseURL = prev })

	_, err := provisionResource("/db/new", "x")
	if err == nil {
		t.Fatal("expected network error")
	}
}

func TestProvisionResource_SessionExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	prevC := HTTPClient
	HTTPClient = &http.Client{Transport: &authTransport{base: http.DefaultTransport, apiKey: "k"}}
	t.Cleanup(func() { HTTPClient = prevC })

	_, err := provisionResource("/db/new", "x")
	if err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Errorf("got %v", err)
	}
}

func TestProvisionResource_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"quota"}`, http.StatusPaymentRequired)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	_, err := provisionResource("/db/new", "x")
	if err == nil {
		t.Fatal("expected api error")
	}
}

func TestProvisionResource_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	_, err := provisionResource("/db/new", "x")
	if err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Errorf("got %v", err)
	}
}

func TestProvisionResource_UnexpectedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	_, err := provisionResource("/db/new", "x")
	if err == nil || !strings.Contains(err.Error(), "unexpected response") {
		t.Errorf("got %v", err)
	}
}
