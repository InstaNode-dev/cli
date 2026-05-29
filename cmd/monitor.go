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

	"github.com/InstaNode-dev/cli/internal/tokens"
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

// resourceEnv is bound to the optional --env flag on every `new` command.
//
// CLI-MCP-8 (BugBash QA round 2): every provisioning verb on the CLI used to
// drop `env` on the request body. The API has honored an `env` parameter
// since migration 026 (defaults to "development" when omitted — CLAUDE.md
// rule 11). Without a CLI surface, an agent had no way to provision into
// "production" without falling back to curl. Empty here means "don't send
// the field" — the server applies its documented default (development).
// Values are not validated client-side; the server enforces the regex +
// policy and surfaces a structured 400 if invalid, so we don't second-guess
// it (this also keeps the CLI forward-compatible with future env-policy
// changes).
var resourceEnv string

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

// B15-P0 (3) — every resource-group command (db / cache / nosql / queue /
// storage / webhook / vector) MUST reject unknown sub-sub-commands with a
// non-zero exit. The previous behaviour was:
//
//   instant db delete <id>   → prints help, exits 0
//
// which silently hid typo bugs in agent scripts and let `... | xargs instant`
// pipelines look successful. The pattern below combines:
//
//   1. Args: cobra.NoArgs            — refuses any positional arg
//   2. RunE: showGroupHelp           — when called with zero args, shows
//                                      help and exits 0 (the legacy path)
//   3. cobra's built-in "did you mean?" suggestions surface for typos that
//      are within 2 edits of a valid subcommand (cobra default).
//
// Together, `instant db delete <id>` now errors with:
//   Error: unknown command "delete" for "instant db"
//   Run 'instant db --help' for usage.
// and exits 1.
func showGroupHelp(cmd *cobra.Command, args []string) error {
	return cmd.Help()
}

func newGroupCmd(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		// Args: NoArgs — any positional arg that isn't a registered
		// sub-sub-command name surfaces cobra's "unknown command" error.
		Args: cobra.NoArgs,
		// RunE fires only when zero args reach the parent — i.e.
		// `instant db` with no subcommand. Print help, exit 0.
		RunE: showGroupHelp,
	}
}

