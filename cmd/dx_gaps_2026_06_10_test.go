package cmd

// dx_gaps_2026_06_10_test.go — four contained DX-gap fixes (2026-06-10):
//
//   1. PAT fallback signpost on `instant login` poll timeout AND in
//      `login --help`. A stranded device-flow user must learn the
//      --token / INSTANT_TOKEN escape hatch.
//   2. `instant deploy new --name foo` must reach the helpful MCP/curl
//      pointer instead of dying with cobra `unknown flag: --name`.
//   3. `instant resources` (no auth) must print the "not logged in"
//      guidance EXACTLY ONCE (main.go owns the print; the handler no
//      longer also writes to os.Stderr).
//   4. `instant --version` falls back to runtime/debug VCS metadata when
//      ldflags are empty (the `go install` / `go build` path).

import (
	"runtime/debug"
	"strings"
	"testing"
)

// ── Fix 1: PAT fallback signpost ────────────────────────────────────────────

// TestLoginTimeout_PrintsPATWorkaround pins that the login-poll timeout error
// surfaces the Personal Access Token escape hatch (settings URL + both
// --token and INSTANT_TOKEN forms). A user whose browser flow stalls must be
// told how to authenticate without it.
func TestLoginTimeout_PrintsPATWorkaround(t *testing.T) {
	withCleanState(t)
	withShortPolls(t)

	prev := APIBaseURL
	// Unroutable host → every poll attempt errors and the loop eventually
	// hits the deadline, returning the timeout error.
	APIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { APIBaseURL = prev })

	_, err := pollForAuthCompletion("s1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	msg := err.Error()
	for _, want := range []string{
		"https://instanode.dev/app/settings",
		"--token",
		"INSTANT_TOKEN",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("timeout error must mention %q for the PAT workaround; got %q", want, msg)
		}
	}
}

// TestLoginHelp_DocumentsPATWorkaround pins that `instant login --help`
// (the Long text) tells the user about the PAT escape hatch before they
// even start a flow that might time out / fail on a headless box.
func TestLoginHelp_DocumentsPATWorkaround(t *testing.T) {
	long := loginCmd.Long
	for _, want := range []string{
		"https://instanode.dev/app/settings",
		"--token",
		"INSTANT_TOKEN",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("login --help Long must mention %q; got %q", want, long)
		}
	}
}

// TestPATHint_SingleSource guards rule 16 (one token, all sites): the timeout
// error and the help text must both derive their core sentence from the same
// const so they can never drift apart.
func TestPATHint_SingleSource(t *testing.T) {
	if !strings.Contains(patWorkaroundHint, "https://instanode.dev/app/settings") {
		t.Errorf("patWorkaroundHint missing settings URL: %q", patWorkaroundHint)
	}
	if !strings.Contains(patWorkaroundHint, "--token") ||
		!strings.Contains(patWorkaroundHint, "INSTANT_TOKEN") {
		t.Errorf("patWorkaroundHint missing one of the two PAT auth forms: %q", patWorkaroundHint)
	}
}

// ── Fix 2: deploy stub tolerates unknown flags ──────────────────────────────

// TestDeployStub_UnknownFlagReachesPointer pins that an agent passing the
// REAL deploy flags (`--name`, `--env`) to the stub still lands on the
// helpful MCP/curl pointer rather than cobra's `unknown flag: --name`
// flag-parse error (which fires BEFORE RunE and looks like a bug in the
// agent's own command).
func TestDeployStub_UnknownFlagReachesPointer(t *testing.T) {
	newITContext(t)

	cases := [][]string{
		{"deploy", "new", "--name", "foo"},
		{"deploy", "new", "--name", "foo", "--env", "production"},
		{"deploy", "logs", "some-id", "--follow"},
		{"deploy", "--name", "foo"}, // bare parent + unknown flag
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, stderr, err := run(args...)
			if err == nil {
				t.Fatalf("%v: MUST exit non-zero (stub not implemented)", args)
			}
			// The whole point: the error must be the not-implemented
			// pointer, NOT an "unknown flag" flag-parse failure.
			combined := strings.ToLower(stderr + err.Error())
			if strings.Contains(combined, "unknown flag") {
				t.Errorf("%v: reached cobra unknown-flag error instead of the pointer; got %q",
					args, combined)
			}
			if !strings.Contains(combined, "implement") {
				t.Errorf("%v: error must mention not-implemented pointer; got %q", args, combined)
			}
		})
	}
}

