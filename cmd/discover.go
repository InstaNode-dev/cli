package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// resourcesCmd lists resources from the agent API (requires login).
var resourcesCmd = &cobra.Command{
	Use:   "resources",
	Short: "List your provisioned resources",
	Long: `List all resources associated with your instanode.dev account.

Requires login. Run 'instant login' first if you haven't already.

Use 'instant status' to see resources tracked locally (no login required).
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		url := fmt.Sprintf("%s/api/v1/resources", APIBaseURL)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}

		resp, err := HTTPClient.Do(req)
		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized {
			fmt.Fprintln(os.Stderr, "Not logged in. Run `instant login` first.")
			return nil
		}

		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("server returned %d: %s", resp.StatusCode, raw)
		}

		var result struct {
			OK    bool `json:"ok"`
			Total int  `json:"total"`
			Items []struct {
				Token        string `json:"token"`
				ResourceType string `json:"resource_type"`
				Name         string `json:"name"`
				Tier         string `json:"tier"`
				Status       string `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		if len(result.Items) == 0 {
			fmt.Println("No resources found.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TOKEN\tTYPE\tNAME\tTIER\tSTATUS")
		for _, r := range result.Items {
			shortToken := r.Token
			if len(shortToken) > 12 {
				shortToken = shortToken[:12] + "…"
			}
			name := r.Name
			if name == "" {
				name = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				shortToken, r.ResourceType, name, r.Tier, r.Status)
		}
		w.Flush()
		return nil
	},
}

func init() {
	rootCmd.AddCommand(resourcesCmd)
}
