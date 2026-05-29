package cmd

// extras.go — B15 scope-gap commands.
//
// Adds the highest-leverage missing surface from the BugBash B15 audit:
//
//	instant storage new --name X       — provision object-storage bucket prefix
//	instant webhook new --name X       — provision a webhook receiver URL
//	instant vector  new --name X       — provision a Postgres + pgvector resource
//	instant resource <token>           — show detail for a single resource
//	instant resource delete <token>    — tear down a resource (--yes skips confirm)
//
// Deploy / stack flows are deliberately out of scope here — they need a
// multipart client that does not yet exist in cli/, and the BugBash brief
// explicitly carved them out for a separate effort.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/InstaNode-dev/cli/internal/tokens"
	"github.com/spf13/cobra"
)

// ── new resource groups ─────────────────────────────────────────────────────

var (
	storageCmd = newGroupCmd("storage", "Manage object-storage bucket prefix resources")
	webhookCmd = newGroupCmd("webhook", "Manage webhook receiver URLs")
	vectorCmd  = newGroupCmd("vector", "Manage Postgres + pgvector resources")
)

var storageNewCmd = &cobra.Command{
	Use:     "new --name <name>",
	Short:   "Provision an object-storage bucket prefix (S3-compatible)",
	Example: "  instant storage new --name app-blob",
	Args:    cobra.NoArgs,
	RunE:    makeProvisionCmd("/storage/new", "storage"),
}

var webhookNewCmd = &cobra.Command{
	Use:     "new --name <name>",
	Short:   "Provision a webhook receiver URL",
	Example: "  instant webhook new --name app-hook",
	Args:    cobra.NoArgs,
	RunE:    makeProvisionCmd("/webhook/new", "webhook"),
}

var vectorNewCmd = &cobra.Command{
	Use:     "new --name <name>",
	Short:   "Provision a Postgres + pgvector resource",
	Example: "  instant vector new --name app-vec",
	Args:    cobra.NoArgs,
	RunE:    makeProvisionCmd("/vector/new", "vector"),
}

// ── resource <token> / resource delete <token> ──────────────────────────────

