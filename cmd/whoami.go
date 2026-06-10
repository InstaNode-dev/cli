package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/InstaNode-dev/cli/internal/cliconfig"
	"github.com/InstaNode-dev/cli/internal/secretstore"
	"github.com/spf13/cobra"
)

// authMePath is the server endpoint that validates the caller's bearer token
// and returns the canonical identity (tier/email/team). Named const (not an
// inline literal) so whoami and login share one path and can never drift.
const authMePath = "/auth/me"

// whoamiJSON is the --json flag for `instant whoami`. T16 P3: machine-readable
// identity output for agents. The bearer token is NEVER included even in JSON
// mode — only the truncated display form and the secret-backend name (P1-1).
var whoamiJSON bool

// whoamiJSONOutput is the stable schema emitted by `whoami --json`. Fields:
//
//	authenticated    true ONLY when the server validated the bearer token
//	                 (B-whoami: was previously a pure local reflector that
//	                 reported true for ANY non-empty token — a false positive
//	                 an agent gating on `.authenticated` could not detect).
//	email            customer email from /auth/me, "" when anonymous
//	tier             effective plan tier from /auth/me (anonymous, hobby, ...)
//	team_name        team display name from /auth/me, "" if unset
//	api_url          resolved api base URL
//	key_display      truncated key for display (NEVER the full token)
//	secret_backend   "macOS Keychain" / "libsecret" / "on-disk fallback" / etc.
//	error            distinct, non-empty when validation could not be performed
//	                 (network/offline) — lets an agent tell "definitely not
//	                 authenticated" from "couldn't reach the server to check".
type whoamiJSONOutput struct {
	Authenticated bool   `json:"authenticated"`
	Email         string `json:"email"`
	Tier          string `json:"tier"`
	TeamName      string `json:"team_name"`
	APIURL        string `json:"api_url"`
	KeyDisplay    string `json:"key_display"`
	SecretBackend string `json:"secret_backend"`
	Error         string `json:"error,omitempty"`
}

// authMeResponse is the subset of GET /auth/me the CLI consumes to confirm a
// token is real and surface the server's authoritative identity.
type authMeResponse struct {
	Tier     string `json:"tier"`
	Email    string `json:"email"`
	TeamName string `json:"team_name"`
}

// whoamiValidation is the outcome of asking the server who we are. validated
// is true only when /auth/me returned 200 for the presented token. offline is
// true when the request could not be completed (network error / no server) —
// distinct from "the server said this token is invalid" (validated=false,
// offline=false). me holds the server identity on success.
type whoamiValidation struct {
	validated bool
	offline   bool
	err       error
	me        authMeResponse
}

// validateTokenWithServer calls GET /auth/me with the given bearer token and
// classifies the outcome. A 200 means the token is real (validated). A 401/403
// means the token is bogus or expired (not validated, not offline). A transport
// error (DNS/connection refused/timeout) means we couldn't check (offline) —
// the caller surfaces this as a distinct state rather than a false "logged out".
//
// The token is sent on an explicit per-request Authorization header rather than
// relying on the package authTransport, because `whoami` resolves token
// precedence (--token > INSTANT_TOKEN > saved login) itself and must validate
// exactly the token it will report on.
func validateTokenWithServer(apiURL, token string) whoamiValidation {
	if strings.TrimSpace(token) == "" {
		return whoamiValidation{validated: false, offline: false}
	}
	req, err := http.NewRequest(http.MethodGet, apiURL+authMePath, nil)
	if err != nil {
		return whoamiValidation{validated: false, offline: true, err: err}
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return whoamiValidation{validated: false, offline: true, err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		var me authMeResponse
		if jerr := json.Unmarshal(raw, &me); jerr != nil {
			return whoamiValidation{validated: false, offline: true, err: jerr}
		}
		return whoamiValidation{validated: true, me: me}
	case http.StatusUnauthorized, http.StatusForbidden:
		// Server explicitly rejected the token — definitively not authenticated.
		return whoamiValidation{validated: false, offline: false}
	default:
		// Any other status (5xx, unexpected) means we couldn't confirm — treat
		// as offline so a flaky server doesn't masquerade as "logged out".
		return whoamiValidation{
			validated: false,
			offline:   true,
			err:       fmt.Errorf("server returned %d validating token", resp.StatusCode),
		}
	}
}

var whoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Show the currently authenticated account",
	Long: `Show the currently authenticated account.

whoami validates the bearer token against the server (GET /auth/me) before
reporting "authenticated": a bogus or expired token reports NOT authenticated
(exit 3), so an agent can gate on the result. If the server can't be reached,
whoami reports authenticated:false with a distinct "error" note rather than a
false positive.

With --json, output is a machine-readable identity object. The bearer
token is NEVER included even in JSON mode (T16 P1-1); only a truncated
display form and the secret-backend name are surfaced.
`,
	RunE: runWhoami,
}

