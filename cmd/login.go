package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/InstaNode-dev/cli/internal/cliconfig"
	"github.com/InstaNode-dev/cli/internal/tokens"
)

// patWorkaroundHint is the single source of truth for the "your login timed
// out — mint a PAT instead" signpost. It is surfaced in two places: the
// pollForAuthCompletion timeout error (so a stranded user sees it the moment
// the browser flow stalls) and the `instant login --help` long text (so a
// user who already knows the device-flow is flaky finds the escape hatch
// before they even start). The CLI already honours --token / INSTANT_TOKEN
// (root.go initConfig priority chain) and instanode-web mints PATs at
// /app/settings — but until this hint shipped the CLI never told a timed-out
// user that path existed. Kept as a package const (not an inline literal) so
// the two emitters can never drift (CLAUDE.md rule 16 — one token, all sites).
const patWorkaroundHint = "Login timed out. Workaround: mint a Personal Access Token at " +
	"https://instanode.dev/app/settings and run `instant --token <pat> ...` or " +
	"`export INSTANT_TOKEN=<pat>`."

// pollInterval is how often the CLI checks for auth completion.
//
// Declared as var (not const) so tests can lower it to milliseconds without
// changing production behaviour. Production callers never reassign it.
var pollInterval = 2 * time.Second

// pollTimeout is the maximum wait time for the user to complete login in the browser.
//
// Same rationale as pollInterval — var, not const, so the 10-minute (or
// 5-minute) production windows can be reduced to milliseconds in tests.
var pollTimeout = 10 * time.Minute

// tierUpgradeTimeout is the upper bound on pollForTierUpgrade. Production
// is 5 minutes; tests lower it.
var tierUpgradeTimeout = 5 * time.Minute

// browserEnvVar is the conventional Unix variable naming a user's preferred
// URL opener (e.g. `BROWSER=firefox` or `BROWSER='wslview'`). U2: honored
// before the per-GOOS default so a headless/agent box with a custom opener
// works without code changes. Named const so the env lookup has one spelling.
const browserEnvVar = "BROWSER"

// loginNoBrowser is bound to `instant login --no-browser`. U2: on a headless
// agent box, attempting to spawn open/xdg-open/rundll32 is useless — it prints
// "Could not open browser automatically" then blocks polling for 5 minutes
// with no escape. With --no-browser the CLI prints the auth URL + session id
// to stdout (so a human or supervising agent can open it elsewhere) and polls
// without EVER trying to launch a browser.
var loginNoBrowser bool

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Log in to instanode.dev and save credentials locally",
	Long: `Open a browser to authenticate with instanode.dev.

The CLI creates a one-time login session, opens your browser,
and waits while you sign in (GitHub OAuth or magic link).

After login your API key is saved to ~/.instant-config.
Subsequent commands will use it automatically for authenticated API calls.

If you upgrade to a paid plan, run `+"`instant login`"+` again to refresh
your tier — or the CLI will detect it automatically on the next API call.

On a headless machine, pass `+"`--no-browser`"+`: the CLI prints the login URL
and session id and polls without trying to launch a browser. It also honors the
`+"`BROWSER`"+` environment variable when choosing how to open the URL.

If the browser flow times out or you can't sign in at all, skip it:
mint a Personal Access Token at https://instanode.dev/app/settings, then
authenticate any command with `+"`instant --token <pat> ...`"+` or by exporting
`+"`INSTANT_TOKEN=<pat>`"+` in your shell.
`,
	RunE: runLogin,
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Open the upgrade page to increase your plan limits",
	Long: `Open the instanode.dev upgrade page in your browser.

Your current anonymous tokens are passed so the upgrade page can
show exactly which resources you have running and pre-fill your plan.
`,
	RunE: runUpgrade,
}

func init() {
	// U2: --no-browser makes login usable on headless/agent boxes — print the
	// auth URL + session id and poll, never trying to spawn a browser.
	loginCmd.Flags().BoolVar(&loginNoBrowser, "no-browser", false,
		"Print the login URL + session id and poll without launching a browser (headless/agent boxes)")
	rootCmd.AddCommand(loginCmd)
	rootCmd.AddCommand(upgradeCmd)
}

