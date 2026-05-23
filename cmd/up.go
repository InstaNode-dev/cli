package cmd

// up.go — `instant up` reconciles the resources declared in ./instant.yaml
// against the live API. Idempotent: existing resources matching by name+type
// are reused; missing ones are provisioned. Output is agent-friendly: each
// declared resource produces a single PROVISION/REUSE/ERROR line plus an
// `export NAME=URL` section ready to be sourced into a shell or .env file.
//
// Manifest shape (instant.yaml):
//
//   env: development           # optional, defaults to "development"
//                              # (matches the platform contract — CLAUDE.md
//                              # rule 11 / migration 026)
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
//
// Env default (T16 P2-3):
//   When neither the manifest nor --env specifies an env, `up` lands resources
//   in `development` — the lowest-stakes bucket. This matches the api's
//   `env=development` default (CLAUDE.md rule 11) so the CLI never silently
//   targets production when the manifest omits `env`. Set `env: production`
//   explicitly in instant.yaml — or pass `--env=production` — to ship live.
//   The platform always requires auth for any env other than the anonymous
//   default, so a missing token + non-default env fails fast locally.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/InstaNode-dev/cli/internal/tokens"
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

Env resolution (in priority order):
  1. --env flag
  2. manifest top-level "env:" field
  3. INSTANT_ENV environment variable
  4. default: "development" — matches the platform's lowest-stakes default
     (CLAUDE.md rule 11 / migration 026). Pass --env=production explicitly
     to ship live.

Exit codes:
  0  every resource reconciled successfully
  1  manifest parse / file errors
  2  one or more resources failed to provision
  3  authentication required for the requested env (any non-default env),
     OR the saved session has expired and needs re-login

Examples:
  instant up                          # development env (default)
  instant up --env=production         # ship live (requires auth)
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

// upDefaultEnv is the env `up` falls back to when the manifest, --env flag,
// and INSTANT_ENV are all empty. T16 P2-3: matches the platform default
// (CLAUDE.md rule 11 — migration 026) so an omitted `env:` cannot silently
// target production. Override explicitly via --env=production to ship live.
const upDefaultEnv = "development"