func runWhoami(cmd *cobra.Command, args []string) error {
	cfg, err := cliconfig.Load()
	if err != nil {
		return wrapJSONErr(cmd, err)
	}

	// Auth token precedence: --token flag > INSTANT_TOKEN env > cliconfig
	// (keychain/file). Mirrors cmd/root.go::initConfig so whoami validates
	// exactly the token the rest of the CLI would use. Whitespace is trimmed
	// at every source so a stray newline from `$(cat .pat)` doesn't break the
	// Authorization header.
	token := strings.TrimSpace(cfg.APIKey)
	if flagTok := strings.TrimSpace(adHocToken); flagTok != "" {
		token = flagTok
	} else if envTok := strings.TrimSpace(os.Getenv("INSTANT_TOKEN")); envTok != "" {
		token = envTok
	}

	// Resolve api_url so --json never emits api_url:"".
	// Priority: cfg.APIBaseURL > INSTANT_API_URL env > APIBaseURL package var > hardcoded default.
	apiURL := cfg.APIBaseURL
	if apiURL == "" {
		apiURL = strings.TrimSpace(os.Getenv("INSTANT_API_URL"))
	}
	if apiURL == "" {
		apiURL = APIBaseURL
	}
	if apiURL == "" {
		apiURL = defaultAPIBaseURL
	}

	// No token at all → cleanly anonymous, never touch the network.
	if token == "" {
		return whoamiReportAnonymous(cmd, cfg, apiURL)
	}

	// B-whoami: REAL validation. The previous implementation reported
	// authenticated:true for ANY non-empty token (a local reflector). We now
	// confirm against GET /auth/me so an agent gating on `.authenticated`
	// can trust it.
	v := validateTokenWithServer(apiURL, token)
	return whoamiReport(cmd, cfg, apiURL, token, v)
}

// whoamiReportAnonymous renders the no-token path (genuinely anonymous).
func whoamiReportAnonymous(cmd *cobra.Command, cfg *cliconfig.Config, apiURL string) error {
	if whoamiJSON {
		return encodeWhoamiJSON(whoamiJSONOutput{
			Authenticated: false,
			APIURL:        apiURL,
			SecretBackend: cfg.SecretBackendName(),
		})
	}
	fmt.Println("Not logged in (anonymous mode).")
	fmt.Printf("Run `instant login` to authenticate, or `instant db new` to provision a database without an account.\n")
	return nil
}

