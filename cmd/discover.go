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

		// T16 P1-2 — uniform 401 handling. Three different code paths used
		// to do three different things on 401 (resources: exit 0, up: silent
		// re-provision, provision: bare error). Now:
		//   - anonymous caller: print 'not logged in' hint, exit 3 (auth req)
		//   - authenticated caller (stale token): print 'session expired',
		//     exit 3 (auth req) — same code so agents have one branch.
		if resp.StatusCode == http.StatusUnauthorized {
			if haveAuth() {
				return errSessionExpired()
			}
			fmt.Fprintln(os.Stderr, "Not logged in. Run `instant login` first.")
			return errAuthRequired("authentication required — run `instant login` first")
		}

		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("server returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
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
