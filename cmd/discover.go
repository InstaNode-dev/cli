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

// resourcesJSON is the --json flag for `instant resources`. T16 P3:
// machine-readable output for agents that script the CLI; replaces the
// tabwriter human-readable table when set.
var resourcesJSON bool

// resourcesCmd lists resources from the agent API (requires login).
var resourcesCmd = &cobra.Command{
	Use:   "resources",
	Short: "List your provisioned resources",
	Long: `List all resources associated with your instanode.dev account.

Requires login. Run 'instant login' first if you haven't already.

Use 'instant status' to see resources tracked locally (no login required).

With --json, output is a machine-readable JSON array of resource objects
({token, resource_type, name, tier, status}) — suitable for piping into
jq or consuming directly from an agent script.
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
			// T16 P2-1 — parse the structured error envelope so 402 / 429 /
			// 5xx render the message + agent_action + upgrade_url rather
			// than a raw JSON dump.
			return parseAPIError(resp.StatusCode, raw)
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

		// T16 P3 — machine-readable output. Stable schema, exit 0 with `[]`
		// when empty (an agent does not have to special-case the "no
		// resources" sentence).
		if resourcesJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(result.Items)
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
	resourcesCmd.Flags().BoolVar(&resourcesJSON, "json", false,
		"Emit a JSON array of resources instead of a human-readable table")
	rootCmd.AddCommand(resourcesCmd)
}