// whoamiReport renders the result of a server validation attempt and returns
// the correct exit-code-bearing error (nil when authenticated, errSessionExpired
// when the server rejected the token, a plain error when offline).
func whoamiReport(cmd *cobra.Command, cfg *cliconfig.Config, apiURL, token string, v whoamiValidation) error {
	keyDisplay := secretstore.TruncateForDisplay(token)
	backend := cfg.SecretBackendName()

	if whoamiJSON {
		out := whoamiJSONOutput{
			Authenticated: v.validated,
			Email:         v.me.Email,
			Tier:          v.me.Tier,
			TeamName:      v.me.TeamName,
			APIURL:        apiURL,
			KeyDisplay:    keyDisplay,
			SecretBackend: backend,
		}
		if !v.validated {
			out.Error = whoamiErrorNote(v)
		}
		// Encode errors to stdout are not actionable (matches json_error.go's
		// _ = enc.Encode convention); the exit code below carries the result.
		_ = encodeWhoamiJSON(out)
		// The identity envelope above already carries the `error` field, so we
		// must NOT funnel through wrapJSONErr (it would emit a SECOND envelope).
		// Return the bare exit-code error so cobra exits non-zero while keeping
		// stdout to a single JSON object.
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return whoamiExitCode(v)
	}

	if v.validated {
		fmt.Printf("Email:    %s\n", v.me.Email)
		fmt.Printf("Plan:     %s\n", v.me.Tier)
		if v.me.TeamName != "" {
			fmt.Printf("Team:     %s\n", v.me.TeamName)
		}
		fmt.Printf("API URL:  %s\n", apiURL)
		// T16 P1-1: never display more than 8 chars of the bearer token, and
		// surface which backend holds it.
		fmt.Printf("Key:      %s\n", keyDisplay)
		fmt.Printf("Stored:   %s\n", backend)
		return nil
	}

	if v.offline {
		fmt.Printf("Could not verify credentials: %s\n", whoamiErrorNote(v))
		fmt.Printf("API URL:  %s\n", apiURL)
		fmt.Printf("Key:      %s\n", keyDisplay)
	} else {
		fmt.Println("Not authenticated: the server rejected the presented token.")
		fmt.Println("Run `instant login`, or set INSTANT_TOKEN to a valid Personal Access Token.")
	}
	// The human-readable lines above ARE the user-facing message; silence
	// cobra's own "Error: …\nUsage:" block so it isn't printed twice.
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return whoamiExitCode(v)
}

// whoamiErrorNote returns a stable, human-readable explanation for a failed
// validation. For the offline case it surfaces a generic, agent-stable phrase
// (not the raw Go transport string) so scripts can branch on it.
func whoamiErrorNote(v whoamiValidation) string {
	if v.offline {
		if v.err != nil {
			return "could not reach the server to validate the token: " + v.err.Error()
		}
		return "could not reach the server to validate the token"
	}
	return "the server rejected the presented token"
}

// whoamiExitCode maps a validation outcome to the CLI's exit-code contract:
// a confirmed token is success (nil → exit 0); a server rejection is a
// session/auth problem (errSessionExpired → exit 3); an offline check that
// could not confirm is a generic failure (exit 1). The returned error carries
// the exit code only — the human/JSON output has already been written by the
// caller, so this never prints anything itself.
func whoamiExitCode(v whoamiValidation) error {
	if v.validated {
		return nil
	}
	if v.offline {
		base := errors.New("could not validate credentials")
		if v.err != nil {
			base = fmt.Errorf("could not validate credentials: %w", v.err)
		}
		return withExitCode(ExitGeneric, base)
	}
	return errSessionExpired()
}

// encodeWhoamiJSON writes the identity object to stdout with the shared
// two-space indentation used by every other --json command.
func encodeWhoamiJSON(out whoamiJSONOutput) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove locally saved credentials",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := cliconfig.Load()
		if err != nil {
			return err
		}
		if !cfg.IsAuthenticated() {
			fmt.Println("Not logged in.")
			return nil
		}
		email := cfg.Email
		if err := cliconfig.Clear(); err != nil {
			return fmt.Errorf("removing credentials: %w", err)
		}
		fmt.Printf("Logged out %s.\n", email)
		fmt.Println("Anonymous mode restored. Your provisioned resources are still active on the server.")
		return nil
	},
}

func init() {
	whoamiCmd.Flags().BoolVar(&whoamiJSON, "json", false,
		"Emit a JSON identity object instead of a human-readable summary")
	rootCmd.AddCommand(whoamiCmd)
	rootCmd.AddCommand(logoutCmd)
}
