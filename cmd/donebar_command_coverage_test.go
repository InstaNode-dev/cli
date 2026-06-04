package cmd

// donebar_command_coverage_test.go — the CLI "done-bar" drift guard.
//
// USER-FLOW-INVENTORY-AND-TEST-MATRIX.md (2026-06-04), item (4):
//   "CLI-command-coverage test: iterate cobra command tree; fail if a
//    command lacks a K-row."
//
// This file is the registry-iterating equivalent of CLAUDE.md rules 16–18:
// rather than a hand-typed list of commands (itself a single-site fallacy),
// it WALKS the live cobra tree (rootCmd and every descendant) and asserts,
// for every leaf/group command:
//
//   (a) it is runnable        — RunE or Run is non-nil (no dead command that
//                               prints nothing and exits 0), AND
//   (b) it carries help       — a non-empty Short string (surfaces in the
//                               parent's command list / `--help`), AND
//   (c) it is mapped to a test — an entry in commandTestMap whose value names
//                               a Test function that actually exists in this
//                               package.
//
// The moment someone adds `instant <newcmd>` via rootCmd.AddCommand without a
// matching contract test + map row, TestDoneBar_EveryCommandCovered RED-fails
// in CI. That is the drift guard the matrix asks for: a new command cannot
// ship "covered" by accident.
//
// Why a map and not "any test mentions the name": a substring match over test
// source is a false-positive magnet (every provisioning test mentions "db").
// The explicit map forces a human to point each command at the test that
// actually exercises its handler + endpoint + flag→payload mapping.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// commandTestMap maps a full command path ("instant db new") to the name of
// the Test function that provides its contract/integration coverage. Group
// commands (db, cache, …) point at the test that proves the group's bare
// invocation behaviour (print help / error). Leaf commands point at the test
// that asserts endpoint + flag→payload + response/error handling.
//
// EVERY command in the live cobra tree MUST have a row here. Adding a command
// without a row fails TestDoneBar_EveryCommandCovered.
var commandTestMap = map[string]string{
	// ── root ────────────────────────────────────────────────────────────────
	// rootCmd has no RunE (prints help) — exempt from the runnable check via
	// rootIsHelpOnly below, but still help-bearing. Its --help / --version
	// surface is covered here.
	"instant": "TestIntegration_HelpExitsZero",

	// ── provisioning groups (parent prints help / errors on bare invoke) ─────
	"instant db":      "TestUnknownSubcommand_BareGroupPrintsHelp",
	"instant cache":   "TestUnknownSubcommand_BareGroupPrintsHelp",
	"instant nosql":   "TestUnknownSubcommand_BareGroupPrintsHelp",
	"instant queue":   "TestUnknownSubcommand_BareGroupPrintsHelp",
	"instant storage": "TestUnknownSubcommand_BareGroupPrintsHelp",
	"instant webhook": "TestUnknownSubcommand_BareGroupPrintsHelp",
	"instant vector":  "TestUnknownSubcommand_BareGroupPrintsHelp",

	// ── provisioning leaves (endpoint + flag→payload + error handling) ───────
	"instant db new":      "TestIntegration_ProvisionAllTypes",
	"instant cache new":   "TestIntegration_ProvisionAllTypes",
	"instant nosql new":   "TestIntegration_ProvisionAllTypes",
	"instant queue new":   "TestIntegration_ProvisionAllTypes",
	"instant storage new": "TestContract_StorageVectorProvisionEndToEnd",
	"instant webhook new": "TestIntegration_UpWebhookReceiveURL",
	"instant vector new":  "TestContract_StorageVectorProvisionEndToEnd",

	// ── read / list / status ─────────────────────────────────────────────────
	"instant resources": "TestIntegration_ResourcesLists",
	"instant resource":  "TestExtras_ResourceDetail",
	"instant status":    "TestIntegration_StatusShowsProvisioned",

	// ── auth surface ─────────────────────────────────────────────────────────
	"instant login":   "TestContract_LoginUsesCanonicalAuthURL",
	"instant logout":  "TestIntegration_LoginAndWhoami",
	"instant whoami":  "TestIntegration_WhoamiAnonymous",
	"instant upgrade": "TestRunUpgrade_Authenticated",

	// ── deploy stub surface (pointer to MCP/curl, exits non-zero) ────────────
	"instant deploy":          "TestDeployStub_BareGroupExitsNonZero",
	"instant deploy new":      "TestDeployStub_KnownVerbs",
	"instant deploy list":     "TestDeployStub_KnownVerbs",
	"instant deploy get":      "TestDeployStub_KnownVerbs",
	"instant deploy logs":     "TestDeployStub_KnownVerbs",
	"instant deploy redeploy": "TestDeployStub_KnownVerbs",
	"instant deploy delete":   "TestDeployStub_KnownVerbs",

	// ── bundle / manifest ────────────────────────────────────────────────────
	"instant up": "TestIntegration_UpProvisionsAndReconciles",

	// ── meta: version + completion (cobra-generated subtree) ─────────────────
	"instant version":               "TestVersion_AliasExitsZero",
	"instant completion":            "TestCompletion_NoShellArg_ReturnsError",
	"instant completion bash":       "TestCompletion_EveryShellSubcommandStillSucceeds",
	"instant completion zsh":        "TestCompletion_EveryShellSubcommandStillSucceeds",
	"instant completion fish":       "TestCompletion_EveryShellSubcommandStillSucceeds",
	"instant completion powershell": "TestCompletion_EveryShellSubcommandStillSucceeds",
}

