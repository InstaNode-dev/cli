package main

import (
	"fmt"
	"os"

	"github.com/instant-dev/cli/cmd"
)

func main() {
	err := cmd.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	// Translate any error returned by the cobra tree into the documented
	// exit-code contract. A nil error exits 0; an *ExitCodeError carries its
	// own code; anything else defaults to 1 (generic failure).
	os.Exit(cmd.ExitCodeFor(err))
}
