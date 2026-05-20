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

// resourcesFilter / resourcesLimit are the B15-P2 filter/pagination flags.
//
// --filter accepts repeated `key=value` pairs (key ∈ {type, env, status,
// tier, name}); rows must satisfy EVERY pair (logical AND). Unknown keys
// are rejected locally so a typo doesn't silently return the unfiltered
// full list (a worse footgun than a missing flag). Comparison is
// case-insensitive on the value.
//
// --limit caps the number of rows printed AFTER filtering (0 = no cap).
// The server's contract is unchanged — filtering is purely client-side so
// the CLI works against older API builds that don't support query params.
// A future server-side filter param would replace this implementation in
// place without changing the user-visible flag shape.
var (
	resourcesFilter []string
	resourcesLimit  int
)

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

Filtering and pagination:
  --filter <key>=<value>   (repeatable) keep rows matching EVERY pair
                           keys: type | env | status | tier | name
  --limit  N               cap to N rows after filtering (0 = no cap)

Examples:
  instant resources --filter type=postgres
  instant resources --filter env=production --filter tier=pro
  instant resources --limit 5 --json
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return wrapJSONErr(cmd, runResources(cmd))
	},
}

// allowedFilterKeys is the closed set of --filter keys. Adding a key here
// without surfacing it in --help is a contract violation (agents read
// --help to discover the surface), so help text above MUST be updated in
// the same edit.
var allowedFilterKeys = map[string]bool{
	"type":   true,
	"env":    true,
	"status": true,
	"tier":   true,
	"name":   true,
}

// runResources is split out from the cobra RunE closure so wrapJSONErr can
// intercept every error path (network, 401, 4xx envelope, 5xx) without
// duplicating the wrapper at each return site. B15-P0 (4).
func runResources(cmd *cobra.Command) error {
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

	// T16 P1-2 — uniform 401 handling.
	if resp.StatusCode == http.StatusUnauthorized {
		if haveAuth() {
			return errSessionExpired()
		}
		// In JSON mode the envelope is the only signal; skip the stderr
		// hint so a `--json | jq` pipeline isn't disturbed.
		if !jsonModeOn(cmd) {
			fmt.Fprintln(os.Stderr, "Not logged in. Run `instant login` first.")
		}
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
			Env          string `json:"env"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}

	// B15-P2 — apply --filter (AND across keys) and --limit client-side.
	// Done after the API call so older API builds that don't support
	// server-side filters still work. Filter parsing errors are local —
	// they fail before any rendering so the user can correct the typo.
	filters, err := parseResourceFilters(resourcesFilter)
	if err != nil {
		return err
	}
	if len(filters) > 0 {
		filtered := result.Items[:0]
		for _, r := range result.Items {
			if !matchResourceFilters(filters, r.ResourceType, r.Env, r.Status, r.Tier, r.Name) {
				continue
			}
			filtered = append(filtered, r)
		}
		result.Items = filtered
	}
	if resourcesLimit > 0 && len(result.Items) > resourcesLimit {
		result.Items = result.Items[:resourcesLimit]
	}

	// T16 P3 — machine-readable output. Stable schema, exit 0 with `[]`
	// when empty (an agent does not have to special-case the "no
	// resources" sentence).
	if resourcesJSON {
		// Force-init the empty-slice literal so the encoder emits `[]`,
		// not `null`, when filtering eliminates every row. Mirrors the
		// B15-P1 (9) status --json fix.
		if result.Items == nil {
			result.Items = result.Items[:0]
		}
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
}

// parseResourceFilters turns ["type=postgres", "env=production"] into a
// map[string]string keyed by canonical lowercase key. Returns a CLI-friendly
// error message (suitable to surface through wrapJSONErr) when a key is
// unknown or a pair is malformed. Used by `instant resources --filter`.
func parseResourceFilters(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		idx := -1
		for i := 0; i < len(p); i++ {
			if p[i] == '=' {
				idx = i
				break
			}
		}
		if idx <= 0 || idx == len(p)-1 {
			return nil, fmt.Errorf(
				"--filter %q is invalid: expected key=value (keys: type|env|status|tier|name)", p)
		}
		k := lower(p[:idx])
		v := p[idx+1:]
		if !allowedFilterKeys[k] {
			return nil, fmt.Errorf(
				"--filter key %q is not allowed (use one of: type, env, status, tier, name)", k)
		}
		out[k] = v
	}
	return out, nil
}

// matchResourceFilters returns true iff EVERY filter pair matches the row.
// Comparison is case-insensitive on the value. An unknown filter key is
// rejected by parseResourceFilters above, so we don't need a default arm.
func matchResourceFilters(filters map[string]string, rType, env, status, tier, name string) bool {
	for k, want := range filters {
		got := ""
		switch k {
		case "type":
			got = rType
		case "env":
			got = env
		case "status":
			got = status
		case "tier":
			got = tier
		case "name":
			got = name
		}
		if !eqFold(got, want) {
			return false
		}
	}
	return true
}

// lower / eqFold — tiny strings helpers kept local so this file does not
// re-import strings (the runResources path already does, but the filter
// helpers stay testable in isolation).
func lower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func eqFold(a, b string) bool { return lower(a) == lower(b) }

func init() {
	resourcesCmd.Flags().BoolVar(&resourcesJSON, "json", false,
		"Emit a JSON array of resources instead of a human-readable table")
	// B15-P2 — filter/pagination flags. --filter repeats for AND semantics.
	resourcesCmd.Flags().StringArrayVar(&resourcesFilter, "filter", nil,
		"Filter rows by key=value (repeatable). Keys: type | env | status | tier | name")
	resourcesCmd.Flags().IntVar(&resourcesLimit, "limit", 0,
		"Cap output to N rows after filtering (0 = no cap)")
	rootCmd.AddCommand(resourcesCmd)
}
