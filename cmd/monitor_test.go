package cmd

// White-box tests for the provisioning commands. They live in package `cmd`
// so they can exercise the unexported command tree, the shared `resourceName`
// flag variable, and validateResourceName directly.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freshProvisionCmd builds an isolated `<group> new` command tree so each test
// gets its own flag set (the production commands share the global
// `resourceName` variable, which would leak state between table cases).
func freshProvisionCmd(endpoint, resourceType string) (root *cobra.Command, name *string) {
	var bound string
	newCmd := &cobra.Command{
		Use:  "new",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateResourceName(bound); err != nil {
				return err
			}
			_, err := provisionResource(endpoint, bound, "")
			return err
		},
	}
	newCmd.Flags().StringVar(&bound, "name", "", "Resource name (required)")
	_ = newCmd.MarkFlagRequired("name")

	group := &cobra.Command{Use: resourceType}
	group.AddCommand(newCmd)
	r := &cobra.Command{Use: "instant"}
	r.AddCommand(group)
	r.SilenceUsage = true
	r.SilenceErrors = true
	return r, &bound
}

func TestValidateResourceName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty rejected", "", true},
		{"simple ok", "app-db", false},
		{"with spaces ok", "My App DB", false},
		{"underscores ok", "app_db_1", false},
		{"alphanumeric start ok", "1db", false},
		{"leading dash rejected", "-db", true},
		{"leading space rejected", " db", true},
		{"slash rejected", "app/db", true},
		{"max length ok", strings.Repeat("a", nameMaxLen), false},
		{"over max length rejected", strings.Repeat("a", nameMaxLen+1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResourceName(tc.input)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestProvisionMissingNameRejected confirms cobra rejects a `new` invocation
// that omits --name before any API call is attempted.
func TestProvisionMissingNameRejected(t *testing.T) {
	for _, rt := range []string{"db", "cache", "nosql", "queue"} {
		t.Run(rt, func(t *testing.T) {
			root, _ := freshProvisionCmd("/"+rt+"/new", rt)
			root.SetArgs([]string{rt, "new"})
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			err := root.Execute()
			require.Error(t, err, "missing --name must error")
			assert.Contains(t, strings.ToLower(err.Error()), "name",
				"error should mention the missing name flag")
		})
	}
}

// TestProvisionInvalidNameRejected confirms a syntactically invalid --name is
// rejected locally before the request is sent.
func TestProvisionInvalidNameRejected(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withTestAPI(t, srv.URL)

	root, _ := freshProvisionCmd("/db/new", "db")
	root.SetArgs([]string{"db", "new", "--name", "bad/name"})
	err := root.Execute()
	require.Error(t, err)
	assert.False(t, hit, "API must not be called when --name is invalid")
}

// TestProvisionValidNameSendsRequest confirms a valid --name reaches the API
// with the name in the JSON body.
func TestProvisionValidNameSendsRequest(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		gotBody = b.String()
		_, _ = w.Write([]byte(`{"ok":true,"token":"tok_test","name":"app-db","connection_url":"postgres://x"}`))
	}))
	defer srv.Close()
	withTestAPI(t, srv.URL)

	root, _ := freshProvisionCmd("/db/new", "db")
	root.SetArgs([]string{"db", "new", "--name", "app-db"})
	require.NoError(t, root.Execute())
	assert.Contains(t, gotBody, `"name":"app-db"`, "name must be sent in request body")
}

// withTestAPI points the package-level APIBaseURL / HTTPClient at a test
// server for the duration of the test, restoring them afterward.
func withTestAPI(t *testing.T, baseURL string) {
	t.Helper()
	prevURL, prevClient := APIBaseURL, HTTPClient
	APIBaseURL = baseURL
	HTTPClient = &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(func() {
		APIBaseURL = prevURL
		HTTPClient = prevClient
	})
}

// ── isTimeoutErr unit coverage ────────────────────────────────────────────────

// fakeNetTimeoutErrMsg is the message carried by fakeNetTimeoutErr; named so
// tests don't scatter string literals.
const fakeNetTimeoutErrMsg = "dial tcp: i/o timeout (synthetic)"

// fakeNetTimeoutErr is a minimal net.Error with a configurable Timeout().
// It deliberately does NOT wrap context.DeadlineExceeded, so it exercises the
// errors.As(net.Error) branch of isTimeoutErr rather than the errors.Is one
// (an http.Client.Timeout error matches DeadlineExceeded first, leaving the
// net.Error branch unreachable through provisionResource alone).
type fakeNetTimeoutErr struct{ timeout bool }

func (e fakeNetTimeoutErr) Error() string   { return fakeNetTimeoutErrMsg }
func (e fakeNetTimeoutErr) Timeout() bool   { return e.timeout }
func (e fakeNetTimeoutErr) Temporary() bool { return e.timeout }

// TestIsTimeoutErr pins every branch of isTimeoutErr: the nil guard, the
// context.DeadlineExceeded path, the net.Error-with-Timeout() path (including
// when wrapped, as *url.Error does), and the non-timeout fallthrough.
func TestIsTimeoutErr(t *testing.T) {
	// Compile-time proof the fake satisfies net.Error.
	var _ net.Error = fakeNetTimeoutErr{}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error is not a timeout", nil, false},
		{"context deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped context deadline exceeded", fmt.Errorf("post: %w", context.DeadlineExceeded), true},
		{"net.Error with Timeout()==true", fakeNetTimeoutErr{timeout: true}, true},
		{"wrapped net.Error with Timeout()==true", fmt.Errorf("post: %w", fakeNetTimeoutErr{timeout: true}), true},
		{"net.Error with Timeout()==false", fakeNetTimeoutErr{timeout: false}, false},
		{"plain non-timeout error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isTimeoutErr(tc.err))
		})
	}
}
