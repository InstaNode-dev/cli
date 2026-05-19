package cmd

// up.go — `instant up` reconciles the resources declared in ./instant.yaml
// against the live API. Idempotent: existing resources matching by name+type
// are reused; missing ones are provisioned. Output is agent-friendly: each
// declared resource produces a single PROVISION/REUSE/ERROR line plus an
// `export NAME=URL` section ready to be sourced into a shell or .env file.
//
// Manifest shape (instant.yaml):
//
//   env: production            # optional, defaults to "production"
//   resources:
//     - type: postgres         # postgres | redis | mongodb | queue | storage | webhook
//       name: app-db           # human label, used as match key
//       export: DATABASE_URL   # env-var name to use when emitting export lines
//     - type: redis
//       name: app-cache
//       export: REDIS_URL
//
// Authentication:
//   Reads the bearer token (PAT or session JWT) from, in priority order:
//     1. INSTANT_TOKEN env var
//     2. ~/.instant-config (set by `instant login`)
//   Anonymous mode (no token) provisions anonymous-tier resources only.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// upManifest is the top-level shape of instant.yaml for `instant up`.
type upManifest struct {
	Env       string         `yaml:"env"`
	Resources []manifestRsrc `yaml:"resources"`
}

// manifestRsrc declares one resource to reconcile. `Export` is the env-var
// name the CLI uses when emitting `export <NAME>=<URL>` lines after a
// successful reconciliation.
type manifestRsrc struct {
	Type   string `yaml:"type"`
	Name   string `yaml:"name"`
	Export string `yaml:"export"`
}

// resourceListItem mirrors the items emitted by GET /api/v1/resources.
// Note: connection_url is NEVER in the list response (security). Use
// fetchCredentials(id) to retrieve plaintext.
type resourceListItem struct {
	ID           string `json:"id"`
	Token        string `json:"token"`
	ResourceType string `json:"resource_type"`
	Name         string `json:"name"`
	Env          string `json:"env"`
	Tier         string `json:"tier"`
}

// credentialsResponse mirrors GET /api/v1/resources/:id/credentials.
type credentialsResponse struct {
	OK            bool   `json:"ok"`
	ConnectionURL string `json:"connection_url"`
}

type resourceListResponse struct {
	OK    bool               `json:"ok"`
	Items []resourceListItem `json:"items"`
}

// upCmd is `instant up`.
var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Reconcile resources declared in ./instant.yaml against the API",
	Long: `Reconcile every resource declared in instant.yaml against your account.

For each resource:
  - if a resource of the same type+name already exists in the requested env,
    it is reused (no new provisioning, no extra cost)
  - otherwise a new resource is provisioned

Exit codes:
  0  every resource reconciled successfully
  1  manifest parse / file errors
  2  one or more resources failed to provision
  3  authentication required for the requested env (e.g. non-production),
     OR the saved session has expired and needs re-login

Examples:
  instant up
  instant up --env=staging
  instant up --file=path/to/instant.yaml
  instant up --emit-env > .env.local
`,
	RunE: runUp,
}

// flag values
var (
	upFile    string
	upEnv     string
	upEmitEnv bool
	upDryRun  bool
)

func init() {
	upCmd.Flags().StringVarP(&upFile, "file", "f", "instant.yaml", "Path to manifest")
	upCmd.Flags().StringVarP(&upEnv, "env", "e", "", "Override manifest env (production / staging / dev / ...)")
	upCmd.Flags().BoolVar(&upEmitEnv, "emit-env", false, "Print only `export KEY=URL` lines on stdout (suitable for `eval $(instant up --emit-env)`)")
	upCmd.Flags().BoolVar(&upDryRun, "dry-run", false, "Print the plan without provisioning")
	rootCmd.AddCommand(upCmd)
}

