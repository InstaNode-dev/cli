package cmd

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/InstaNode-dev/cli/internal/cliconfig"
	"github.com/InstaNode-dev/cli/internal/secretstore"
)

var _ = httpListTimeout // documented constant; referenced in tests / future refactor

// APIBaseURL is the instanode.dev API base URL.
// Resolved at init from (in priority order):
//  1. INSTANT_API_URL env var
//  2. ~/.instant-config api_base_url
//  3. Default: https://api.instanode.dev
var APIBaseURL = "https://api.instanode.dev"

// adHocToken is bound to the global --token flag. B15-P2: ad-hoc auth
// override that doesn't require exporting INSTANT_TOKEN or running
// `instant login` — useful for one-off invocations and CI scripts that
// want to inline a PAT. When set, it takes precedence over INSTANT_TOKEN
// (which itself takes precedence over the saved cliconfig). Whitespace is
// trimmed for parity with INSTANT_TOKEN (B15-P1 (8)).
var adHocToken string

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
	// B15-P0 (2) — Version is populated by SetBuildInfo() from main.go's
	// ldflag-stamped vars. cobra surfaces this via `instant --version`
	// and `instant -v`. The format mirrors api/worker/provisioner's
	// /healthz output (version (commit, buildtime)) so CLAUDE.md rule 14
	// (build-SHA gate) can be enforced against the CLI binary too.
	Version: "dev (unknown, unknown)",
	Long: `instanode.dev CLI

Provision databases, caches, queues, and document stores with a single command.
No account required to get started. Log in with 'instant login' to persist resources.

Every provisioning command requires a --name flag (1–64 chars).

Examples:
  instant db new --name app-db        Provision a Postgres database (+ pgvector)
  instant cache new --name app-cache  Provision a Redis cache
  instant nosql new --name app-docs   Provision a MongoDB document store
  instant queue new --name app-jobs   Provision a NATS JetStream queue
  instant storage new --name app-blob Provision an object-storage bucket prefix
  instant webhook new --name app-hook Provision a webhook receiver URL
  instant vector new --name app-vec   Provision a Postgres+pgvector resource
  instant resources                   List your provisioned resources (requires login)
  instant resource <token>            Show detail for a single resource by token
  instant resource delete <token>     Delete a resource (use --yes to skip confirm)
  instant status                      Show locally tracked resources
  instant login                       Log in to your instanode.dev account
  instant logout                      Remove locally saved credentials
  instant whoami                      Show current account
  instant upgrade                     Open the upgrade page
  instant --version                   Print version, commit SHA, build time
`,
}

// Execute runs the root command.
//
// This is intentionally a 1-line wrapper around ExecuteWithArgs so the
// production callsite (main.go) and tests share the same entry point.
// Tests use ExecuteWithArgs directly to assert behaviour without polluting
// os.Args.
func Execute() error {
	return ExecuteWithArgs(os.Args[1:])
}

// ExecuteWithArgs runs the root command with an explicit args slice. The
// production callsite passes os.Args[1:]; tests pass a fixed slice.
func ExecuteWithArgs(args []string) error {
	rootCmd.SetArgs(args)
	return rootCmd.Execute()
}

// SetBuildInfo wires the ldflag-stamped Version/Commit/BuildTime from main
// into the cobra root so `instant --version` prints them. Called from
// main.go::main(). Kept here (rather than init()) so the seam between the
// main package and cmd/ stays explicit and so tests can override it.
//
// B15-P0 (2): CLAUDE.md rule 14 requires every deploy to verify the live
// binary's commit matches `git rev-parse --short HEAD`. For the CLI, that
// gate is `instant --version | grep <sha>` — which can only be satisfied
// if the linker actually stamped these vars.
func SetBuildInfo(version, commit, buildTime string) {
	if version == "" {
		version = "dev"
	}
	if commit == "" {
		commit = "unknown"
	}
	if buildTime == "" {
		buildTime = "unknown"
	}
	rootCmd.Version = fmt.Sprintf("%s (%s, %s)", version, commit, buildTime)
}

