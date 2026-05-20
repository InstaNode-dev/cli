package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"text/tabwriter"

	"github.com/instant-dev/cli/internal/tokens"
	"github.com/spf13/cobra"
)

// ── Provisioning subcommand groups ───────────────────────────────────────────
// instant db new --name <name>
// instant cache new --name <name>
// instant nosql new --name <name>
// instant queue new --name <name>
//
// The resource `name` is REQUIRED on every provisioning endpoint. The server
// enforces 1–64 chars matching nameRegexp and rejects an omitted name with
// HTTP 400; the CLI marks --name required so the error surfaces locally
// before any API round trip.

// nameMaxLen and nameRegexp mirror the server-side resource-name contract
// (1–64 chars, must start with an alphanumeric character).
const nameMaxLen = 64

var nameRegexp = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]*$`)

// resourceName is bound to the required --name flag on every `new` command.
var resourceName string

// validateResourceName applies the server-side name contract locally so the
// CLI fails fast with a clear message instead of a bare HTTP 400.
func validateResourceName(name string) error {
	if name == "" {
		return fmt.Errorf("--name is required")
	}
	if len(name) > nameMaxLen {
		return fmt.Errorf("--name must be 1–%d characters (got %d)", nameMaxLen, len(name))
	}
	if !nameRegexp.MatchString(name) {
		return fmt.Errorf("--name %q is invalid: must match %s", name, nameRegexp.String())
	}
	return nil
}

var (
	dbCmd    = &cobra.Command{Use: "db", Short: "Manage Postgres database resources"}
	cacheCmd = &cobra.Command{Use: "cache", Short: "Manage Redis cache resources"}
	nosqlCmd = &cobra.Command{Use: "nosql", Short: "Manage MongoDB document-store resources"}
	queueCmd = &cobra.Command{Use: "queue", Short: "Manage NATS JetStream queue resources"}
)

var dbNewCmd = &cobra.Command{
	Use:     "new --name <name>",
	Short:   "Provision a Postgres database (+ pgvector)",
	Example: "  instant db new --name app-db",
	Args:    cobra.NoArgs,
	RunE:    makeProvisionCmd("/db/new", "db"),
}

var cacheNewCmd = &cobra.Command{
	Use:     "new --name <name>",
	Short:   "Provision a Redis cache",
	Example: "  instant cache new --name app-cache",
	Args:    cobra.NoArgs,
	RunE:    makeProvisionCmd("/cache/new", "cache"),
}

var nosqlNewCmd = &cobra.Command{
	Use:     "new --name <name>",
	Short:   "Provision a MongoDB document store",
	Example: "  instant nosql new --name app-docs",
	Args:    cobra.NoArgs,
	RunE:    makeProvisionCmd("/nosql/new", "nosql"),
}

var queueNewCmd = &cobra.Command{
	Use:     "new --name <name>",
	Short:   "Provision a NATS JetStream queue",
	Example: "  instant queue new --name app-jobs",
	Args:    cobra.NoArgs,
	RunE:    makeProvisionCmd("/queue/new", "queue"),
}

// makeProvisionCmd returns a RunE function that POSTs to the given endpoint
// and prints the provisioned connection URL. The resource name comes from the
// required --name flag.
func makeProvisionCmd(endpoint, resourceType string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		name := resourceName
		if err := validateResourceName(name); err != nil {
			return err
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
// /webhook/new returns receive_url instead of connection_url.
type provisionResponse struct {
	OK            bool   `json:"ok"`
	Token         string `json:"token"`
	Name          string `json:"name"`
	ConnectionURL string `json:"connection_url"`
	ReceiveURL    string `json:"receive_url"`
	Tier          string `json:"tier"`
	Note          string `json:"note"`
	Upgrade       string `json:"upgrade"`
}

// provisionResource calls POST {APIBaseURL}{endpoint} and returns parsed credentials.
//
// T16 P1-2: a 401 against an authenticated request returns the uniform
// errSessionExpired() error so the exit-code contract is consistent across
// `resources`, `up`, and direct provisioning.
func provisionResource(endpoint, name string) (*provisionResponse, error) {
	url := APIBaseURL + endpoint
	body, _ := json.Marshal(map[string]string{"name": name})

	resp, err := HTTPClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized && haveAuth() {
		return nil, errSessionExpired()
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// T16 P2-1 — surface the structured error envelope
		// ({message, agent_action, upgrade_url, ...}) rather than
		// dumping the raw JSON blob at the user.
		return nil, parseAPIError(resp.StatusCode, raw)
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

// statusJSON is the --json flag for `instant status`. T16 P3: machine-readable
// output for agents.
var statusJSON bool

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show locally tracked resources",
	Long: `Display all resources saved in ~/.instant-tokens.

Resources are saved automatically when you run:
  instant db new --name <name>
  instant cache new --name <name>
  instant nosql new --name <name>
  instant queue new --name <name>

With --json, output is a machine-readable JSON array of token entries
({token, name, url, source, created_at}).
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := tokens.Load()
		if err != nil {
			return fmt.Errorf("loading token store: %w", err)
		}

		// T16 P3 — machine-readable output. Empty list emits `[]`.
		if statusJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(store.Entries)
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
	// --name is REQUIRED on every provisioning command. Cobra surfaces a
	// clear `required flag(s) "name" not set` error before RunE runs.
	for _, c := range []*cobra.Command{dbNewCmd, cacheNewCmd, nosqlNewCmd, queueNewCmd} {
		c.Flags().StringVar(&resourceName, "name", "", "Resource name (required, 1–64 chars, matches ^[A-Za-z0-9][A-Za-z0-9 _-]*$)")
		_ = c.MarkFlagRequired("name")
	}

	dbCmd.AddCommand(dbNewCmd)
	cacheCmd.AddCommand(cacheNewCmd)
	nosqlCmd.AddCommand(nosqlNewCmd)
	queueCmd.AddCommand(queueNewCmd)
	rootCmd.AddCommand(dbCmd)
	rootCmd.AddCommand(cacheCmd)
	rootCmd.AddCommand(nosqlCmd)
	rootCmd.AddCommand(queueCmd)

	statusCmd.Flags().BoolVar(&statusJSON, "json", false,
		"Emit a JSON array of local token entries instead of a human-readable table")
	rootCmd.AddCommand(statusCmd)
}
