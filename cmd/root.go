package cmd

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/instant-dev/cli/internal/cliconfig"
	"github.com/instant-dev/cli/internal/secretstore"
)

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

// HTTPClient is the shared HTTP client used by all subcommands.
// It is configured with the auth transport during init.
var HTTPClient = &http.Client{Timeout: 10 * time.Second}

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
	HTTPClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &authTransport{
			base:   http.DefaultTransport,
			apiKey: apiKey,
		},
	}
}

