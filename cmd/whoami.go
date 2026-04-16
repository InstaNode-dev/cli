package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/instant-dev/cli/internal/cliconfig"
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
		fmt.Printf("Key:      %s…\n", cfg.APIKey[:min(16, len(cfg.APIKey))])
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