// runUp is the entrypoint for `instant up`.
func runUp(_ *cobra.Command, _ []string) error {
	manifest, err := readManifest(upFile)
	if err != nil {
		return err
	}
	env := strings.TrimSpace(manifest.Env)
	if upEnv != "" {
		env = upEnv
	}
	if env == "" {
		env = "production"
	}

	// Non-production env requires auth (server enforces this; we surface a
	// friendly hint locally so the founder doesn't waste a round trip).
	if env != "production" && !haveAuth() {
		return errAuthRequired(fmt.Sprintf(
			"env %q requires an INSTANT_TOKEN — run `instant login` or set INSTANT_TOKEN to a Personal Access Token",
			env))
	}

	// Print plan up front.
	if !upEmitEnv {
		fmt.Fprintf(os.Stderr, "instanode.dev — env=%s, %d resource(s)\n", env, len(manifest.Resources))
	}
	if upDryRun {
		for _, r := range manifest.Resources {
			fmt.Fprintf(os.Stderr, "  PLAN  %-9s %s\n", r.Type, r.Name)
		}
		return nil
	}

	// T16 P1-4 — Fetch current resources once. If the list-fetch fails for
	// ANY reason (401, 429, 5xx, network), we MUST abort — re-provisioning
	// blind would create duplicate resources, burn quota, and break the
	// idempotency contract `up` advertises.
	//
	// 401 is special: surface the uniform "session expired" error so an
	// agent script can branch on the same exit code everywhere.
	existing, listErr := fetchExistingResources(env)
	if listErr != nil {
		if errors.Is(listErr, errSessionExpiredSentinel) {
			return errSessionExpired()
		}
		return errResourceFailed(fmt.Errorf(
			"could not fetch existing resources (%w); refusing to provision blind. Retry or run `instant resources` to check status",
			listErr))
	}

	var hadErr bool
	for _, decl := range manifest.Resources {
		if err := decl.validate(); err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR %-9s %s: %v\n", decl.Type, decl.Name, err)
			hadErr = true
			continue
		}
		match := findExisting(existing, decl, env)
		if match != nil {
			url, err := fetchCredentials(match.Token)
			if err != nil {
				// Webhooks have no connection URL — fall back to a polite note.
				fmt.Fprintf(os.Stderr, "  REUSE     %-9s %s (%s) — credentials hidden: %v\n",
					decl.Type, decl.Name, shortToken(match.Token), err)
				continue
			}
			emit(decl, url, "REUSE", match.Token)
			continue
		}
		creds, err := provisionForUp(decl, env)
		if err != nil {
			if errors.Is(err, errSessionExpiredSentinel) {
				return errSessionExpired()
			}
			fmt.Fprintf(os.Stderr, "  ERROR %-9s %s: %v\n", decl.Type, decl.Name, err)
			hadErr = true
			continue
		}
		// /webhook/new returns receive_url, not connection_url.
		urlStr := creds.ConnectionURL
		if urlStr == "" {
			urlStr = creds.ReceiveURL
		}
		emit(decl, urlStr, "PROVISION", creds.Token)
	}

	if hadErr {
		return errResourceFailed(errors.New("one or more resources failed to reconcile"))
	}
	return nil
}

// readManifest parses ./instant.yaml.
func readManifest(path string) (*upManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var m upManifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(m.Resources) == 0 {
		return nil, fmt.Errorf("manifest %s declares no resources", path)
	}
	return &m, nil
}

// validate ensures the resource declaration is well-formed.
func (r manifestRsrc) validate() error {
	switch r.Type {
	case "postgres", "redis", "mongodb", "queue", "storage", "webhook":
		// ok
	default:
		return fmt.Errorf("unknown resource type %q (must be postgres|redis|mongodb|queue|storage|webhook)", r.Type)
	}
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("name is required")
	}
	return nil
}

// haveAuth reports whether the HTTPClient will send an Authorization header.
// True when INSTANT_TOKEN is set or `instant login` saved a token.
func haveAuth() bool {
	t, ok := HTTPClient.Transport.(*authTransport)
	if !ok {
		return false
	}
	if t.apiKey != "" {
		return true
	}
	return os.Getenv("INSTANT_TOKEN") != ""
}

// errSessionExpiredSentinel is a private marker error returned by the
// fetch helpers when the server returned 401 to an authenticated request.
// Callers translate this into the user-facing errSessionExpired() so the
// exit code stays uniform (3).
var errSessionExpiredSentinel = errors.New("session expired")

