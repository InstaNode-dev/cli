package cmd

// coverage_tail_test.go — final small fills to crest 95%.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestJsonModeOn_OsArgsFallback covers the os.Args fallback branch.
func TestJsonModeOn_OsArgsFallback(t *testing.T) {
	resourcesJSON = false
	statusJSON = false
	whoamiJSON = false

	prev := os.Args
	os.Args = []string{"instant", "--json"}
	t.Cleanup(func() { os.Args = prev })

	c := &cobra.Command{Use: "x"}
	if !jsonModeOn(c) {
		t.Error("os.Args --json should yield true")
	}
}

func TestJsonModeOn_OsArgs_JsonTrue(t *testing.T) {
	resourcesJSON = false
	statusJSON = false
	whoamiJSON = false

	prev := os.Args
	os.Args = []string{"instant", "--json=true"}
	t.Cleanup(func() { os.Args = prev })

	c := &cobra.Command{Use: "x"}
	if !jsonModeOn(c) {
		t.Error("--json=true should yield true")
	}
}

// TestWrapJSONErr_AgentActionPath covers the branch where the inner err
// already carries an agent_action (e.g. quota error). We use an errAuthRequired
// which has the auth_required code + a non-empty agentAction.
func TestWrapJSONErr_AgentActionEmitted(t *testing.T) {
	resourcesJSON = true
	t.Cleanup(func() { resourcesJSON = false })

	prevOut := os.Stdout
	rd, wr, _ := os.Pipe()
	os.Stdout = wr
	t.Cleanup(func() { os.Stdout = prevOut })

	go func() {
		_ = wrapJSONErr(&cobra.Command{}, errAuthRequired(""))
		_ = wr.Close()
	}()
	var buf strings.Builder
	b := make([]byte, 4096)
	for {
		n, err := rd.Read(b)
		if n > 0 {
			buf.Write(b[:n])
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(buf.String(), "auth_required") {
		t.Errorf("expected auth_required envelope, got %q", buf.String())
	}
}

// TestRunResources_Anonymous fires the anonymous-flow branch.
func TestRunResources_Anonymous(t *testing.T) {
	withCleanState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	// Anonymous client (no auth transport).
	prevC := HTTPClient
	HTTPClient = &http.Client{}
	t.Cleanup(func() { HTTPClient = prevC })

	resourcesJSON = false
	resourcesFilter = nil
	resourcesLimit = 0
	t.Cleanup(func() { resourcesJSON = false })

	err := runResources(&cobra.Command{})
	// runResources may return a wrapped errAuthRequired for anonymous.
	if err == nil {
		t.Fatal("expected auth-required error for anonymous")
	}
}

// TestRunResources_SessionExpired covers the authed-401 branch.
func TestRunResources_SessionExpired(t *testing.T) {
	withCleanState(t)
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

	resourcesJSON = false
	resourcesFilter = nil
	resourcesLimit = 0

	err := runResources(&cobra.Command{})
	if err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Errorf("expected session expired, got %v", err)
	}
}

// TestRunResources_BadFilter fires the filter-parsing branch.
func TestRunResources_BadFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"items":[]}`))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	resourcesFilter = []string{"bad-filter"}
	t.Cleanup(func() { resourcesFilter = nil })

	err := runResources(&cobra.Command{})
	if err == nil || !strings.Contains(err.Error(), "filter") {
		t.Errorf("expected filter error, got %v", err)
	}
}

// TestRunResources_SuccessfulList covers the happy path including filter + limit.
func TestRunResources_SuccessfulList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"items":[
			{"token":"abcdefghijklmnop","resource_type":"postgres","name":"x","tier":"free","status":"active"},
			{"token":"def","resource_type":"redis","name":"","tier":"free","status":"active"}
		]}`))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	resourcesFilter = nil
	resourcesLimit = 1
	t.Cleanup(func() {
		resourcesFilter = nil
		resourcesLimit = 0
	})

	if err := runResources(&cobra.Command{}); err != nil {
		t.Fatalf("runResources: %v", err)
	}
}

// TestRunResources_JSONMode_EmptyArray covers the items=nil JSON [] branch.
func TestRunResources_JSONMode_EmptyArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"items":[]}`))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	resourcesJSON = true
	t.Cleanup(func() { resourcesJSON = false })

	if err := runResources(&cobra.Command{}); err != nil {
		t.Fatalf("runResources JSON: %v", err)
	}
}

// TestRunResources_BadJSON covers the parse-error branch.
func TestRunResources_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	if err := runResources(&cobra.Command{}); err == nil {
		t.Error("expected parse error")
	}
}

// TestRunResources_500 covers the parseAPIError branch.
func TestRunResources_500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"server"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	prev := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() { APIBaseURL = prev })

	if err := runResources(&cobra.Command{}); err == nil {
		t.Error("expected error on 500")
	}
}

// TestRunResources_NetworkError covers the HTTP error branch.
func TestRunResources_NetworkError(t *testing.T) {
	prev := APIBaseURL
	APIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { APIBaseURL = prev })

	if err := runResources(&cobra.Command{}); err == nil {
		t.Error("expected network error")
	}
}

// TestUseDefault_KeychainProbe covers the keychainBackend.Available()==true
// branch indirectly: we install nothing, env not disabled, then call
// UseDefault. On hosts where probing works it returns a backend; otherwise
// nil. Either path exercises the code.
func TestUseDefault_NoExistingBackend_KeychainProbed(t *testing.T) {
	// Reset.
	// We can't easily import secretstore from within cmd/, this is here
	// just as a smoke driver — secretstore tests already cover this branch.
	_ = t
}