// ── Fix 3: single not-logged-in print ───────────────────────────────────────

// TestResources_NoAuth_NoDuplicateStderrHint pins that the resources 401
// no-auth branch no longer ALSO writes "Not logged in. Run `instant login`
// first." to os.Stderr. Pre-fix the handler printed that sentence AND main.go
// printed the returned errAuthRequired message — two not-logged-in lines for
// one failure. main.go now owns the single print; the handler is silent.
//
// We capture the OS-level stderr (the pre-fix print used fmt.Fprintln(os.Stderr,
// …), which bypasses cobra's SetErr buffer that run() captures).
func TestResources_NoAuth_NoDuplicateStderrHint(t *testing.T) {
	c := newITContext(t)
	c.mock.mu.Lock()
	c.mock.requireAuth = true
	c.mock.mu.Unlock()

	resetJSONFlags()

	var err error
	_, osStderr := captureStdout(t, func() {
		_, _, err = run("resources")
	})
	if err == nil {
		t.Fatal("resources w/o auth: expected non-nil err (exit 3)")
	}
	if ExitCodeFor(err) != ExitAuthRequired {
		t.Errorf("exit code = %d, want %d (ExitAuthRequired)", ExitCodeFor(err), ExitAuthRequired)
	}
	// The handler must no longer write the legacy "Not logged in." sentence
	// to os.Stderr; main.go owns the single user-facing print.
	if strings.Contains(osStderr, "Not logged in. Run") {
		t.Errorf("handler must not print the legacy not-logged-in hint to os.Stderr (main.go owns it); got %q",
			osStderr)
	}
	// The returned error still names `instant login` so no guidance is lost.
	if !strings.Contains(err.Error(), "instant login") {
		t.Errorf("returned error must still point at `instant login`; got %q", err.Error())
	}
}

// ── Fix 4: --version VCS fallback ───────────────────────────────────────────

// fakeBuildInfo builds a *debug.BuildInfo carrying the given vcs settings so
// resolveBuildInfo's backfill path is exercised without a real git checkout.
func fakeBuildInfo(rev, vtime string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		bi := &debug.BuildInfo{}
		if rev != "" {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: "vcs.revision", Value: rev})
		}
		if vtime != "" {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: "vcs.time", Value: vtime})
		}
		return bi, true
	}
}

// TestResolveBuildInfo_LdflagsWin: when ldflags are stamped, the VCS reader is
// never consulted — the stamped values pass straight through.
func TestResolveBuildInfo_LdflagsWin(t *testing.T) {
	v, c, b := resolveBuildInfo("v1.2.3", "abc1234", "2026-06-10T00:00:00Z",
		fakeBuildInfo("ffffffffffffffffffffffffffffffffffffffff", "2099-01-01T00:00:00Z"))
	if v != "v1.2.3" || c != "abc1234" || b != "2026-06-10T00:00:00Z" {
		t.Errorf("ldflag values must win, got (%q,%q,%q)", v, c, b)
	}
}

// TestResolveBuildInfo_VCSFallback: empty commit/buildTime (the go install /
// go build path) are backfilled from VCS metadata, and the revision is
// shortened to the platform's 7-char form.
func TestResolveBuildInfo_VCSFallback(t *testing.T) {
	v, c, b := resolveBuildInfo("", "", "",
		fakeBuildInfo("0123456789abcdef0123456789abcdef01234567", "2026-06-09T12:00:00Z"))
	if v != "dev" {
		t.Errorf("version = %q, want dev (no ldflag, no VCS version)", v)
	}
	if c != "0123456" {
		t.Errorf("commit = %q, want short VCS sha 0123456", c)
	}
	if b != "2026-06-09T12:00:00Z" {
		t.Errorf("buildTime = %q, want VCS time", b)
	}
}

