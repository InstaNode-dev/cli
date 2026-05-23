package main

import (
	"fmt"
	"io"
	"os"

	"github.com/InstaNode-dev/cli/cmd"
)

// B15-P0 (2) — build-info stamping. Wired in at link time via:
//
//	go build -ldflags "-X main.Version=$(cat VERSION) \
//	                   -X main.Commit=$(git rev-parse --short HEAD) \
//	                   -X main.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// Defaults are sentinel strings ("dev" / "unknown") so an un-flagged
// `go build` still produces a runnable binary — useful for `go test`
// and `go run`. The Makefile and the release workflow always pass real
// values via -X. CLAUDE.md rule 14 (build-SHA gate) reads this via
// `instant --version` after every deploy.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is the testable inner entrypoint. It is identical to main() except
// it does not call os.Exit and accepts the args slice and stderr writer
// as explicit parameters. Returns the documented exit code (0 = success;
// see cmd.ExitCodeFor for the contract).
func run(args []string, stderr io.Writer) int {
	// Propagate the ldflag-stamped values into the cobra root so
	// `instant --version` prints them. cmd.SetBuildInfo stays a tiny
	// seam — the cmd/ package has no dependency on main.
	cmd.SetBuildInfo(Version, Commit, BuildTime)

	err := cmd.ExecuteWithArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
	}
	// Translate any error returned by the cobra tree into the documented
	// exit-code contract. A nil error exits 0; an *ExitCodeError carries its
	// own code; anything else defaults to 1 (generic failure).
	return cmd.ExitCodeFor(err)
}