// ── login ─────────────────────────────────────────────────────────────────────

func runLogin(cmd *cobra.Command, args []string) error {
	cfg, err := cliconfig.Load()
	if err != nil {
		return err
	}

	if cfg.IsAuthenticated() {
		fmt.Printf("Already logged in as %s (%s).\n", cfg.Email, cfg.EffectiveTier())
		fmt.Println("Run `instant logout` first to switch accounts.")
		return nil
	}

	// Gather locally saved anonymous tokens so the server can pre-associate them.
	anonTokens := loadAnonymousTokens()

	// Step 1: Create a CLI auth session on the server.
	session, err := createCLISession(anonTokens)
	if err != nil {
		return fmt.Errorf("starting login: %w", err)
	}

	// Step 2: Surface the auth URL. U2: with --no-browser (headless / agent
	// box) we print the URL + session id to stdout and NEVER attempt to launch
	// a browser — spawning open/xdg-open on a headless host is useless and the
	// failure message was followed by a 5-minute silent poll. The interactive
	// happy path is unchanged: print + best-effort browser launch.
	if loginNoBrowser {
		fmt.Printf("Open this URL to sign in:\n  %s\n", session.AuthURL)
		fmt.Printf("Session: %s\n\n", session.SessionID)
		fmt.Println("Waiting for you to sign in… (Ctrl-C to cancel)")
	} else {
		fmt.Printf("Opening browser to:\n  %s\n\n", session.AuthURL)
		fmt.Println("Waiting for you to sign in… (Ctrl-C to cancel)")
		openBrowser(session.AuthURL)
	}

	// Step 3: Poll until the user completes auth or we time out.
	result, err := pollForAuthCompletion(session.SessionID)
	if err != nil {
		return err
	}

	// Step 4: Save credentials. cfg.Save() durably persists the bearer token —
	// keychain when it's available AND the write survives an immediate
	// read-back, otherwise the 0600 ~/.instant-config file fallback (the path
	// that the next `instant whoami` / authenticated call reads). If Save
	// returns an error NEITHER store accepted the token: we MUST NOT print
	// "✓ Logged in" (the headless false-success this fix exists to kill).
	// Instead hand the user the token + an explicit `export INSTANT_TOKEN=…`
	// escape hatch so a headless agent can still proceed in-flow.
	cfg.APIKey = result.APIKey
	cfg.Email = result.Email
	cfg.Tier = result.Tier
	cfg.TeamName = result.TeamName
	cfg.APIBaseURL = APIBaseURL
	if err := cfg.Save(); err != nil {
		return fmt.Errorf(
			"login succeeded but the API key could not be saved to the keychain "+
				"or to ~/.instant-config (%w).\nUse the token directly instead:\n"+
				"  export INSTANT_TOKEN=%s\n"+
				"…then re-run your command. (Avoid committing the token.)",
			err, result.APIKey)
	}

	fmt.Printf("\n✓  Logged in as %s (%s)\n", result.Email, result.Tier)
	if len(result.ClaimedTokens) > 0 {
		fmt.Printf("   %d anonymous resource token(s) claimed to your account.\n", len(result.ClaimedTokens))
	}
	if result.Tier == "anonymous" || result.Tier == "hobby" {
		fmt.Printf("\nUpgrade for higher limits: %s/pricing\n", APIBaseURL)
	}
	return nil
}

// ── upgrade ───────────────────────────────────────────────────────────────────

