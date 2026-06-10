package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

// provisionJSON is bound to the --json flag on every provisioning `new`
// command (db/cache/nosql/queue/storage/webhook/vector). B-provision-json:
// provisioning is the wedge action an agent most needs machine-readable — it
// must capture the returned token + connection_url programmatically. Before
// this flag, `instant db new --name x --json` failed with `unknown flag:
// --json` and an agent had to scrape the human-readable lines. Matches the
// --json convention already on resources/status/whoami/resource.
var provisionJSON bool

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
//	instant db delete <id>   → prints help, exits 0
//
// which silently hid typo bugs in agent scripts and let `... | xargs instant`
// pipelines look successful. The pattern below combines:
//
//  1. Args: cobra.NoArgs            — refuses any positional arg
//  2. RunE: showGroupHelp           — when called with zero args, shows
//     help and exits 0 (the legacy path)
//  3. cobra's built-in "did you mean?" suggestions surface for typos that
//     are within 2 edits of a valid subcommand (cobra default).
//
// Together, `instant db delete <id>` now errors with:
//
//	Error: unknown command "delete" for "instant db"
//	Run 'instant db --help' for usage.
//
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
			// B-provision-json: in --json mode, errors funnel through the
			// shared envelope so an agent piping into jq never crashes on a
			// 402/429/network failure.
			return wrapJSONErr(cmd, fmt.Errorf("provisioning failed: %w", err))
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

		// CLI-MCP-8: surface the resolved env (and env_override_reason when
		// the server downgraded the request — e.g. anonymous caller asking
		// for production gets demoted to development with a reason string).
		// Empty `creds.Env` against an older API build still prints the
		// "development" fallback used for the local tokens cache above.
		envOut := creds.Env
		if envOut == "" {
			envOut = "development"
		}

		// B-provision-json: emit the full structured response so an agent can
		// capture token + connection_url + environment in one machine-readable
		// blob. Matches the two-space-indent convention of every other --json
		// command. The resolved env is echoed under both `env` (raw server
		// field) and `environment` (the /deploy/new-style alias) so an agent
		// keys off whichever it already uses.
		if provisionJSON {
			return emitProvisionJSON(resourceType, creds, envOut)
		}

		fmt.Printf("ok    %-8s  %s\n", resourceType, creds.Token)
		// F2: /webhook/new returns receive_url (NOT connection_url), so a
		// bare creds.ConnectionURL printed `url   ` (blank). Fall back to
		// ReceiveURL like the local token-store code above already does, so
		// webhook provisions show their real receiver URL. (--json already
		// emits both fields via emitProvisionJSON.)
		provisionURL := creds.ConnectionURL
		if provisionURL == "" {
			provisionURL = creds.ReceiveURL
		}
		fmt.Printf("url   %s\n", provisionURL)
		if creds.Tier != "" {
			fmt.Printf("tier  %s\n", creds.Tier)
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

// provisionJSONOutput is the stable schema emitted by every provisioning verb
// under --json. It surfaces the full success response an agent needs to wire
// up the resource: the token (for later `instant resource …` calls), the
// connection_url (or receive_url for webhooks), the resolved environment, and
// the tier/note/override metadata the human path already prints.
type provisionJSONOutput struct {
	OK                bool   `json:"ok"`
	ResourceType      string `json:"resource_type"`
	Token             string `json:"token"`
	Name              string `json:"name"`
	ConnectionURL     string `json:"connection_url,omitempty"`
	ReceiveURL        string `json:"receive_url,omitempty"`
	Tier              string `json:"tier,omitempty"`
	Env               string `json:"env"`
	Environment       string `json:"environment"`
	EnvOverrideReason string `json:"env_override_reason,omitempty"`
	Note              string `json:"note,omitempty"`
}

// emitProvisionJSON writes the structured provisioning result to stdout with
// the shared two-space indentation. resolvedEnv is the env after the empty →
// "development" fallback so the JSON never reports an empty environment.
func emitProvisionJSON(resourceType string, creds *provisionResponse, resolvedEnv string) error {
	out := provisionJSONOutput{
		OK:                true,
		ResourceType:      resourceType,
		Token:             creds.Token,
		Name:              creds.Name,
		ConnectionURL:     creds.ConnectionURL,
		ReceiveURL:        creds.ReceiveURL,
		Tier:              creds.Tier,
		Env:               resolvedEnv,
		Environment:       resolvedEnv,
		EnvOverrideReason: creds.EnvOverrideReason,
		Note:              creds.Note,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
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
		// A provision that hit the client/context deadline may have ALREADY
		// landed server-side (provisioning is synchronous on the api) — exit 1
		// alone hides a possible orphan. Surface actionable durability guidance
		// while preserving the underlying cause for %w-aware callers.
		if isTimeoutErr(err) {
			return nil, fmt.Errorf("%s (%w)", provisionTimeoutGuidance, err)
		}
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

// isTimeoutErr reports whether err is a request timeout — either the
// http.Client.Timeout firing (surfaces as a net.Error with Timeout()==true,
// wrapped in a *url.Error) or a context deadline being exceeded. Both mean the
// provision request was abandoned client-side, so the resource may still be
// landing server-side. Kept separate from json_error.go's network classifier
// because that path emits a generic "network_error"; here we want the orphan
// durability hint specifically on the timeout case.
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
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
		// B-provision-json: emit the full structured response (token,
		// connection_url, environment, …) instead of the human-readable lines.
		c.Flags().BoolVar(&provisionJSON, "json", false,
			"Emit the provisioning result as a JSON object instead of human-readable lines")
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
