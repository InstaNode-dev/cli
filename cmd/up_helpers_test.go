package cmd

// up_helpers_test.go — direct tests for small up.go helpers that the
// integration suite hits incidentally but not exhaustively.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("short: %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("long: %q", got)
	}
	if got := truncate("exact", 5); got != "exact" {
		t.Errorf("exact-length: %q", got)
	}
}

func TestApiResourceType(t *testing.T) {
	cases := map[string]string{
		"postgres": "postgres",
		"REDIS":    "redis",
		"  mongo  ": "mongo", // not in the canonical set, but returned lowercased
		"webhook": "webhook",
		"vector":  "vector",
		"unknown": "unknown",
	}
	for in, want := range cases {
		if got := apiResourceType(in); got != want {
			t.Errorf("apiResourceType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebhookReceiveURL(t *testing.T) {
	prev := APIBaseURL
	APIBaseURL = "https://api.example.com/"
	t.Cleanup(func() { APIBaseURL = prev })
	got := webhookReceiveURL("tok-1")
	if got != "https://api.example.com/webhook/receive/tok-1" {
		t.Errorf("got %q", got)
	}
}

func TestHaveAuth_Branches(t *testing.T) {
	// Save and restore the transport AND the client itself so we don't leak
	// nil-Transport state into later tests in this package.
	prev := HTTPClient
	t.Cleanup(func() { HTTPClient = prev })

	// Use a separate client so we don't corrupt the package-global one.
	c := &http.Client{}
	HTTPClient = c

	// Non-authTransport -> false.
	c.Transport = http.DefaultTransport
	if haveAuth() {
		t.Error("DefaultTransport (not authTransport) should be false")
	}

	// authTransport with empty apiKey but ad-hoc token set -> true.
	c.Transport = &authTransport{base: http.DefaultTransport, apiKey: ""}
	adHocToken = "tok"
	t.Cleanup(func() { adHocToken = "" })
	if !haveAuth() {
		t.Error("ad-hoc token should yield true")
	}
	adHocToken = ""

	// INSTANT_TOKEN env -> true.
	t.Setenv("INSTANT_TOKEN", "from-env")
	if !haveAuth() {
		t.Error("INSTANT_TOKEN env should yield true")
	}
}

func TestValidate_Manifest(t *testing.T) {
	cases := []struct {
		r       manifestRsrc
		wantErr bool
	}{
		{manifestRsrc{Type: "postgres", Name: "x"}, false},
		{manifestRsrc{Type: "redis", Name: "x"}, false},
		{manifestRsrc{Type: "kafka", Name: "x"}, true},
		{manifestRsrc{Type: "postgres", Name: ""}, true},
		{manifestRsrc{Type: "postgres", Name: "   "}, true},
	}
	for _, c := range cases {
		err := c.r.validate()
		if (err != nil) != c.wantErr {
			t.Errorf("validate(%+v) err=%v want-err=%v", c.r, err, c.wantErr)
		}
	}
}

func TestReadManifest_AbsentFile(t *testing.T) {
	_, err := readManifest(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !strings.Contains(err.Error(), "reading") {
		t.Errorf("expected reading-error, got %v", err)
	}
}

func TestReadManifest_BadYaml(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.yaml")
	if err := os.WriteFile(path, []byte("{not-yaml: [["), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := readManifest(path)
	if err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Errorf("expected parse error, got %v", err)
	}
}

func TestReadManifest_NoResources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.yaml")
	if err := os.WriteFile(path, []byte("resources: []"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := readManifest(path)
	if err == nil || !strings.Contains(err.Error(), "declares no resources") {
		t.Errorf("expected empty-resources error, got %v", err)
	}
}

func TestReadManifest_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.yaml")
	body := `resources:
  - type: postgres
    name: app-db
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(path)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if len(m.Resources) != 1 {
		t.Errorf("expected 1 resource, got %d", len(m.Resources))
	}
}

func TestFindExisting_Branches(t *testing.T) {
	items := []resourceListItem{
		{ResourceType: "Postgres", Name: " App-DB ", Env: ""},
		{ResourceType: "redis", Name: "cache", Env: "production"},
	}
	// Empty Env on item with default env -> match.
	if e := findExisting(items, manifestRsrc{Type: "postgres", Name: "app-db"}, "production"); e == nil {
		t.Error("empty env should match production")
	}
	// exact env match
	if e := findExisting(items, manifestRsrc{Type: "redis", Name: "cache"}, "production"); e == nil {
		t.Error("exact env mismatch")
	}
	// Type mismatch
	if e := findExisting(items, manifestRsrc{Type: "kafka", Name: "x"}, "production"); e != nil {
		t.Error("type mismatch should be nil")
	}
	// Name mismatch
	if e := findExisting(items, manifestRsrc{Type: "redis", Name: "x"}, "production"); e != nil {
		t.Error("name mismatch should be nil")
	}
	// Env mismatch and no empty-fallback
	if e := findExisting(items, manifestRsrc{Type: "redis", Name: "cache"}, "staging"); e != nil {
		t.Error("env staging vs production should not match")
	}
}

func TestFetchCredentials_BadStatus(t *testing.T) {
	// Drive via withCleanState + httptest in the existing helper file.
	// Here we hit the wire-format error branch with an invalid path.
	prev := APIBaseURL
	APIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { APIBaseURL = prev })

	_, err := fetchCredentials("tok")
	if err == nil {
		t.Fatal("expected network error")
	}
}