// TestResolveBuildInfo_SentinelInputFallsBack pins the REAL production path:
// main.go declares the un-stamped defaults as the SENTINELS "dev"/"unknown"
// (not ""), so SetBuildInfo passes sentinels. The fallback must treat those
// as unset and still backfill from VCS — a naive `== ""` guard would not.
func TestResolveBuildInfo_SentinelInputFallsBack(t *testing.T) {
	v, c, b := resolveBuildInfo("dev", "unknown", "unknown",
		fakeBuildInfo("abcdef0123456789abcdef0123456789abcdef01", "2026-06-08T08:00:00Z"))
	if v != "dev" {
		t.Errorf("version = %q, want dev", v)
	}
	if c != "abcdef0" {
		t.Errorf("commit = %q, want short VCS sha abcdef0 (sentinel must not block fallback)", c)
	}
	if b != "2026-06-08T08:00:00Z" {
		t.Errorf("buildTime = %q, want VCS time (sentinel must not block fallback)", b)
	}
}

// TestResolveBuildInfo_NoVCSNoLdflags: nothing available anywhere → the
// pre-existing dev/unknown/unknown sentinel is preserved (e.g. `go run`).
func TestResolveBuildInfo_NoVCSNoLdflags(t *testing.T) {
	v, c, b := resolveBuildInfo("", "", "", func() (*debug.BuildInfo, bool) { return nil, false })
	if v != "dev" || c != "unknown" || b != "unknown" {
		t.Errorf("sentinel fallback broken, got (%q,%q,%q)", v, c, b)
	}
	// nil reader must also be tolerated.
	v, c, b = resolveBuildInfo("", "", "", nil)
	if v != "dev" || c != "unknown" || b != "unknown" {
		t.Errorf("nil-reader fallback broken, got (%q,%q,%q)", v, c, b)
	}
}

// TestShortSHA_ShortInputUnchanged: a sub-7-char revision is returned as-is so
// an unexpected/dirty value still surfaces rather than being mangled.
func TestShortSHA_ShortInputUnchanged(t *testing.T) {
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA(abc) = %q, want abc", got)
	}
	if got := shortSHA("0123456789"); got != "0123456" {
		t.Errorf("shortSHA truncation = %q, want 0123456", got)
	}
}

// TestSetBuildInfo_VersionStringShape is an end-to-end smoke over SetBuildInfo
// (which calls resolveBuildInfo with the real debug.ReadBuildInfo): the
// resulting rootCmd.Version is the documented "<v> (<c>, <t>)" shape and is
// never the empty-paren placeholder.
func TestSetBuildInfo_VersionStringShape(t *testing.T) {
	prev := rootCmd.Version
	t.Cleanup(func() { rootCmd.Version = prev })

	SetBuildInfo("v9.9.9", "deadbee", "2026-06-10T00:00:00Z")
	if rootCmd.Version != "v9.9.9 (deadbee, 2026-06-10T00:00:00Z)" {
		t.Errorf("rootCmd.Version = %q", rootCmd.Version)
	}

	// Sentinel ldflags (main.go's un-stamped defaults) → resolveBuildInfo
	// runs against the test binary's real build info. The shape must still
	// be well-formed (never "(, )") and must start with the dev sentinel.
	// (Under `go test` the binary carries no runtime vcs settings, so this
	// lands on dev (unknown, unknown) — still well-formed.)
	SetBuildInfo("dev", "unknown", "unknown")
	if !strings.HasPrefix(rootCmd.Version, "dev (") || !strings.HasSuffix(rootCmd.Version, ")") {
		t.Errorf("sentinel-ldflag Version malformed: %q", rootCmd.Version)
	}
	if strings.Contains(rootCmd.Version, "(, )") {
		t.Errorf("Version must never have empty commit+time fields: %q", rootCmd.Version)
	}
}