// fetchExistingResources returns the team's resources for the given env,
// or an error explaining why it couldn't determine the state. The CALLER
// must treat any non-nil error as fatal — we MUST NOT swallow errors and
// re-provision blind (T16 P1-4).
func fetchExistingResources(env string) ([]resourceListItem, error) {
	url := APIBaseURL + "/api/v1/resources?env=" + env
	resp, err := HTTPClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 401: unauthenticated. For anonymous callers this is expected (no
	// resources to reuse, no error); for callers with a token it means the
	// session is stale.
	if resp.StatusCode == http.StatusUnauthorized {
		if haveAuth() {
			return nil, errSessionExpiredSentinel
		}
		// Anonymous: not an error, just nothing to reuse.
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	raw, _ := io.ReadAll(resp.Body)
	var out resourceListResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parsing list response: %w", err)
	}
	return out.Items, nil
}

// findExisting returns the existing resource matching (type, name, env), or
// nil. Match key is (resource_type, name) — lowercase, trimmed.
func findExisting(items []resourceListItem, decl manifestRsrc, env string) *resourceListItem {
	wantType := strings.ToLower(decl.Type)
	wantName := strings.ToLower(strings.TrimSpace(decl.Name))
	for i := range items {
		it := &items[i]
		if strings.ToLower(it.ResourceType) != wantType {
			continue
		}
		if strings.ToLower(strings.TrimSpace(it.Name)) != wantName {
			continue
		}
		// env match: server stores empty for legacy / production
		if it.Env == "" && env == "production" {
			return it
		}
		if it.Env == env {
			return it
		}
	}
	return nil
}

// provisionForUp calls POST /{type}/new with name+env in the body.
func provisionForUp(decl manifestRsrc, env string) (*provisionResponse, error) {
	endpoint := "/" + map[string]string{
		"postgres": "db",
		"redis":    "cache",
		"mongodb":  "nosql",
		"queue":    "queue",
		"storage":  "storage",
		"webhook":  "webhook",
	}[decl.Type] + "/new"

	url := APIBaseURL + endpoint
	body, _ := json.Marshal(map[string]string{
		"name": decl.Name,
		"env":  env,
	})
	resp, err := HTTPClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized && haveAuth() {
		return nil, errSessionExpiredSentinel
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out provisionResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if !out.OK || out.Token == "" {
		return nil, fmt.Errorf("unexpected response: ok=%v token=%q", out.OK, out.Token)
	}
	return &out, nil
}

// emit writes an agent-friendly line for one resource. In --emit-env mode,
// only the export line is printed (so `eval $(instant up --emit-env)` works).
//
// T16 P1-5: shell-safe quoting — values are POSIX single-quoted (not
// Go-%q'd), and the export NAME is sanitized to a valid shell identifier.
// A name that sanitizes to empty is rejected (the `export = ...` line
// would be a shell syntax error).
func emit(decl manifestRsrc, url, action, token string) {
	exportName := decl.Export
	if exportName == "" {
		exportName = sanitizeExportName(decl.Name) + "_URL"
	}
	if !isValidShellIdentifier(exportName) {
		// Fallback: sanitize the user-supplied export name too.
		exportName = sanitizeExportName(exportName)
	}
	if !isValidShellIdentifier(exportName) {
		fmt.Fprintf(os.Stderr,
			"  ERROR %-9s %s: cannot derive a valid shell variable name from %q — set `export:` in manifest\n",
			decl.Type, decl.Name, decl.Name)
		return
	}
	if upEmitEnv {
		fmt.Printf("export %s=%s\n", exportName, shellQuote(url))
		return
	}
	short := token
	if len(short) > 8 {
		short = short[:8]
	}
	fmt.Fprintf(os.Stderr, "  %-9s %-9s %s (%s)\n", action, decl.Type, decl.Name, short)
	fmt.Printf("export %s=%s\n", exportName, shellQuote(url))
}

// truncate clamps a string for error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// fetchCredentials retrieves the plaintext connection URL for an existing
// resource via GET /api/v1/resources/:id/credentials. Used on REUSE so the
// CLI can re-emit the same .env contents on every run.
func fetchCredentials(id string) (string, error) {
	url := APIBaseURL + "/api/v1/resources/" + id + "/credentials"
	resp, err := HTTPClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var out credentialsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if out.ConnectionURL == "" {
		return "", errors.New("no connection_url in response")
	}
	return out.ConnectionURL, nil
}

// shortToken returns the first 8 chars of a token (or the whole token if shorter).
func shortToken(t string) string {
	if len(t) > 8 {
		return t[:8]
	}
	return t
}
