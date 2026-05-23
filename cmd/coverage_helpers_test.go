package cmd

import (
	"testing"
)

// TestShortToken covers the 0%-line in up.go.
func TestShortToken(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"abc":           "abc",
		"abcdefgh":      "abcdefgh",
		"abcdefghi":     "abcdefgh",
		"verylongtoken": "verylong",
	}
	for in, want := range cases {
		got := shortToken(in)
		if got != want {
			t.Errorf("shortToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExtractTagged covers the partial-coverage helper in json_error.go.
func TestExtractTagged(t *testing.T) {
	s := "blah request_id=abc-123 other_tag=xx upgrade=https://foo trailing"
	if got := extractTagged(s, "request_id="); got != "abc-123" {
		t.Errorf("extractTagged request_id = %q", got)
	}
	if got := extractTagged(s, "upgrade="); got != "https://foo" {
		t.Errorf("extractTagged upgrade = %q", got)
	}
	// Tag at end of string runs to EOS.
	if got := extractTagged("x trailing=end", "trailing="); got != "end" {
		t.Errorf("trailing tag = %q", got)
	}
	// Missing tag returns "".
	if got := extractTagged(s, "absent="); got != "" {
		t.Errorf("missing tag should yield empty, got %q", got)
	}
	// Stop on newline.
	if got := extractTagged("v=stop\nmore", "v="); got != "stop" {
		t.Errorf("newline should terminate, got %q", got)
	}
}

// TestHumanMessage covers apierror.go humanMessage preference order.
func TestHumanMessage(t *testing.T) {
	cases := []struct {
		env  apiErrorEnvelope
		want string
	}{
		{apiErrorEnvelope{Message: "msg", AgentAction: "act", Error: "code"}, "msg"},
		{apiErrorEnvelope{AgentAction: "act", Error: "code"}, "act"},
		{apiErrorEnvelope{Error: "code"}, "code"},
		{apiErrorEnvelope{}, ""},
	}
	for _, c := range cases {
		if got := c.env.humanMessage(); got != c.want {
			t.Errorf("humanMessage(%+v) = %q, want %q", c.env, got, c.want)
		}
	}
}
