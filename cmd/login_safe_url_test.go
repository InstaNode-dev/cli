package cmd

import (
	"strings"
	"testing"
)

// SEC-CLI FINDING-17 — defense-in-depth around openBrowser.
//
// A hostile API server returning an auth_url like "-FattackerFile" would
// otherwise cause `open -F path` to execute on macOS, surfacing a local file
// in Finder. We refuse anything that:
//   - has a leading '-' (so it can't be parsed as a helper-binary flag)
//   - has a scheme other than http/https
//   - has an empty host
//   - is empty
func TestSafeBrowserURL(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantErr  bool
		errMatch string
	}{
		{name: "https ok", in: "https://instanode.dev/auth/cli/abc"},
		{name: "http ok", in: "http://localhost:8080/auth"},
		{name: "leading dash macOS exploit", in: "-Fattacker", wantErr: true, errMatch: "leading '-'"},
		{name: "javascript scheme", in: "javascript:alert(1)", wantErr: true, errMatch: "scheme"},
		{name: "file scheme", in: "file:///etc/passwd", wantErr: true, errMatch: "scheme"},
		{name: "ftp scheme", in: "ftp://x.example/", wantErr: true, errMatch: "scheme"},
		{name: "empty", in: "", wantErr: true, errMatch: "empty"},
		{name: "whitespace-only", in: "   ", wantErr: true, errMatch: "empty"},
		{name: "https no host", in: "https://", wantErr: true, errMatch: "host"},
		{name: "garbage", in: "::not a url", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeBrowserURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("safeBrowserURL(%q) = %q, want error", tc.in, got)
				}
				if tc.errMatch != "" && !strings.Contains(err.Error(), tc.errMatch) {
					t.Errorf("safeBrowserURL(%q) err = %v, want substring %q", tc.in, err, tc.errMatch)
				}
				return
			}
			if err != nil {
				t.Fatalf("safeBrowserURL(%q) unexpected error: %v", tc.in, err)
			}
			if got == "" {
				t.Errorf("safeBrowserURL(%q) returned empty", tc.in)
			}
		})
	}
}

// TestOpenBrowser_RefuseDoesNotPanic verifies the wrapper safely skips the
// exec.Command path when the URL is refused — we don't crash and we don't
// pass user-controlled bytes to a helper binary.
func TestOpenBrowser_RefuseDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("openBrowser panicked on bad URL: %v", r)
		}
	}()
	openBrowser("-FattackerPath")
	openBrowser("javascript:alert(1)")
	openBrowser("")
}