// rootIsHelpOnly lists the command paths that are intentionally NOT runnable
// (they print help and exit 0). Only the root is exempt — every other command
// must be runnable so an agent script never gets a silent exit-0 no-op.
var rootIsHelpOnly = map[string]bool{
	"instant": true,
}

// cobraBuiltins are commands cobra injects automatically (not authored in this
// repo) — `instant help` is added lazily inside Execute(). We skip them from
// the done-bar map requirement because they are framework surface, not the
// CLI's own contract; but they're still walked, so a future cobra change that
// adds a NEW builtin will surface here for a conscious decision.
var cobraBuiltins = map[string]bool{
	"instant help": true,
}

// walkCommands returns every command path in the tree rooted at c.
func walkCommands(c *cobra.Command, parent string) []*cobra.Command {
	full := strings.TrimSpace(parent + " " + c.Name())
	out := []*cobra.Command{c}
	for _, sub := range c.Commands() {
		out = append(out, walkCommands(sub, full)...)
	}
	return out
}

// commandPath renders the dotted command path ("instant db new") for c.
func commandPath(c *cobra.Command) string {
	parts := []string{}
	for cur := c; cur != nil; cur = cur.Parent() {
		parts = append([]string{cur.Name()}, parts...)
	}
	return strings.Join(parts, " ")
}

// TestDoneBar_EveryCommandCovered is the drift guard. It walks the LIVE cobra
// tree and asserts each command is runnable + help-bearing + mapped to a test.
// A new `rootCmd.AddCommand(...)` without a commandTestMap row fails here.
func TestDoneBar_EveryCommandCovered(t *testing.T) {
	// cobra adds `help` (and completion) lazily inside Execute(); force them in
	// so the walked tree matches what a user/agent actually sees.
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd()

	cmds := walkCommands(rootCmd, "")

	seen := map[string]bool{}
	for _, c := range cmds {
		path := commandPath(c)
		seen[path] = true

		t.Run(path, func(t *testing.T) {
			if cobraBuiltins[path] {
				t.Skipf("%q is a cobra-injected builtin (not this repo's contract surface)", path)
			}
			// (a) runnable — RunE or Run set, unless explicitly help-only.
			if !rootIsHelpOnly[path] {
				if c.RunE == nil && c.Run == nil {
					t.Errorf("command %q has neither RunE nor Run — a non-runnable command exits 0 silently, stranding agent scripts (CLAUDE.md). Give it a handler or remove it.", path)
				}
			}

			// (b) help-bearing — non-empty Short so it shows in `--help`.
			if strings.TrimSpace(c.Short) == "" {
				t.Errorf("command %q has an empty Short — it would render blank in the parent command list. Add a one-line Short.", path)
			}

			// (c) mapped to a test.
			testName, ok := commandTestMap[path]
			if !ok {
				t.Errorf("command %q is NOT in commandTestMap — a new command shipped without a contract test. Add a test that exercises its endpoint + flags + error handling, then map it here.", path)
				return
			}
			if strings.TrimSpace(testName) == "" {
				t.Errorf("command %q maps to an empty test name", path)
			}
		})
	}

	// Reverse check: every map row must point at a REAL command in the tree —
	// stale rows (command renamed/removed) are themselves drift.
	for path := range commandTestMap {
		if !seen[path] {
			t.Errorf("commandTestMap has a row for %q but that command is NOT in the live tree — remove the stale row (command renamed/removed?).", path)
		}
	}
}

// TestDoneBar_TestMapPointsAtRealTests asserts every test name referenced by
// commandTestMap actually exists as a `func TestXxx(t *testing.T)` in this
// package. Without this, the map could rot: a row could point at a deleted
// test and TestDoneBar_EveryCommandCovered would still pass (it only checks
// presence of the key). This closes that loophole by parsing the package's
// _test.go files for the referenced names.
func TestDoneBar_TestMapPointsAtRealTests(t *testing.T) {
	defined := definedTestFuncs(t)

	// Unique, sorted set of referenced test names for a stable failure list.
	refs := map[string]bool{}
	for _, name := range commandTestMap {
		refs[name] = true
	}
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		if !defined[name] {
			t.Errorf("commandTestMap references test %q which is not defined in package cmd — it was renamed or deleted. Point the map row at the real covering test.", name)
		}
	}
}

// definedTestFuncs parses every *_test.go file in the package directory and
// returns the set of top-level `func TestXxx(...)` names. Driven off the
// source (not reflection) because Go test functions aren't reflectable at
// runtime.
func definedTestFuncs(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(".", e.Name())
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			name := fn.Name.Name
			if strings.HasPrefix(name, "Test") {
				out[name] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found zero Test functions in package — parser misconfigured")
	}
	return out
}