// resourceCmd is a top-level command (NOT under `instant resources`, which
// stays the list view). `instant resource <token>` shows one resource;
// `instant resource delete <token>` tears it down.
var resourceCmd = &cobra.Command{
	Use:   "resource <token> | delete <token>",
	Short: "Show or delete a single resource by token",
	Long: `Show or delete a single resource by token.

  instant resource <token>             Print the resource's metadata + connection URL
  instant resource delete <token>      Tear down the resource. Requires --yes
                                       (or an interactive 'y' confirmation) to
                                       actually delete — printing nothing
                                       destructive when --yes is absent.

The token argument is the bearer token returned by 'instant <type> new' or
listed in 'instant resources'.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// `instant resource delete <token>` routes here via the same parent;
		// peel the verb off and dispatch. Done at RunE rather than as a real
		// sub-sub-command so the natural `instant resource <token>` (no
		// verb) reads as the detail view.
		if args[0] == "delete" {
			if len(args) < 2 {
				return wrapJSONErr(cmd, fmt.Errorf("instant resource delete: token argument is required"))
			}
			return wrapJSONErr(cmd, runResourceDelete(cmd, args[1]))
		}
		return wrapJSONErr(cmd, runResourceDetail(cmd, args[0]))
	},
}

// resourceDetailJSON / resourceDeleteYes are the flags shared across the
// resource subcommands. --json on either path emits the structured
// envelope; --yes on delete is the non-interactive opt-in.
var (
	resourceDetailJSON bool
	resourceDeleteYes  bool
)

// runResourceDetail GETs /api/v1/resources/:token and renders the result.
//
// CLI-MCP-11: this command requires auth — the token in the URL identifies
// the resource being inspected, NOT the caller. Pre-checking haveAuth() and
// short-circuiting with errAuthRequired (exit 3) keeps the exit-code
// contract consistent with `instant resources` (list), regardless of
// whether the API treats a path token as a bearer for some routes today.
// The post-call 401 branch below stays as defense-in-depth (covers expired
// PAT, server-side policy change, etc.).
func runResourceDetail(cmd *cobra.Command, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("token is required")
	}
	if !haveAuth() {
		return errAuthRequired("authentication required — run `instant login` first")
	}
	url := fmt.Sprintf("%s/api/v1/resources/%s", APIBaseURL, token)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		if haveAuth() {
			return errSessionExpired()
		}
		return errAuthRequired("authentication required — run `instant login` first")
	}
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return parseAPIError(resp.StatusCode, raw)
	}

	// The API may return the bare resource object OR an envelope:
	// {ok:true, resource:{...}}. Accept either shape.
	var envelope struct {
		OK       bool            `json:"ok"`
		Resource json.RawMessage `json:"resource"`
	}
	_ = json.Unmarshal(raw, &envelope)
	body := raw
	if len(envelope.Resource) > 0 {
		body = envelope.Resource
	}

	var detail struct {
		Token         string `json:"token"`
		ID            string `json:"id"`
		ResourceType  string `json:"resource_type"`
		Name          string `json:"name"`
		Env           string `json:"env"`
		Tier          string `json:"tier"`
		Status        string `json:"status"`
		ConnectionURL string `json:"connection_url"`
		ReceiveURL    string `json:"receive_url"`
		CreatedAt     string `json:"created_at"`
		ExpiresAt     string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if detail.Token == "" {
		detail.Token = token
	}

	if resourceDetailJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(detail)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "TOKEN\t%s\n", detail.Token)
	if detail.ID != "" {
		_, _ = fmt.Fprintf(w, "ID\t%s\n", detail.ID)
	}
	if detail.ResourceType != "" {
		_, _ = fmt.Fprintf(w, "TYPE\t%s\n", detail.ResourceType)
	}
	if detail.Name != "" {
		_, _ = fmt.Fprintf(w, "NAME\t%s\n", detail.Name)
	}
	if detail.Env != "" {
		_, _ = fmt.Fprintf(w, "ENV\t%s\n", detail.Env)
	}
	if detail.Tier != "" {
		_, _ = fmt.Fprintf(w, "TIER\t%s\n", detail.Tier)
	}
	if detail.Status != "" {
		_, _ = fmt.Fprintf(w, "STATUS\t%s\n", detail.Status)
	}
	if detail.ConnectionURL != "" {
		_, _ = fmt.Fprintf(w, "URL\t%s\n", detail.ConnectionURL)
	}
	if detail.ReceiveURL != "" {
		_, _ = fmt.Fprintf(w, "RECEIVE_URL\t%s\n", detail.ReceiveURL)
	}
	if detail.CreatedAt != "" {
		_, _ = fmt.Fprintf(w, "CREATED\t%s\n", detail.CreatedAt)
	}
	if detail.ExpiresAt != "" {
		_, _ = fmt.Fprintf(w, "EXPIRES\t%s\n", detail.ExpiresAt)
	}
	return w.Flush()
}

// runResourceDelete DELETEs /api/v1/resources/:token. Requires --yes (or a
// 'y' from an interactive terminal) to actually fire the request — destructive
// commands MUST NOT silently delete on a typo'd token.
//
// CLI-MCP-11 (paired with runResourceDetail): a destructive command without
// auth must exit 3 BEFORE any side effects (including the interactive
// confirmation prompt) — otherwise an unauth user can be coaxed into
// confirming a delete that then 401s with the wrong exit code.
func runResourceDelete(cmd *cobra.Command, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("token is required")
	}
	if !haveAuth() {
		return errAuthRequired("authentication required — run `instant login` first")
	}
	if !resourceDeleteYes {
		// Interactive confirmation when stdin is a TTY; otherwise abort
		// with a clear message so a piped invocation can't silently delete.
		fi, _ := os.Stdin.Stat()
		if (fi.Mode() & os.ModeCharDevice) == 0 {
			return fmt.Errorf("refusing to delete %s without --yes (stdin is not a TTY)", token)
		}
		fmt.Fprintf(os.Stderr, "About to DELETE resource %s. This is irreversible.\nType 'y' to confirm: ", token)
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(line)) != "y" {
			return fmt.Errorf("aborted")
		}
	}

	url := fmt.Sprintf("%s/api/v1/resources/%s", APIBaseURL, token)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		if haveAuth() {
			return errSessionExpired()
		}
		return errAuthRequired("authentication required — run `instant login` first")
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("resource %s not found", token)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted {
		return parseAPIError(resp.StatusCode, raw)
	}

	// Also remove the local token-store entry so `instant status` reflects
	// the deletion. Failures here are non-fatal — the server is the source
	// of truth.
	if store, err := tokens.Load(); err == nil {
		_ = store.Remove(token)
	}

	if resourceDetailJSON {
		out := map[string]any{
			"ok":      true,
			"deleted": token,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Printf("deleted %s\n", token)
	return nil
}

func init() {
	// storage / webhook / vector — same --name + --env plumbing as monitor.go.
	// CLI-MCP-8: --env is forwarded on every provisioning verb.
	for _, c := range []*cobra.Command{storageNewCmd, webhookNewCmd, vectorNewCmd} {
		c.Flags().StringVar(&resourceName, "name", "",
			"Resource name (required, 1–64 chars, matches ^[A-Za-z0-9][A-Za-z0-9 _-]*$)")
		c.Flags().StringVar(&resourceEnv, "env", "",
			"Provisioning environment (default: server-side \"development\"; common: development|staging|production)")
		_ = c.MarkFlagRequired("name")
	}
	storageCmd.AddCommand(storageNewCmd)
	webhookCmd.AddCommand(webhookNewCmd)
	vectorCmd.AddCommand(vectorNewCmd)
	rootCmd.AddCommand(storageCmd)
	rootCmd.AddCommand(webhookCmd)
	rootCmd.AddCommand(vectorCmd)

	resourceCmd.Flags().BoolVar(&resourceDetailJSON, "json", false,
		"Emit a JSON object instead of a human-readable summary")
	resourceCmd.Flags().BoolVar(&resourceDeleteYes, "yes", false,
		"Skip the interactive confirmation (required when stdin is not a TTY)")
	rootCmd.AddCommand(resourceCmd)
}