func runUpgrade(cmd *cobra.Command, args []string) error {
	cfg, _ := cliconfig.Load()

	// Build upgrade URL. If the user is logged in, go directly to billing.
	// If anonymous, go to /start with known tokens so the page is pre-filled.
	var upgradeURL string
	if cfg.IsAuthenticated() {
		upgradeURL = fmt.Sprintf("%s/billing", APIBaseURL)
	} else {
		anonTokens := loadAnonymousTokens()
		if len(anonTokens) > 0 {
			upgradeURL = fmt.Sprintf("%s/start?tokens=%s", APIBaseURL, strings.Join(anonTokens, ","))
		} else {
			upgradeURL = fmt.Sprintf("%s/pricing", APIBaseURL)
		}
	}

	fmt.Printf("Opening: %s\n", upgradeURL)
	openBrowser(upgradeURL)

	// After the browser opens, poll to detect when the user's tier changes.
	if cfg.IsAuthenticated() {
		fmt.Println("Waiting for upgrade to complete…")
		if err := pollForTierUpgrade(cfg); err != nil {
			fmt.Println("Could not detect upgrade automatically. Run `instant login` to refresh.")
		}
	} else {
		fmt.Println("Sign up and your anonymous resources can be associated with your account.")
		fmt.Println("Then run `instant login` to authenticate the CLI.")
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

type cliSession struct {
	SessionID string `json:"session_id"`
	AuthURL   string `json:"auth_url"`
}

type authResult struct {
	APIKey        string   `json:"api_key"`
	Email         string   `json:"email"`
	Tier          string   `json:"tier"`
	TeamName      string   `json:"team_name"`
	ClaimedTokens []string `json:"claimed_tokens"`
}

// createCLISession calls POST /auth/cli to start a login session.
// anonTokens are passed so the server can pre-associate them.
func createCLISession(anonTokens []string) (*cliSession, error) {
	url := fmt.Sprintf("%s/auth/cli", APIBaseURL)

	type body struct {
		AnonTokens []string `json:"anon_tokens,omitempty"`
	}
	b, _ := json.Marshal(body{AnonTokens: anonTokens})

	resp, err := HTTPClient.Post(url, "application/json",
		strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, raw)
	}

	var session cliSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, fmt.Errorf("parsing session: %w", err)
	}
	if session.SessionID == "" || session.AuthURL == "" {
		return nil, fmt.Errorf("invalid session response from server")
	}
	return &session, nil
}

// pollForAuthCompletion calls GET /auth/cli/<id> every pollInterval until
// the server returns a completed auth result or pollTimeout elapses.
func pollForAuthCompletion(sessionID string) (*authResult, error) {
	url := fmt.Sprintf("%s/auth/cli/%s", APIBaseURL, sessionID)
	deadline := time.Now().Add(pollTimeout)
	dots := 0

	for time.Now().Before(deadline) {
		resp, err := HTTPClient.Get(url)
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}

		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusAccepted {
			// Still pending — print a progress dot and wait.
			dots++
			if dots%5 == 0 {
				fmt.Print(".")
			}
			time.Sleep(pollInterval)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			var result authResult
			if err := json.Unmarshal(raw, &result); err != nil {
				return nil, fmt.Errorf("parsing auth result: %w", err)
			}
			if result.APIKey == "" {
				return nil, fmt.Errorf("server returned success but no API key")
			}
			return &result, nil
		}

		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, raw)
	}

	return nil, fmt.Errorf("timed out waiting for login after %.0f minutes; try again.\n%s",
		pollTimeout.Minutes(), patWorkaroundHint)
}

// pollForTierUpgrade polls GET /auth/me until the tier changes, up to 5 minutes.
func pollForTierUpgrade(cfg *cliconfig.Config) error {
	url := fmt.Sprintf("%s/auth/me", APIBaseURL)
	deadline := time.Now().Add(tierUpgradeTimeout)
	originalTier := cfg.Tier

	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

		resp, err := HTTPClient.Do(req)
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}

		var result struct {
			Tier     string `json:"tier"`
			Email    string `json:"email"`
			TeamName string `json:"team_name"`
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err := json.Unmarshal(raw, &result); err != nil {
			time.Sleep(pollInterval)
			continue
		}

		if result.Tier != originalTier {
			cfg.Tier = result.Tier
			cfg.TeamName = result.TeamName
			_ = cfg.Save()
			fmt.Printf("\n✓  Plan upgraded to %s!\n", result.Tier)
			return nil
		}
		fmt.Print(".")
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("timed out")
}

