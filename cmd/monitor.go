package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/instant-dev/cli/internal/tokens"
	"github.com/spf13/cobra"
)

// ── Provisioning subcommand groups ───────────────────────────────────────────
// instant db new [name]
// instant cache new [name]
// instant nosql new [name]
// instant queue new [name]

var (
	dbCmd    = &cobra.Command{Use: "db", Short: "Manage Postgres database resources"}
	cacheCmd = &cobra.Command{Use: "cache", Short: "Manage Redis cache resources"}
	nosqlCmd = &cobra.Command{Use: "nosql", Short: "Manage MongoDB document-store resources"}
	queueCmd = &cobra.Command{Use: "queue", Short: "Manage NATS JetStream queue resources"}
)

var dbNewCmd = &cobra.Command{
	Use:   "new [name]",
	Short: "Provision a Postgres database (+ pgvector)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  makeProvisionCmd("/db/new", "db"),
}

var cacheNewCmd = &cobra.Command{
	Use:   "new [name]",
	Short: "Provision a Redis cache",
	Args:  cobra.MaximumNArgs(1),
	RunE:  makeProvisionCmd("/cache/new", "cache"),
}

var nosqlNewCmd = &cobra.Command{
	Use:   "new [name]",
	Short: "Provision a MongoDB document store",
	Args:  cobra.MaximumNArgs(1),
	RunE:  makeProvisionCmd("/nosql/new", "nosql"),
}

var queueNewCmd = &cobra.Command{
	Use:   "new [name]",
	Short: "Provision a NATS JetStream queue",
	Args:  cobra.MaximumNArgs(1),
	RunE:  makeProvisionCmd("/queue/new", "queue"),
}

// makeProvisionCmd returns a RunE function that POSTs to the given endpoint
// and prints the provisioned connection URL.
func makeProvisionCmd(endpoint, resourceType string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		name := resourceType
		if len(args) == 1 {
			name = args[0]
		}

		creds, err := provisionResource(endpoint, name)
		if err != nil {
			return fmt.Errorf("provisioning failed: %w", err)
		}

		// Save token locally for `instant status`.
		if store, loadErr := tokens.Load(); loadErr == nil {
			_ = store.Add(tokens.Entry{
				Token:  creds.Token,
				Name:   creds.Name,
				URL:    creds.ConnectionURL,
				Source: "provision",
			})
		}

		fmt.Printf("ok    %-8s  %s\n", resourceType, creds.Token)
		fmt.Printf("url   %s\n", creds.ConnectionURL)
		if creds.Tier != "" {
			fmt.Printf("tier  %s\n", creds.Tier)
		}
		if creds.Note != "" {
			fmt.Printf("\n%s\n", creds.Note)
		}
		return nil
	}
}

// provisionResponse is the shape returned by POST /{service}/new endpoints.
type provisionResponse struct {
	OK            bool   `json:"ok"`
	Token         string `json:"token"`
	Name          string `json:"name"`
	ConnectionURL string `json:"connection_url"`
	Tier          string `json:"tier"`
	Note          string `json:"note"`
	Upgrade       string `json:"upgrade"`
}

// provisionResource calls POST {APIBaseURL}{endpoint} and returns parsed credentials.
func provisionResource(endpoint, name string) (*provisionResponse, error) {
	url := APIBaseURL + endpoint
	body, _ := json.Marshal(map[string]string{"name": name})

	resp, err := HTTPClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, raw)
	}

	var result provisionResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if !result.OK || result.Token == "" {
		return nil, fmt.Errorf("unexpected response: ok=%v token=%q", result.OK, result.Token)
	}
	return &result, nil
}

// ── status command ────────────────────────────────────────────────────────────

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show locally tracked resources",
	Long: `Display all resources saved in ~/.instant-tokens.

Resources are saved automatically when you run:
  instant db new
  instant cache new
  instant nosql new
  instant queue new
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := tokens.Load()
		if err != nil {
			return fmt.Errorf("loading token store: %w", err)
		}

		if len(store.Entries) == 0 {
			fmt.Println("No resources found. Run `instant db new` or similar to get started.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TOKEN\tNAME\tSOURCE\tCREATED")
		for _, e := range store.Entries {
			shortToken := e.Token
			if len(shortToken) > 12 {
				shortToken = shortToken[:12] + "…"
			}
			created := e.CreatedAt.Format("2006-01-02")
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				shortToken, e.Name, e.Source, created)
		}
		w.Flush()
		return nil
	},
}

func init() {
	dbCmd.AddCommand(dbNewCmd)
	cacheCmd.AddCommand(cacheNewCmd)
	nosqlCmd.AddCommand(nosqlNewCmd)
	queueCmd.AddCommand(queueNewCmd)
	rootCmd.AddCommand(dbCmd)
	rootCmd.AddCommand(cacheCmd)
	rootCmd.AddCommand(nosqlCmd)
	rootCmd.AddCommand(queueCmd)
	rootCmd.AddCommand(statusCmd)
}