func init() {
	cobra.OnInitialize(initConfig)
	// B15-P1 (10) — silence cobra's "Error: …\nUsage:\n…" block on every
	// RunE failure. main.go owns the single stderr print of the error
	// message, and the cobra usage dump is not actionable for runtime
	// failures (a 429 quota error doesn't get fixed by changing flags).
	// Cobra still reports flag-parse failures (e.g. unknown-flag, required-flag
	// missing) via the FParseErrWhitelist path, which DO surface usage when
	// relevant — so usability for "wrong-flag" cases is preserved.
	//
	// Without this the prior output for `instant resources` (no auth) was:
	//   Error: authentication required ...     ← cobra
	//   Usage:                                 ← cobra (irrelevant)
	//     instant resources [flags]
	//   ...
	//   authentication required ...           ← main.go (duplicate)
	// ≈15 lines of noise per failure. Now main.go prints once.
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true

	// B15-P2 — global --token flag for ad-hoc auth without exporting
	// INSTANT_TOKEN. Persistent so every subcommand sees it; takes
	// precedence over INSTANT_TOKEN and saved cliconfig. The bound var
	// is consumed by initConfig (cobra.OnInitialize) which fires AFTER
	// flag parsing — so the auth transport sees the flag value.
	rootCmd.PersistentFlags().StringVar(&adHocToken, "token", "",
		"Bearer token for this invocation (overrides INSTANT_TOKEN and saved login)")

	// BUG-CLI-016 (QA 2026-05-29): cobra's default `completion` parent
	// command prints its help and exits 0 when invoked with no shell
	// argument. That's the wrong contract for CI/wrapper scripts —
	// "no shell selected" is a usage error, not success. We need to
	// force-init the default completion subtree (cobra normally adds
	// it lazily inside Execute()), then stamp a RunE that returns an
	// error so cobra-emitted exit propagates to main.go::ExitCodeFor.
	// Sub-shells (`completion bash`, etc.) keep their original RunE —
	// only the bare `completion` invocation changes.
	rootCmd.InitDefaultCompletionCmd()
	for _, c := range rootCmd.Commands() {
		if c.Name() == "completion" {
			c.RunE = func(cmd *cobra.Command, args []string) error {
				_ = cmd.Help()
				// Plain error → ExitCodeFor falls through to ExitGeneric (1).
				// "shell argument required" is a usage error, not a runtime
				// failure of the requested shell-completion generation.
				return fmt.Errorf("instant completion: shell argument required (bash | zsh | fish | powershell)")
			}
			break
		}
	}

	// BUG-CLI-041 (QA 2026-05-29): many CLIs accept both `version` and
	// `--version`. Cobra wires `--version`/`-v` via rootCmd.Version,
	// but `instant version` returned "unknown command 'version'" with
	// exit=1 — confusing for users muscle-memorying `git version` /
	// `node version` patterns. Register an explicit `version` alias
	// that prints the same line cobra emits for `--version`.
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version, commit SHA, build time (alias for `instant --version`)",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			// rootCmd.Version is shaped "vX.Y.Z (sha, buildtime)" by
			// SetBuildInfo. Mirror cobra's default `--version` output
			// ("instant version vX.Y.Z (sha, buildtime)") so a script
			// that grep-greps either path sees the same string.
			fmt.Printf("%s version %s\n", rootCmd.Name(), rootCmd.Version)
		},
	})
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
	// Priority:
	//   1. --token global flag (B15-P2 ad-hoc override)
	//   2. INSTANT_TOKEN env var
	//   3. saved config (instant login)
	// B15-P1: TrimSpace at every source so `INSTANT_TOKEN=$(cat .pat)`
	// or `--token "$(cat .pat)"` (with trailing newline) doesn't produce
	// an "Authorization: Bearer tok\n" header that the server rejects.
	apiKey := strings.TrimSpace(adHocToken)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("INSTANT_TOKEN"))
	}
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

