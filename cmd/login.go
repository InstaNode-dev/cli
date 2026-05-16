package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/instant-dev/cli/internal/cliconfig"
	"github.com/instant-dev/cli/internal/tokens"
)

// pollInterval is how often the CLI checks for auth completion.
const pollInterval = 2 * time.Second

// pollTimeout is the maximum wait time for the user to complete login in the browser.
const pollTimeout = 10 * time.Minute

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

	// Step 2: Open the browser.
	fmt.Printf("Opening browser to:\n  %s\n\n", session.AuthURL)
	fmt.Println("Waiting for you to sign in… (Ctrl-C to cancel)")
	openBrowser(session.AuthURL)

	// Step 3: Poll until the user completes auth or we time out.
	result, err := pollForAuthCompletion(session.SessionID)
	if err != nil {
		return err
	}

	// Step 4: Save credentials.
	cfg.APIKey = result.APIKey
	cfg.Email = result.Email
	cfg.Tier = result.Tier
	cfg.TeamName = result.TeamName
	cfg.APIBaseURL = APIBaseURL
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
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
	defer resp.Body.Close()

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
		resp.Body.Close()

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

	return nil, fmt.Errorf("timed out waiting for login (%.0f minutes). Try again.", pollTimeout.Minutes())
}

// pollForTierUpgrade polls GET /auth/me until the tier changes, up to 5 minutes.
func pollForTierUpgrade(cfg *cliconfig.Config) error {
	url := fmt.Sprintf("%s/auth/me", APIBaseURL)
	deadline := time.Now().Add(5 * time.Minute)
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
		resp.Body.Close()
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

// openBrowser opens url in the user's default browser, best-effort.
func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "darwin":
		err = exec.Command("open", url).Start()
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not open browser automatically. Visit the URL above manually.\n")
	}
}