// runUp is the entrypoint for `instant up`.
func runUp(_ *cobra.Command, _ []string) error {
	manifest, err := readManifest(upFile)
	if err != nil {
		return err
	}
	// T16 P2-3 — env resolution: --env > manifest.env > $INSTANT_ENV > default.
	// Default is "development" (not "production"); see upDefaultEnv.
	env := strings.TrimSpace(manifest.Env)
	if upEnv != "" {
		env = upEnv
	}
	if env == "" {
		env = strings.TrimSpace(os.Getenv("INSTANT_ENV"))
	}
	if env == "" {
		env = upDefaultEnv
	}

	// Any non-default env requires auth (server enforces this; we surface a
	// friendly hint locally so the founder doesn't waste a round trip).
	// `development` is the safe local-iteration default and is the ONLY env
	// that can be reconciled anonymously.
	if env != upDefaultEnv && !haveAuth() {
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

	// B15-P1 (7) — anonymous-`up` idempotency. Anonymous callers can't
	// authenticate to GET /api/v1/resources so `existing` is nil. Without a
	// fallback, the 2nd `instant up --emit-env` call re-POSTs every resource
	// — which 429s after 5 calls and breaks `eval $(instant up --emit-env)`.
	// We load ~/.instant-tokens (populated on each provision in monitor.go's
	// makeProvisionCmd + below in runUp's "PROVISION" branch) and use it as
	// a (type, name, env) lookup. This makes anon-up genuinely idempotent
	// on the same machine. A different machine still pays the re-provision
	// cost — but the README claim of "`up` is idempotent" now holds on the
	// machine that ran `up` once. Authenticated callers still prefer the
	// authoritative API list (anonCache only fills the gap when existing
	// is nil/empty AND we're anonymous).
	var anonCache *tokens.Store
	if !haveAuth() && len(existing) == 0 {
		anonCache, _ = tokens.Load() // best-effort; nil cache is fine
	}

	var hadErr bool
	for _, decl := range manifest.Resources {
		if err := decl.validate(); err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR %-9s %s: %v\n", decl.Type, decl.Name, err)
			hadErr = true
			continue
		}
		// First check the live server list; if anonymous and unauthed, also
		// peek at the local cache. The server list is authoritative — only
		// fall back to anonCache when the API view is empty.
		match := findExisting(existing, decl, env)
		if match == nil && anonCache != nil {
			if cached := anonCache.FindByTypeNameEnv(apiResourceType(decl.Type), decl.Name, env); cached != nil {
				// Synthesize a resourceListItem so the reuse branch below
				// works unchanged. The cached URL is sufficient for emit;
				// no /credentials round trip needed (which would 401 for
				// anon anyway).
				match = &resourceListItem{
					Token:        cached.Token,
					ResourceType: apiResourceType(decl.Type),
					Name:         cached.Name,
					Env:          cached.Env,
					Tier:         "anonymous",
				}
				// Short-circuit: emit straight from the cache and skip the
				// /credentials call (the loop below would attempt and fail).
				url := cached.URL
				if url == "" && strings.EqualFold(decl.Type, "webhook") {
					url = webhookReceiveURL(cached.Token)
				}
				if url == "" {
					// Cache row without a URL — fall through to the normal
					// reuse path so the credentials endpoint can try.
				} else {
					emit(decl, url, "REUSE", cached.Token)
					continue
				}
			}
		}
		if match != nil {
			// T16 P2-4 — webhook REUSE must still emit a stable
			// `export NAME=...` line (otherwise the second `up --emit-env`
			// run silently drops the webhook variable and breaks .env).
			// The receive URL is deterministic — base + token — so we can
			// reconstruct it without a round trip rather than depending on
			// the credentials endpoint that returns 404 for older API
			// builds / non-paid tiers.
			if strings.EqualFold(match.ResourceType, "webhook") {
				url := webhookReceiveURL(match.Token)
				emit(decl, url, "REUSE", match.Token)
				continue
			}
			url, err := fetchCredentials(match.Token)
			if err != nil {
				// Non-webhook resource with hidden credentials — surface a
				// clear note but do NOT silently emit a broken/empty line.
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
		// B15-P1 (7) — persist the freshly-provisioned token into the local
		// store, keyed by (type, name, env), so the NEXT `instant up` run
		// can recognize it without an API round trip (critical for the
		// anonymous quick-start path that can't call GET /api/v1/resources).
		// Errors are non-fatal: the resource is real on the server and a
		// failed local-write only costs us idempotency, not correctness.
		if store, loadErr := tokens.Load(); loadErr == nil {
			cacheEnv := creds.Env
			if cacheEnv == "" {
				cacheEnv = env
			}
			_ = store.Add(tokens.Entry{
				Token:  creds.Token,
				Name:   decl.Name,
				Type:   apiResourceType(decl.Type),
				Env:    cacheEnv,
				URL:    urlStr,
				Source: "up",
			})
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
// True when --token, INSTANT_TOKEN, or `instant login` configured a token.
// B15-P1: TrimSpace so whitespace-only values don't read as "authed".
// B15-P2: --token global flag is also honored (mirrors initConfig precedence).
func haveAuth() bool {
	t, ok := HTTPClient.Transport.(*authTransport)
	if !ok {
		return false
	}
	if t.apiKey != "" {
		return true
	}
	if strings.TrimSpace(adHocToken) != "" {
		return true
	}
	return strings.TrimSpace(os.Getenv("INSTANT_TOKEN")) != ""
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
	defer func() { _ = resp.Body.Close() }()

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
		// T16 P2-1 — surface structured agent_action / upgrade hints.
		return nil, parseAPIError(resp.StatusCode, raw)
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
		// env match: server stores empty for legacy / production, and
		// post-migration-026 stores "development" for env-less provisions.
		// Both must match when the caller asks for the new platform default.
		if it.Env == "" && (env == "production" || env == upDefaultEnv) {
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
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized && haveAuth() {
		return nil, errSessionExpiredSentinel
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// T16 P2-1 — surface agent_action / upgrade hints on provision-time
		// errors (402 quota, 429 rate-limit, 5xx, etc.) instead of dumping
		// the raw JSON body at the user.
		return nil, parseAPIError(resp.StatusCode, raw)
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
// resource via GET /api/v1/resources/:id/credentials.
//
// T16 P2-5: the URL path parameter `:id` is the resource's **TOKEN (UUID)** —
// NOT the database row id. This is the api's documented contract
// (api/internal/handlers/resource.go GetCredentials uses
// `uuid.Parse(c.Params("id"))` and looks up by token, not by row id). The
// list endpoint returns BOTH `id` and `token`, so callers MUST pass `token`
// here, never `id`. Misusing `id` would 400 ("invalid_id" — not a UUID) or
// 404. Callers in this file go through `match.Token` from resourceListItem.
func fetchCredentials(token string) (string, error) {
	url := APIBaseURL + "/api/v1/resources/" + token + "/credentials"
	resp, err := HTTPClient.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
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

// webhookReceiveURL reconstructs the canonical webhook receive URL from the
// stored token. The shape `<API_BASE>/webhook/receive/<token>` is fixed by
// the api (api/internal/handlers/webhook.go: `receiveURL(baseURL, token)`)
// and never changes per-token, so the CLI can derive it locally without a
// credentials round trip. T16 P2-4 uses this on REUSE so `up --emit-env`
// produces a stable export line for webhook resources on every run.
func webhookReceiveURL(token string) string {
	base := strings.TrimRight(APIBaseURL, "/")
	return base + "/webhook/receive/" + token
}

// apiResourceType maps a manifest type string (postgres|redis|mongodb|queue|
// storage|webhook) to the api's resource_type field. The mapping is identity
// for all current types — the api stores resource_type using the same
// strings — but we route through this helper so a future divergence
// (e.g. manifest `mongo` → api `mongodb`) is a single-site fix rather than
// scattered string-literal edits. B15-P1 (7) uses this to key the local
// tokens cache and to synthesize resourceListItem entries when the live
// server list is unavailable to anonymous callers.
func apiResourceType(manifestType string) string {
	switch strings.ToLower(strings.TrimSpace(manifestType)) {
	case "postgres", "redis", "mongodb", "queue", "storage", "webhook", "vector":
		return strings.ToLower(strings.TrimSpace(manifestType))
	}
	return strings.ToLower(strings.TrimSpace(manifestType))
}
