package cmd

// apierror.go — parse the standardised API error envelope and surface its
// fields cleanly to the user.
//
// T16 P2-1: every 4xx/5xx response from api.instanode.dev follows the W7G
// envelope:
//
//   {
//     "ok": false,
//     "error": "quota_exceeded",      // machine-readable code
//     "error_code": "quota_exceeded", // alternate spelling (server may emit either)
//     "message": "You've hit your hobby-tier postgres limit (1 / 1).",
//     "agent_action": "Run `instant upgrade` to raise the limit, …",
//     "upgrade_url": "https://instanode.dev/billing",
//     "request_id": "req_abc123",
//     "retry_after_seconds": 30
//   }
//
// Before this fix the CLI dumped the entire raw JSON body to the user,
// which is non-actionable noise (and may leak server-internal strings).
// parseAPIError() turns the envelope into a stable, human-readable error
// message that always carries the most useful field for the status code:
//
//   402 → message + agent_action + upgrade_url
//   429 → "rate limited, retry in N seconds" + agent_action
//   5xx → message + agent_action ("server error, retry later")
//   4xx → message + agent_action
//   other → fall back to truncated raw body (defensive)
//
// The helper never returns nil — even if parsing fails (non-JSON body),
// it returns a truncated raw-body error so the caller still gets *some*
// message.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// apiErrorEnvelope mirrors the W7G error shape served by api.instanode.dev.
// Optional fields are pointers/empty-zero so we can detect "field absent"
// vs "field present but empty".
type apiErrorEnvelope struct {
	OK                bool   `json:"ok"`
	Error             string `json:"error"`
	ErrorCode         string `json:"error_code"`
	Message           string `json:"message"`
	AgentAction       string `json:"agent_action"`
	UpgradeURL        string `json:"upgrade_url"`
	Upgrade           string `json:"upgrade"`              // legacy shape
	RequestID         string `json:"request_id"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
}

// code returns the most-specific error code the envelope carries.
func (e *apiErrorEnvelope) code() string {
	if e.ErrorCode != "" {
		return e.ErrorCode
	}
	return e.Error
}

// humanMessage returns the most descriptive single-line message available.
// Preference: message > agent_action > error code > "".
func (e *apiErrorEnvelope) humanMessage() string {
	if e.Message != "" {
		return e.Message
	}
	if e.AgentAction != "" {
		return e.AgentAction
	}
	return e.code()
}

// parseAPIError takes an HTTP status code and a raw response body, and
// produces a single error whose message is the cleanest representation
// the envelope contents allow. The status is part of the message so an
// agent script can grep on it; the body is only echoed (truncated) when
// parsing fails entirely.
//
// Never returns nil.
func parseAPIError(status int, raw []byte) error {
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return fmt.Errorf("server returned %d (no body)", status)
	}

	var env apiErrorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Body wasn't JSON, or wasn't the envelope shape. Fall back to the
		// raw body, truncated so we don't dump megabytes of HTML / stack
		// traces into the user's terminal.
		return fmt.Errorf("server returned %d: %s", status, truncate(body, 200))
	}

	// If we got an envelope but every interesting field is empty, fall back
	// to the truncated raw body so we still surface *something*.
	if env.code() == "" && env.Message == "" && env.AgentAction == "" {
		return fmt.Errorf("server returned %d: %s", status, truncate(body, 200))
	}

	// Build the human message. Status-specific framing first, then the
	// envelope's own message, then agent_action, then upgrade_url.
	var parts []string

	switch {
	case status == 402:
		// Tier wall — the most actionable status.
		parts = append(parts, fmt.Sprintf("%d %s", status, codeOrDefault(env.code(), "tier limit reached")))
	case status == 429:
		// Rate limited. Include retry hint when the server provides one.
		if env.RetryAfterSeconds > 0 {
			parts = append(parts,
				fmt.Sprintf("%d rate limited (retry in %ds)",
					status, env.RetryAfterSeconds))
		} else {
			parts = append(parts, fmt.Sprintf("%d rate limited", status))
		}
	case status >= 500:
		// Transient server error — agent should retry.
		parts = append(parts, fmt.Sprintf("%d %s", status, codeOrDefault(env.code(), "server error, retry later")))
	default:
		parts = append(parts, fmt.Sprintf("%d %s", status, codeOrDefault(env.code(), "request rejected")))
	}

	if env.Message != "" {
		parts = append(parts, env.Message)
	}
	if env.AgentAction != "" && env.AgentAction != env.Message {
		parts = append(parts, "→ "+env.AgentAction)
	}
	upgrade := env.UpgradeURL
	if upgrade == "" {
		upgrade = env.Upgrade
	}
	if upgrade != "" {
		parts = append(parts, "upgrade: "+upgrade)
	}
	if env.RequestID != "" {
		parts = append(parts, "(request_id="+env.RequestID+")")
	}

	return fmt.Errorf("%s", strings.Join(parts, " — "))
}

// codeOrDefault returns code when non-empty, otherwise the fallback.
func codeOrDefault(code, fallback string) string {
	if code == "" {
		return fallback
	}
	return code
}
