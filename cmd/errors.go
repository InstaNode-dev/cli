package cmd

// errors.go — typed CLI errors that carry an exit-code contract.
//
// The CLI advertises specific exit codes in its `up` help text (and intends
// the same discipline platform-wide):
//
//   ExitOK              0  success
//   ExitGeneric         1  generic / unknown failure (manifest parse, I/O, ...)
//   ExitResourceFailed  2  one or more resources failed to provision / reconcile
//   ExitAuthRequired    3  authentication required for the requested action
//                          (also used for 401 from a previously-valid session
//                          — see ExitSessionExpired below, which folds into 3)
//   ExitSessionExpired  3  same exit code as ExitAuthRequired: the server
//                          rejected our credentials, so re-login is required.
//
// main.go inspects the returned error for an *ExitCodeError; if present the
// embedded code is the process exit code. RunE handlers return one of the
// helpers below (errResourceFailed, errAuthRequired, errSessionExpired) and
// wrap any extra context with %w if needed.
//
// Pre-existing surface preserved: a plain `error` still exits 1 (errors.go's
// extractExitCode default), matching today's behaviour for legacy code paths
// that have not been converted to the typed-error contract.

import (
	"errors"
	"fmt"
)

// Exit codes — the public contract the CLI promises.
const (
	ExitOK             = 0
	ExitGeneric        = 1
	ExitResourceFailed = 2
	ExitAuthRequired   = 3
	// ExitSessionExpired collapses to the same value as ExitAuthRequired: an
	// agent script only needs one branch ("any auth issue → re-login") and
	// the documented contract advertises 0/1/2/3.
	ExitSessionExpired = ExitAuthRequired
)

// ExitCodeError carries both a wrapped cause and the documented exit code
// the CLI should terminate with. main.go uses errors.As to extract it.
type ExitCodeError struct {
	Code int
	Err  error
}

// Error implements error.
func (e *ExitCodeError) Error() string {
	if e == nil || e.Err == nil {
		return fmt.Sprintf("exit %d", e.codeOrDefault())
	}
	return e.Err.Error()
}

// Unwrap supports errors.Is / errors.As.
func (e *ExitCodeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *ExitCodeError) codeOrDefault() int {
	if e == nil || e.Code == 0 {
		return ExitGeneric
	}
	return e.Code
}

// withExitCode wraps an error with the requested exit code. Nil err returns
// nil so callers can chain it on the happy path.
func withExitCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &ExitCodeError{Code: code, Err: err}
}

// errResourceFailed wraps an error with ExitResourceFailed (2). Used by `up`
// when one or more resources failed to reconcile.
func errResourceFailed(err error) error {
	return withExitCode(ExitResourceFailed, err)
}

// errAuthRequired returns a 'login required' error (exit code 3). The message
// is a single uniform string so agents can branch on it deterministically.
func errAuthRequired(detail string) error {
	if detail == "" {
		detail = "authentication required — run `instant login` or set INSTANT_TOKEN to a Personal Access Token"
	}
	return &ExitCodeError{Code: ExitAuthRequired, Err: errors.New(detail)}
}

// errSessionExpired is the uniform "your saved session is no longer valid"
// error. Emitted whenever the server returns 401 for a request that did send
// a bearer token. Tests assert on this exact wording so the contract is
// stable for downstream agents.
//
// IMPORTANT: keep the literal phrase "session expired" in the message — the
// hermetic suite (and the project's "shipped ≠ verified" rules) grep for it.
func errSessionExpired() error {
	return &ExitCodeError{
		Code: ExitSessionExpired,
		Err:  errors.New("session expired — run `instant login` to re-authenticate"),
	}
}

// ExitCodeFor returns the exit code for an error: 0 for nil, the embedded
// code if it is an *ExitCodeError, else ExitGeneric (1). main.go uses this
// to translate any error from the cobra tree into os.Exit(n).
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}
	var ec *ExitCodeError
	if errors.As(err, &ec) {
		return ec.codeOrDefault()
	}
	return ExitGeneric
}
