package cmd

// json_error.go — B15-P0 (4): when a CLI subcommand is invoked with --json,
// any error MUST be emitted as a JSON envelope on stdout rather than as
// cobra's text "Error: …\nUsage:\n…" block. Agents that pipe `--json` into
// `jq` previously crashed on every 4xx/5xx because cobra's text error
// handler doesn't speak JSON.
//
// The wrapper is opt-in per-command — call wrapJSONErr(cmd, err) from any
// RunE that supports --json. It detects the flag, emits the envelope, and
// silences cobra's usage-on-error block by setting SilenceUsage +
// SilenceErrors on the command.
//
// Envelope schema (stable; mirrors the API's W7G envelope where possible):
//
//	{
//	  "ok":           false,
//	  "error":        "<code or 'cli_error'>",
//	  "message":      "<human-readable message>",
//	  "agent_action": "<what the agent should do next>",
//	  "request_id":   "<server-supplied id, when present>",
//	  "upgrade_url":  "<set on 402>",
//	  "exit_code":    <integer matching the documented contract>
//	}
//
// Network errors (DNS, connection refused, TLS handshake) get wrapped here
// too — instead of surfacing the raw Go error string ("dial tcp: lookup …")
// the envelope carries a stable "network_error" code + a "check connectivity
// or set INSTANT_API_URL" agent_action so an agent script has one branch.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// jsonErrorEnvelope is the stable shape every --json command emits on
// failure. Optional fields use omitempty so the JSON stays compact when
// the server didn't supply them.
type jsonErrorEnvelope struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error"`
	Message     string `json:"message"`
	AgentAction string `json:"agent_action,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	UpgradeURL  string `json:"upgrade_url,omitempty"`
	ExitCode    int    `json:"exit_code"`
}

// jsonModeOn reports whether the command (or any ancestor) has --json set.
// We walk up the parent chain because cobra binds flags per-command, and a
// caller like `instant resources --json` resolves --json on resourcesCmd
// while a hypothetical `instant db --json new ...` would bind it on dbCmd.
//
// We also peek at the raw os.Args as a last resort because the global
// helpers in monitor.go bind --json on individual commands (statusJSON,
// resourcesJSON, whoamiJSON) and tests reset them between cases.
func jsonModeOn(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if fl := c.Flags().Lookup("json"); fl != nil && fl.Changed {
			return true
		}
	}
	// Tests + the global json bools cover the common case; fall back to
	// scanning the raw args so callers that build the command tree
	// programmatically (without running through cobra's parser) still
	// behave correctly.
	for _, a := range os.Args[1:] {
		if a == "--json" || a == "--json=true" {
			return true
		}
	}
	// Also honor the package-global toggles set by cobra's BoolVar bindings
	// — these are how the existing whoami/resources/status commands carry
	// the flag. They're already wired by the time RunE fires.
	if resourcesJSON || statusJSON || whoamiJSON {
		return true
	}
	return false
}

// classifyError inspects err and returns (code, message, agentAction).
// Used by wrapJSONErr to turn raw Go errors into a stable envelope.
func classifyError(err error) (code, message, agentAction string) {
	if err == nil {
		return "", "", ""
	}
	msg := err.Error()

	// Already an *ExitCodeError? Preserve the inner message.
	var ec *ExitCodeError
	if errors.As(err, &ec) {
		switch ec.Code {
		case ExitAuthRequired:
			return "auth_required", msg,
				"run `instant login`, or set INSTANT_TOKEN to a Personal Access Token"
		case ExitResourceFailed:
			return "resource_failed", msg,
				"retry the command; if it persists, inspect `instant resources` and run `instant up` again"
		}
		// Fall through to the network/URL classifier with the unwrapped err.
		if ec.Err != nil {
			msg = ec.Err.Error()
		}
	}

	// B15-P1 — wrap network errors so DNS/TCP failures surface as a clean
	// "network_error" envelope rather than a raw Go string.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return "network_error",
				fmt.Sprintf("DNS lookup failed for %s: %s", dnsErr.Name, dnsErr.Err),
				"check internet connectivity, or set INSTANT_API_URL to override the endpoint"
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return "network_error",
				fmt.Sprintf("network error reaching %s: %s", urlErr.URL, opErr.Err),
				"check internet connectivity; if it persists, set INSTANT_API_URL to a reachable endpoint"
		}
		return "network_error", urlErr.Error(),
			"check internet connectivity, or set INSTANT_API_URL to override the endpoint"
	}

	// Default: surface the raw message; agents read .error code regardless.
	if strings.Contains(strings.ToLower(msg), "session expired") {
		return "session_expired", msg,
			"run `instant login` to re-authenticate"
	}
	return "cli_error", msg, ""
}

// wrapJSONErr emits a JSON error envelope on stdout when --json is on, and
// returns the same error to the caller so cobra's exit-code path still fires
// (with usage printing silenced). When --json is OFF, returns err unchanged
// so the legacy text path takes over.
//
// This is the central seam B15-P0 (4) calls for: every command that supports
// --json should funnel its errors through wrapJSONErr at the end of RunE.
func wrapJSONErr(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	if !jsonModeOn(cmd) {
		return err
	}

	code, message, agentAction := classifyError(err)
	if code == "" {
		code = "cli_error"
	}
	if message == "" {
		message = err.Error()
	}

	env := jsonErrorEnvelope{
		OK:          false,
		Error:       code,
		Message:     message,
		AgentAction: agentAction,
		ExitCode:    ExitCodeFor(err),
	}

	// Pull request_id / upgrade_url out of the API-error message when the
	// caller already passed through parseAPIError() — those fields are
	// embedded as " — (request_id=…)" / " — upgrade: …" suffixes.
	if id := extractTagged(message, "request_id="); id != "" {
		env.RequestID = strings.TrimRight(id, ")")
	}
	if up := extractTagged(message, "upgrade: "); up != "" {
		env.UpgradeURL = strings.SplitN(up, " ", 2)[0]
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(env)

	// Silence cobra's "Error: … / Usage:" block; the envelope is the only
	// output an agent should see.
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return err
}

// extractTagged scans s for `tag<value>` and returns value until the next
// space or end of string. Used to pull request_id / upgrade values out of
// the error string parseAPIError emits.
func extractTagged(s, tag string) string {
	idx := strings.Index(s, tag)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(tag):]
	for i, r := range rest {
		if r == ' ' || r == '\n' {
			return rest[:i]
		}
	}
	return rest
}
