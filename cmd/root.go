package cmd

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/instant-dev/cli/internal/cliconfig"
	"github.com/instant-dev/cli/internal/secretstore"
)

var _ = httpListTimeout // documented constant; referenced in tests / future refactor

// APIBaseURL is the instanode.dev API base URL.
// Resolved at init from (in priority order):
//  1. INSTANT_API_URL env var
//  2. ~/.instant-config api_base_url
//  3. Default: https://api.instanode.dev
var APIBaseURL = "https://api.instanode.dev"

// authTransport adds the Authorization header to every request when the user
// is logged in. Anonymous requests are sent without a header.
type authTransport struct {
	base   http.RoundTripper
	apiKey string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.apiKey != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	return t.base.RoundTrip(req)
}

// httpListTimeout is the default per-request timeout for read-only API calls
// (list, get, auth/me). Short — these never call into provisioning latency
// and a stuck server should fail-fast.
const httpListTimeout = 10 * time.Second

// httpProvisionTimeout is the per-request timeout for synchronous provisioning
// and reconcile calls. T16 P2-2: a 10s client timeout used to abort real
// `db new` / `cache new` / `nosql new` requests under load and leave an
// orphan resource on the server (provisioning is synchronous on the API:
// CREATE DATABASE / CREATE USER / pool warm-up against shared infra can
// legitimately exceed 10s). 60s matches the api's documented provisioning
// budget and gives the agent a chance to receive the token before the
// timer fires.
//
// Operators can override via INSTANT_TIMEOUT_SECONDS (uses int seconds for
// CLI simplicity; <=0 falls back to the default).
const httpProvisionTimeout = 60 * time.Second

// HTTPClient is the shared HTTP client used by all subcommands.
// It is configured with the auth transport during init.
//
// IMPORTANT: HTTPClient's Timeout is set to httpProvisionTimeout because
// provisioning + reconcile use this client. The few read-only `resources`
// and auth-poll paths are still safe because their requests complete in
// milliseconds; the longer ceiling is harmless. We keep one client (rather
// than two) to preserve the auth transport wiring through cobra OnInitialize.
var HTTPClient = &http.Client{Timeout: httpProvisionTimeout}

var rootCmd = &cobra.Command{
	Use:   "instant",
	Short: "instanode.dev CLI — zero-friction developer infrastructure",
	Long: `instanode.dev CLI

Provision databases, caches, queues, and document stores with a single command.
No account required to get started. Log in with 'instant login' to persist resources.

Every provisioning command requires a --name flag (1–64 chars).

Examples:
  instant db new --name app-db        Provision a Postgres database (+ pgvector)
  instant cache new --name app-cache  Provision a Redis cache
  instant nosql new --name app-docs   Provision a MongoDB document store
  instant queue new --name app-jobs   Provision a NATS JetStream queue
  instant resources                   List your provisioned resources (requires login)
  instant status                      Show locally tracked resources
  instant login                       Log in to your instanode.dev account
  instant logout                      Remove locally saved credentials
  instant whoami                      Show current account
  instant upgrade                     Open the upgrade page
`,
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)
}

func initConfig() {
	// Wire up the secret backend BEFORE loading cliconfig (cliconfig.Load
	// reads the bearer token via secretstore.Get). On test runs HOME points
	// to a temp dir and INSTANT_DISABLE_KEYCHAIN=1 is set; the OS keychain
	// is then bypassed and the cliconfig file-fallback path is exercised.
	secretstore.UseDefault()

	// Load saved CLI credentials (may be empty = anonymous).
	cfg, err := cliconfig.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load credentials: %v\n", err)
	}

	// Resolve API base URL (env var beats config, config beats hardcoded default).
	if envURL := os.Getenv("INSTANT_API_URL"); envURL != "" {
		APIBaseURL = envURL
	} else if cfg != nil && cfg.APIBaseURL != "" {
		APIBaseURL = cfg.APIBaseURL
	}

	// Wire up the auth transport — no-ops when unauthenticated.
	// Priority: INSTANT_TOKEN env var > saved config (instant login).
	apiKey := os.Getenv("INSTANT_TOKEN")
	if apiKey == "" && cfg != nil {
		apiKey = cfg.APIKey
	}
	// T16 P2-2 — provisioning is synchronous and can legitimately exceed 10s
	// against the live api under load. Use httpProvisionTimeout (60s) by default
	// so a slow-but-successful provision returns the token rather than
	// orphaning the resource on the server. Operators can override via
	// INSTANT_TIMEOUT_SECONDS (e.g. set to 120 on very slow links).
	timeout := httpProvisionTimeout
	if env := os.Getenv("INSTANT_TIMEOUT_SECONDS"); env != "" {
		if n, parseErr := strconv.Atoi(env); parseErr == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	HTTPClient = &http.Client{
		Timeout: timeout,
		Transport: &authTransport{
			base:   http.DefaultTransport,
			apiKey: apiKey,
		},
	}
}

