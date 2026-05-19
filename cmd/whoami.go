package cmd

import (
	"fmt"

	"github.com/instant-dev/cli/internal/cliconfig"
	"github.com/instant-dev/cli/internal/secretstore"
	"github.com/spf13/cobra"
)

var whoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Show the currently authenticated account",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := cliconfig.Load()
		if err != nil {
			return err
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
	rootCmd.AddCommand(whoamiCmd)
	rootCmd.AddCommand(logoutCmd)
}
