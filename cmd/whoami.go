package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/instant-dev/cli/internal/cliconfig"
	"github.com/instant-dev/cli/internal/secretstore"
	"github.com/spf13/cobra"
)

// whoamiJSON is the --json flag for `instant whoami`. T16 P3: machine-readable
// identity output for agents. The bearer token is NEVER included even in JSON
// mode — only the truncated display form and the secret-backend name (P1-1).
var whoamiJSON bool

// whoamiJSONOutput is the stable schema emitted by `whoami --json`. Fields:
//
//	authenticated    true when a credential is on disk / in the keychain
//	email            customer email, "" when anonymous
//	tier             effective plan tier (anonymous, hobby, pro, ...)
//	team_name        team display name, "" if unset
//	api_url          resolved api base URL
//	key_display      truncated key for display (NEVER the full token)
//	secret_backend   "macOS Keychain" / "libsecret" / "on-disk fallback" / etc.
type whoamiJSONOutput struct {
	Authenticated bool   `json:"authenticated"`
	Email         string `json:"email"`
	Tier          string `json:"tier"`
	TeamName      string `json:"team_name"`
	APIURL        string `json:"api_url"`
	KeyDisplay    string `json:"key_display"`
	SecretBackend string `json:"secret_backend"`
}

var whoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Show the currently authenticated account",
	Long: `Show the currently authenticated account.

With --json, output is a machine-readable identity object. The bearer
token is NEVER included even in JSON mode (T16 P1-1); only a truncated
display form and the secret-backend name are surfaced.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := cliconfig.Load()
		if err != nil {
			return err
		}

		if whoamiJSON {
			out := whoamiJSONOutput{
				Authenticated: cfg.IsAuthenticated(),
				Email:         cfg.Email,
				Tier:          cfg.EffectiveTier(),
				TeamName:      cfg.TeamName,
				APIURL:        cfg.APIBaseURL,
				KeyDisplay:    secretstore.TruncateForDisplay(cfg.APIKey),
				SecretBackend: cfg.SecretBackendName(),
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}

		if !cfg.IsAuthenticated() {
			fmt.Println("Not logged in (anonymous mode).")
			fmt.Printf("Run `instant login` to authenticate, or `instant db new` to provision a database without an account.\n")
			return nil
		}

		fmt.Printf("Email:    %s\n", cfg.Email)
		fmt.Printf("Plan:     %s\n", cfg.EffectiveTier())
		if cfg.TeamName != "" {
			fmt.Printf("Team:     %s\n", cfg.TeamName)
		}
		fmt.Printf("API URL:  %s\n", cfg.APIBaseURL)
		// T16 P1-1: never display more than 8 chars of the bearer token,
		// and surface which backend holds it so the user can tell
		// "macOS Keychain" from "on-disk fallback".
		fmt.Printf("Key:      %s\n", secretstore.TruncateForDisplay(cfg.APIKey))
		fmt.Printf("Stored:   %s\n", cfg.SecretBackendName())
		return nil
	},
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove locally saved credentials",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := cliconfig.Load()
		if err != nil {
			return err
		}
		if !cfg.IsAuthenticated() {
			fmt.Println("Not logged in.")
			return nil
		}
		email := cfg.Email
		if err := cliconfig.Clear(); err != nil {
			return fmt.Errorf("removing credentials: %w", err)
		}
		fmt.Printf("Logged out %s.\n", email)
		fmt.Println("Anonymous mode restored. Your provisioned resources are still active on the server.")
		return nil
	},
}

func init() {
	whoamiCmd.Flags().BoolVar(&whoamiJSON, "json", false,
		"Emit a JSON identity object instead of a human-readable summary")
	rootCmd.AddCommand(whoamiCmd)
	rootCmd.AddCommand(logoutCmd)
}