var (
	dbCmd    = newGroupCmd("db", "Manage Postgres database resources")
	cacheCmd = newGroupCmd("cache", "Manage Redis cache resources")
	nosqlCmd = newGroupCmd("nosql", "Manage MongoDB document-store resources")
	queueCmd = newGroupCmd("queue", "Manage NATS JetStream queue resources")
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

		creds, err := provisionResource(endpoint, name, resourceEnv)
		if err != nil {
			return fmt.Errorf("provisioning failed: %w", err)
		}

		// Save token locally for `instant status` + B15-P1 (7) anon-up
		// idempotency. Type+Env are required so anonymous `up` can match
		// (type, name, env) on subsequent runs without an API list call.
		// Env is populated from the resolved provision env when the server
		// echoed it back (api ≥ 2026-05-13 / migration 026); falls back to
		// "development" — the platform default — when omitted.
		if store, loadErr := tokens.Load(); loadErr == nil {
			urlOrReceive := creds.ConnectionURL
			if urlOrReceive == "" {
				urlOrReceive = creds.ReceiveURL
			}
			env := creds.Env
			if env == "" {
				env = "development"
			}
			_ = store.Add(tokens.Entry{
				Token:  creds.Token,
				Name:   creds.Name,
				Type:   resourceType,
				Env:    env,
				URL:    urlOrReceive,
				Source: "provision",
			})
		}

		fmt.Printf("ok    %-8s  %s\n", resourceType, creds.Token)
		fmt.Printf("url   %s\n", creds.ConnectionURL)
		if creds.Tier != "" {
			fmt.Printf("tier  %s\n", creds.Tier)
		}
		// CLI-MCP-8: surface the resolved env (and env_override_reason when
		// the server downgraded the request — e.g. anonymous caller asking
		// for production gets demoted to development with a reason string).
		// Empty `creds.Env` against an older API build still prints the
		// "development" fallback used for the local tokens cache above.
		envOut := creds.Env
		if envOut == "" {
			envOut = "development"
		}
		fmt.Printf("env   %s\n", envOut)
		if creds.EnvOverrideReason != "" {
			fmt.Printf("env_override_reason  %s\n", creds.EnvOverrideReason)
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
	// Env is the resolved provisioning environment the server echoed back
	// (api ≥ 2026-05-13 / migration 026). May be empty against older builds;
	// callers default to "development" — the platform's lowest-stakes
	// default (CLAUDE.md rule 11) — when empty. Used to key the local
	// tokens cache so B15-P1 (7) anonymous-up idempotency can match on
	// (type, name, env) without an API list call.
	Env string `json:"env"`
	// EnvOverrideReason is set by the API when the requested env was
	// downgraded server-side (e.g. anonymous caller asking for production
	// gets demoted to "development" with a reason). CLI surfaces it so the
	// user sees WHY their requested env didn't stick. May be empty.
	EnvOverrideReason string `json:"env_override_reason"`
	Note              string `json:"note"`
	Upgrade           string `json:"upgrade"`
}

// provisionResource calls POST {APIBaseURL}{endpoint} and returns parsed credentials.
//
// T16 P1-2: a 401 against an authenticated request returns the uniform
// errSessionExpired() error so the exit-code contract is consistent across
// `resources`, `up`, and direct provisioning.
//
// CLI-MCP-8: `env` is the optional `--env` flag. Empty == "don't send the
// field" so the server applies its documented default (development). A
// non-empty value is forwarded verbatim; server-side validation owns the
// regex + policy.
func provisionResource(endpoint, name, env string) (*provisionResponse, error) {
	url := APIBaseURL + endpoint
	payload := map[string]string{"name": name}
	if env != "" {
		payload["env"] = env
	}
	body, _ := json.Marshal(payload)

	resp, err := HTTPClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

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
			return wrapJSONErr(cmd, fmt.Errorf("loading token store: %w", err))
		}

		// T16 P3 — machine-readable output. Empty list emits `[]`.
		//
		// B15-P1 (9) — store.Entries is a nil slice when ~/.instant-tokens
		// has never been written; json.Encoder serializes a nil []T as
		// `null`, which crashes `instant status --json | jq '.[] | …'`.
		// Force the empty-slice literal so agents can pipe the output
		// unconditionally and `resources --json` / `status --json` share
		// the same `[]` shape on empty stores.
		if statusJSON {
			entries := store.Entries
			if entries == nil {
				entries = []tokens.Entry{}
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(entries)
		}

		if len(store.Entries) == 0 {
			fmt.Println("No resources found. Run `instant db new` or similar to get started.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "TOKEN\tNAME\tSOURCE\tCREATED")
		for _, e := range store.Entries {
			shortToken := e.Token
			if len(shortToken) > 12 {
				shortToken = shortToken[:12] + "…"
			}
			created := e.CreatedAt.Format("2006-01-02")
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				shortToken, e.Name, e.Source, created)
		}
		_ = w.Flush()
		return nil
	},
}

func init() {
	// --name is REQUIRED on every provisioning command. Cobra surfaces a
	// clear `required flag(s) "name" not set` error before RunE runs.
	// --env is OPTIONAL (CLI-MCP-8). Empty == server default ("development",
	// CLAUDE.md rule 11); set to "production" / "staging" / etc. to override.
	for _, c := range []*cobra.Command{dbNewCmd, cacheNewCmd, nosqlNewCmd, queueNewCmd} {
		c.Flags().StringVar(&resourceName, "name", "", "Resource name (required, 1–64 chars, matches ^[A-Za-z0-9][A-Za-z0-9 _-]*$)")
		c.Flags().StringVar(&resourceEnv, "env", "",
			"Provisioning environment (default: server-side \"development\"; common: development|staging|production)")
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
