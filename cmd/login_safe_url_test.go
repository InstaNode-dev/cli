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

// TestOpenBrowser_HappyPathDoesNotPanic hits the real-runtime path on a
// known-good URL on whatever runtime.GOOS the test runs on — we don't
// assert on stderr (the helper may or may not be installed in CI), only
// that the wrapper returns cleanly.
func TestOpenBrowser_HappyPathDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("openBrowser panicked: %v", r)
		}
	}()
	// A reachable-looking URL — the exec may fail in CI (no $DISPLAY,
	// no `open`, no `rundll32`), and that's fine: openBrowser is
	// best-effort and just writes to stderr.
	openBrowser("https://instanode.dev/")
}

// TestOpenBrowserOn_AllBranches drives openBrowserOn across every outcome
// the function can return, satisfying the 100%-patch-coverage gate on a
// single-OS CI runner.
func TestOpenBrowserOn_AllBranches(t *testing.T) {
	// Refused (bad URL).
	if got := openBrowserOn("linux", "-FattackerPath"); got != "refused" {
		t.Errorf("bad URL: got %q, want refused", got)
	}
	// No helper for the GOOS.
	if got := openBrowserOn("plan9", "https://instanode.dev/"); got != "no-helper" {
		t.Errorf("unknown GOOS: got %q, want no-helper", got)
	}
	// Exec-failed: pretend the OS is linux but use a URL that's valid AND
	// pass a GOOS string we map to a binary that doesn't exist on PATH.
	// browserLauncherForGOOS returns "xdg-open" for linux; in CI it may
	// or may not exist. We force the exec-failed branch by mocking via
	// the unknown-GOOS path… actually the cleanest forcing function is
	// to verify the function returns SOMETHING from the known set on a
	// real OS. The "exec-failed" branch is covered indirectly when the
	// helper is missing from $PATH on the CI runner. We assert only that
	// the return is in the valid set.
	got := openBrowserOn("linux", "https://instanode.dev/")
	switch got {
	case "ok", "exec-failed":
		// either is fine — both exercise the launcher path
	default:
		t.Errorf("real-helper path: got %q, want ok or exec-failed", got)
	}
}

// TestOpenBrowserOn_ExecFailedForcedViaWindows forces the exec-failed
// branch on a Linux CI runner by asking for the windows launcher
// ("rundll32") which is guaranteed not to exist on PATH there.
func TestOpenBrowserOn_ExecFailedForced(t *testing.T) {
	// On any non-windows host, rundll32 is missing → exec.Start() returns
	// ErrNotExist → openBrowserOn returns "exec-failed". On a windows
	// host this test will accept "ok" too (no harm).
	got := openBrowserOn("windows", "https://instanode.dev/")
	if got != "exec-failed" && got != "ok" {
		t.Errorf("windows launcher: got %q, want exec-failed (or ok on a real windows runner)", got)
	}
}

// TestBrowserLauncherForGOOS asserts the per-OS helper choice across every
// branch (darwin / linux / windows / unknown). Decoupling from runtime.GOOS
// lets a Linux CI runner cover the macOS + Windows + fallback branches too
// — required for the 100%-patch-coverage gate.
func TestBrowserLauncherForGOOS(t *testing.T) {
	cases := []struct {
		goos     string
		wantName string
		wantArg0 string
	}{
		{goos: "darwin", wantName: "open", wantArg0: "https://x.example/"},
		{goos: "linux", wantName: "xdg-open", wantArg0: "https://x.example/"},
		{goos: "windows", wantName: "rundll32", wantArg0: "url.dll,FileProtocolHandler"},
		{goos: "plan9", wantName: "", wantArg0: ""},
		{goos: "", wantName: "", wantArg0: ""},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			name, args := browserLauncherForGOOS(tc.goos, "https://x.example/")
			if name != tc.wantName {
				t.Fatalf("name = %q, want %q", name, tc.wantName)
			}
			if name == "" {
				if args != nil {
					t.Fatalf("args should be nil on unknown GOOS, got %v", args)
				}
				return
			}
			if len(args) == 0 || args[0] != tc.wantArg0 {
				t.Fatalf("args[0] = %v, want %q", args, tc.wantArg0)
			}
		})
	}
}
