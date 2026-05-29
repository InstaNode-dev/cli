package cmd

// deploy_stub.go — B15-P2 (T9): scope-gap stubs for the missing
// `instant deploy …` surface.
//
// The platform exposes POST /deploy/new (multipart tarball upload + Kaniko
// build) plus GET/DELETE/redeploy/logs sibling endpoints. None of those
// have a CLI binding today — the binary is ~30% of the platform surface
// (BugBash B15 finding), forcing agents to fall back to curl or MCP.
//
// Implementing the full multipart upload + tarball assembly + SSE log
// stream is out of scope for this PR (it requires a multipart client + a
// tar walker the CLI doesn't have yet). What we CAN ship — and what's
// strictly better than the prior "Did you mean: login" — is a clear
// "use this instead" stub on every documented verb:
//
//   instant deploy            → list-pointer (MCP / dashboard / curl)
//   instant deploy new        → pointer + the curl invocation
//   instant deploy logs       → pointer + the curl invocation
//   instant deploy redeploy   → pointer
//   instant deploy delete     → pointer
//
// Exit code 1 (ExitGeneric) is returned so an agent script's
// `if ! instant deploy new …` branch fires — silently exiting 0 would
// strand the agent thinking the deploy succeeded.
//
// When the CLI grows real deploy support, every stub here becomes an
// implementation with the same Use/Args shape — no breakage for scripts
// that grep --help today.

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var deployCmd = &cobra.Command{
	Use: "deploy",
	// CLI-MCP-9: label the parent (and every sub-sub-command, below) as a
	// stub so `instant --help` / `instant deploy --help` make it
	// unambiguous that these verbs are NOT implemented in the CLI yet.
	// The Short string surfaces in the root command list — that one row is
	// the agent's first signal, so it has to carry the "use MCP or curl"
	// pointer.
	Short: "[stub — current MCP/API path: POST /deploy/new or create_deploy via MCP. CLI deploy verbs not yet implemented]",
	Long: `Deploy commands are not implemented in the CLI yet.

The platform exposes the full deploy API at:
  POST   /deploy/new          (multipart tarball upload + build)
  GET    /api/v1/deployments
  GET    /api/v1/deployments/:id
  POST   /deploy/:id/redeploy
  DELETE /deploy/:id
  GET    /deploy/:id/logs     (SSE stream)

Use one of these surfaces today:

  1. MCP tools (Claude Code, Cursor, etc.):
       create_deploy, list_deployments, get_deployment, redeploy,
       delete_deployment

  2. Dashboard:
       https://instanode.dev/app/deployments

  3. curl, with a tarball ready:
       curl -X POST https://api.instanode.dev/deploy/new \
         -H "Authorization: Bearer $INSTANT_TOKEN" \
         -F "name=my-app" \
         -F "env=production" \
         -F "tarball=@./app.tar.gz"

Track the upcoming native CLI support at:
  https://github.com/InstaNode-dev/cli/issues
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Print the long help (covers the alternative-surface pointers)
		// and exit non-zero so scripts that test exit code don't proceed
		// as if a deploy happened.
		_ = cmd.Help()
		return withExitCode(ExitGeneric,
			fmt.Errorf("`instant deploy` is not yet implemented — use MCP, dashboard, or curl (see help text above)"))
	},
}

// newDeployStub returns a sub-sub-command that points at the canonical
// alternative for `instant deploy <verb>`. Helps agents that ran
// `instant deploy logs <id>` (and got "Did you mean: login") find the
// real path without checking docs.
func newDeployStub(verb, extra string) *cobra.Command {
	short := "Deploy " + verb + " (not yet implemented — use MCP or curl)"
	return &cobra.Command{
		Use:   verb,
		Short: short,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"`instant deploy %s` is not yet implemented in the CLI.\n"+
					"Use one of:\n"+
					"  - MCP tool   (Claude Code / Cursor: %s)\n"+
					"  - dashboard  (https://instanode.dev/app/deployments)\n"+
					"  - curl       (%s)\n",
				verb, mcpAliasFor(verb), curlHintFor(verb, args, extra))
			return withExitCode(ExitGeneric,
				fmt.Errorf("instant deploy %s: not implemented", verb))
		},
	}
}

// mcpAliasFor returns the MCP tool name for a given deploy verb so agents
// reading the stub error know exactly which tool to call. Keeping the
// mapping inline (rather than fetching from MCP) keeps this file
// dependency-free.
func mcpAliasFor(verb string) string {
	switch verb {
	case "new":
		return "create_deploy"
	case "list":
		return "list_deployments"
	case "get":
		return "get_deployment"
	case "logs":
		return "get_deployment"
	case "redeploy":
		return "redeploy"
	case "delete":
		return "delete_deployment"
	}
	return "<deploy MCP tools>"
}

// curlHintFor renders a minimal curl invocation for the given deploy verb
// so an agent can copy-paste from the error and proceed.
func curlHintFor(verb string, args []string, _ string) string {
	id := "<deploy-id>"
	if len(args) > 0 && args[0] != "" {
		id = args[0]
	}
	base := "https://api.instanode.dev"
	auth := `-H "Authorization: Bearer $INSTANT_TOKEN"`
	switch verb {
	case "new":
		return fmt.Sprintf(
			"curl -X POST %s/deploy/new %s -F name=NAME -F env=production -F tarball=@./app.tar.gz",
			base, auth)
	case "list":
		return fmt.Sprintf("curl %s/api/v1/deployments %s", base, auth)
	case "get":
		return fmt.Sprintf("curl %s/api/v1/deployments/%s %s", base, id, auth)
	case "logs":
		return fmt.Sprintf("curl -N %s/deploy/%s/logs %s", base, id, auth)
	case "redeploy":
		return fmt.Sprintf("curl -X POST %s/deploy/%s/redeploy %s", base, id, auth)
	case "delete":
		return fmt.Sprintf("curl -X DELETE %s/deploy/%s %s", base, id, auth)
	}
	return strings.TrimSpace(fmt.Sprintf("curl %s/deploy/... %s", base, auth))
}

func init() {
	deployCmd.AddCommand(
		newDeployStub("new", ""),
		newDeployStub("list", ""),
		newDeployStub("get", ""),
		newDeployStub("logs", ""),
		newDeployStub("redeploy", ""),
		newDeployStub("delete", ""),
	)
	rootCmd.AddCommand(deployCmd)
}