// loadAnonymousTokens returns token strings from the local tokens store.
func loadAnonymousTokens() []string {
	store, err := tokens.Load()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(store.Entries))
	for _, e := range store.Entries {
		out = append(out, e.Token)
	}
	return out
}

// safeBrowserURL validates that raw is a well-formed http(s) URL whose first
// character is not '-' (so the URL can never be interpreted as a flag by the
// helper binary we exec). SEC-CLI FINDING-17.
//
// This is defense-in-depth: a hostile API server returning
// `{"auth_url":"-Fpath"}` would otherwise have `open -F path` invoked on
// macOS, exposing a local file in Finder. The CLI talks to TLS-protected
// instanode.dev today so the threat model is narrow — but the cost of
// hardening is < 20 LOC.
func safeBrowserURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty URL")
	}
	// Reject leading dash so the URL can't be parsed as a flag by the
	// underlying open/xdg-open/rundll32 helper.
	if raw[0] == '-' {
		return "", fmt.Errorf("refusing to open URL with leading '-': %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("refusing to open URL with scheme %q (only http/https allowed)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("refusing to open URL with empty host: %q", raw)
	}
	return raw, nil
}

// browserLauncherForGOOS returns the helper binary + arg list that opens a
// URL in the user's default browser on the given GOOS. Extracted so the
// per-platform fan-out is testable from a single-OS CI runner (the variant
// not matching runtime.GOOS would otherwise be uncovered, which is what
// our 100%-patch-coverage gate cares about).
//
// U2: $BROWSER takes precedence over the per-GOOS default on EVERY platform.
// On a headless Linux agent there is no xdg-open, but the operator may export
// `BROWSER=wslview` (WSL) or any custom opener; honoring it lets login work
// without a code change. The $BROWSER value is treated as a bare command name
// (no shell parsing) so a hostile config can't inject extra args — the safe
// URL is passed as a single argument exactly as for the built-in helpers.
//
// nil result means "no known helper for this GOOS"; caller should skip the
// exec attempt and tell the user to open the URL manually.
func browserLauncherForGOOS(goos, safeURL string) (name string, args []string) {
	if b := strings.TrimSpace(os.Getenv(browserEnvVar)); b != "" {
		return b, []string{safeURL}
	}
	switch goos {
	case "darwin":
		return "open", []string{safeURL}
	case "linux":
		return "xdg-open", []string{safeURL}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", safeURL}
	}
	return "", nil
}

// openBrowserOn is the GOOS-injectable core of openBrowser; the public
// wrapper passes runtime.GOOS but tests can drive every per-OS branch
// (including the unknown-GOOS fallback and the exec-failure path) from a
// single CI runner. Returns "ok" / "refused" / "no-helper" / "exec-failed"
// so a test can assert outcome without parsing stderr.
func openBrowserOn(goos, rawURL string) string {
	safe, verr := safeBrowserURL(rawURL)
	if verr != nil {
		fmt.Fprintf(os.Stderr, "Refusing to open URL: %v\n", verr)
		return "refused"
	}
	name, args := browserLauncherForGOOS(goos, safe)
	if name == "" {
		fmt.Fprintf(os.Stderr, "Could not open browser automatically. Visit the URL above manually.\n")
		return "no-helper"
	}
	if err := exec.Command(name, args...).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Could not open browser automatically. Visit the URL above manually.\n")
		return "exec-failed"
	}
	return "ok"
}

// openBrowser opens url in the user's default browser, best-effort.
//
// The url is validated by safeBrowserURL before being passed to any helper
// binary; a server-controlled URL with a hostile scheme or leading-dash
// payload is refused with a clear stderr message rather than executed.
func openBrowser(rawURL string) {
	_ = openBrowserOn(runtime.GOOS, rawURL)
}
