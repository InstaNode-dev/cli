package cmd

// operate.go — Wave-2 A4: operate-verb parity with the MCP server (mcp v0.9 /
// PR #41 operate tools). The MCP server can vault / env-patch / presign /
// pause / resume / rotate / wake / capabilities / deploy-events; until this
// file the CLI could only provision + list, forcing agents back to curl for
// day-2 operations.
//
// Commands added (and the API endpoint each calls — mirrors mcp/src/client.ts):
//
//	instant vault set <env> <key>        PUT   /api/v1/vault/:env/:key
//	instant vault rotate <env> <key>     POST  /api/v1/vault/:env/:key/rotate
//	instant deploy env <app-id> K=V…     PATCH /deploy/:id/env
//	instant deploy wake <app-id>         POST  /deploy/:id/wake
//	instant deploy events <app-id>       GET   /api/v1/deployments/:id/events
//	instant stack env <slug> K=V…        PATCH /stacks/:slug/env
//	instant storage presign <token>      POST  /storage/:token/presign
//	instant capabilities                 GET   /api/v1/capabilities
//	instant resource pause <token>       POST  /api/v1/resources/:token/pause
//	instant resource resume <token>      POST  /api/v1/resources/:token/resume
//	instant resource rotate <token>      POST  /api/v1/resources/:token/rotate-credentials
//	instant resource backup <token>      POST  /api/v1/resources/:token/backup
//	instant resource backups <token>     GET   /api/v1/resources/:token/backups
//
// (the `instant resource …` verbs are dispatched from resourceCmd in
// extras.go — same pattern as `instant resource delete <token>`).
//
// Conventions:
//   - Auth: everything is auth-REQUIRED except `storage presign` (the storage
//     token in the URL is the credential — broker mode) and `capabilities`
//     (a public pre-flight discovery surface). Matches the MCP client.
//   - Path fragments are named constants — no scattered string literals.
//   - Every command supports --json; errors funnel through wrapJSONErr so the
//     agent-facing envelope contract holds.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// ── API path fragments (single source — never inline these) ─────────────────
const (
	vaultBasePath        = "/api/v1/vault"        // + /:env/:key
	vaultRotateSuffix    = "/rotate"              // POST = audit-distinct rotation
	deployBasePath       = "/deploy"              // + /:id/env | /:id/wake
	deployEnvSuffix      = "/env"                 // PATCH merge env vars
	deployWakeSuffix     = "/wake"                // POST explicit scale-to-zero wake
	deploymentsBasePath  = "/api/v1/deployments"  // + /:id/events
	deployEventsSuffix   = "/events"              // GET failure-timeline autopsy
	stacksBasePath       = "/stacks"              // + /:slug/env
	storageBasePath      = "/storage"             // + /:token/presign
	storagePresignSuffix = "/presign"             // POST broker-mode signed URL
	capabilitiesPath     = "/api/v1/capabilities" // GET tier matrix (public)
	resourcesBasePath    = "/api/v1/resources"    // + /:token/<verb>
	resourcePauseSuffix  = "/pause"               // POST suspend (Pro+)
	resourceResumeSuffix = "/resume"              // POST un-pause (Pro+)
	resourceRotateSuffix = "/rotate-credentials"  // POST rotate password
	resourceBackupSuffix = "/backup"              // POST ad-hoc backup (tier-gated)
	resourceBackupsList  = "/backups"             // GET list backups
	resourceCredsSuffix  = "/credentials"         // GET re-fetch connection URL (no rotation)
)

// operateJSON is the shared --json toggle for every operate-verb command.
// Bound per-command in init(); tests reset it between cases.
var operateJSON bool

// ── shared request helper ────────────────────────────────────────────────────

// doOperate issues one JSON API request and returns the raw success body.
// It owns the full error contract for operate verbs:
//
//   - authRequired + anonymous       → errAuthRequired (exit 3, no round trip)
//   - 401 while authenticated        → errSessionExpired (exit 3)
//   - any other non-2xx              → parseAPIError (structured envelope)
//   - transport failure              → wrapped "request failed"
func doOperate(method, url string, payload any, authRequired bool) ([]byte, error) {
	if authRequired && !haveAuth() {
		return nil, errAuthRequired("")
	}
	var rd io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized && haveAuth() {
		return nil, errSessionExpired()
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode > 299 {
		return nil, parseAPIError(resp.StatusCode, raw)
	}
	return raw, nil
}

// emitJSON pretty-prints v to stdout — the success-side --json surface.
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ── vault set / rotate ───────────────────────────────────────────────────────

var vaultCmd = newGroupCmd("vault", "Manage team vault secrets (vault://env/KEY refs for deploys)")

var vaultSetCmd = &cobra.Command{
	Use:     "set <env> <key>",
	Short:   "Write a secret to the team vault (PUT /api/v1/vault/:env/:key)",
	Example: "  instant vault set production STRIPE_KEY --value sk_live_…\n  cat secret.txt | instant vault set production STRIPE_KEY",
	Args:    cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return wrapJSONErr(cmd, runVaultWrite(cmd, args[0], args[1], false))
	},
}

var vaultRotateCmd = &cobra.Command{
	Use:     "rotate <env> <key>",
	Short:   "Rotate a vault secret's value (POST /api/v1/vault/:env/:key/rotate)",
	Example: "  instant vault rotate production STRIPE_KEY --value sk_live_new…",
	Args:    cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return wrapJSONErr(cmd, runVaultWrite(cmd, args[0], args[1], true))
	},
}

// vaultValue is bound to --value on vault set / rotate. Empty → read stdin
// (so secrets can be piped instead of landing in shell history).
var vaultValue string

// vaultWriteResult mirrors the api's {ok,key,env,version} write response.
// The plaintext value is never echoed back.
type vaultWriteResult struct {
	OK      bool   `json:"ok"`
	Key     string `json:"key"`
	Env     string `json:"env"`
	Version int    `json:"version"`
}

func runVaultWrite(cmd *cobra.Command, env, key string, rotate bool) error {
	value := vaultValue
	if value == "" {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return fmt.Errorf("reading secret value from stdin: %w", err)
		}
		value = strings.TrimRight(string(data), "\r\n")
	}
	if value == "" {
		return fmt.Errorf("secret value required — pass --value or pipe it on stdin")
	}
	method := http.MethodPut
	url := fmt.Sprintf("%s%s/%s/%s", APIBaseURL, vaultBasePath,
		neturl.PathEscape(env), neturl.PathEscape(key))
	if rotate {
		method = http.MethodPost
		url += vaultRotateSuffix
	}
	raw, err := doOperate(method, url, map[string]string{"value": value}, true)
	if err != nil {
		return err
	}
	var res vaultWriteResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if operateJSON {
		return emitJSON(res)
	}
	verb := "set"
	if rotate {
		verb = "rotated"
	}
	fmt.Printf("ok    vault %s  %s/%s  version %d\n", verb, res.Env, res.Key, res.Version)
	fmt.Printf("ref   vault://%s/%s\n", res.Env, res.Key)
	if rotate {
		fmt.Println("note  redeploy any app referencing this secret so the rotated value takes effect")
	}
	return nil
}

// ── env-patch (deploy + stack) ───────────────────────────────────────────────

var deployEnvCmd = &cobra.Command{
	Use:     "env <app-id> KEY=VALUE [KEY=VALUE…]",
	Short:   "Merge env vars into a deployment (PATCH /deploy/:id/env)",
	Example: "  instant deploy env my-app-x4f2 LOG_LEVEL=debug DB_URL=vault://production/DATABASE_URL",
	Args:    cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := fmt.Sprintf("%s%s/%s%s", APIBaseURL, deployBasePath,
			neturl.PathEscape(args[0]), deployEnvSuffix)
		return wrapJSONErr(cmd, runEnvPatch(url, args[1:]))
	},
}

var stackCmd = newGroupCmd("stack", "Manage multi-service stacks")

var stackEnvCmd = &cobra.Command{
	Use:     "env <slug> KEY=VALUE [KEY=VALUE…]",
	Short:   "Merge env vars into a stack (PATCH /stacks/:slug/env; empty value deletes the key)",
	Example: "  instant stack env my-stack LOG_LEVEL=debug OLD_KEY=",
	Args:    cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := fmt.Sprintf("%s%s/%s%s", APIBaseURL, stacksBasePath,
			neturl.PathEscape(args[0]), deployEnvSuffix)
		return wrapJSONErr(cmd, runEnvPatch(url, args[1:]))
	},
}

// parseEnvPairs turns CLI KEY=VALUE args into the request map. An empty
// VALUE is forwarded as-is (the stacks endpoint deletes that key). A pair
// without '=' or with an empty KEY is a usage error.
func parseEnvPairs(pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		i := strings.Index(kv, "=")
		if i <= 0 {
			return nil, fmt.Errorf("invalid env pair %q — expected KEY=VALUE", kv)
		}
		out[kv[:i]] = kv[i+1:]
	}
	return out, nil
}

// envPatchResult covers both PATCH variants: the deploy route returns `note`,
// the stack route returns `message`.
type envPatchResult struct {
	OK      bool              `json:"ok"`
	Env     map[string]string `json:"env"`
	Note    string            `json:"note"`
	Message string            `json:"message"`
}

func runEnvPatch(url string, pairs []string) error {
	envMap, err := parseEnvPairs(pairs)
	if err != nil {
		return err
	}
	raw, err := doOperate(http.MethodPatch, url, map[string]any{"env": envMap}, true)
	if err != nil {
		return err
	}
	var res envPatchResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if operateJSON {
		return emitJSON(res)
	}
	keys := make([]string, 0, len(res.Env))
	for k := range res.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("env   %s=%s\n", k, res.Env[k])
	}
	note := res.Note
	if note == "" {
		note = res.Message
	}
	if note == "" {
		note = "env merged — redeploy to apply the change to running pods"
	}
	fmt.Printf("note  %s\n", note)
	return nil
}

// ── deploy wake / events ─────────────────────────────────────────────────────

var deployWakeCmd = &cobra.Command{
	Use:     "wake <app-id>",
	Short:   "Wake a scaled-to-zero deployment (POST /deploy/:id/wake)",
	Example: "  instant deploy wake my-app-x4f2",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return wrapJSONErr(cmd, runDeployWake(args[0]))
	},
}

// wakeResult mirrors POST /deploy/:id/wake. The endpoint is flag-gated
// server-side (501 scale_to_zero_disabled when off) — that arrives via
// parseAPIError, not here.
type wakeResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func runDeployWake(id string) error {
	url := fmt.Sprintf("%s%s/%s%s", APIBaseURL, deployBasePath,
		neturl.PathEscape(id), deployWakeSuffix)
	raw, err := doOperate(http.MethodPost, url, nil, true)
	if err != nil {
		return err
	}
	var res wakeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if operateJSON {
		return emitJSON(res)
	}
	msg := res.Message
	if msg == "" {
		msg = "wake requested — the pod cold-starts before serving traffic"
	}
	fmt.Printf("ok    %s\n", msg)
	return nil
}

// deployEventsLimit is bound to --limit on `deploy events` (api default 50).
var deployEventsLimit int

var deployEventsCmd = &cobra.Command{
	Use:     "events <app-id>",
	Short:   "Show a deployment's failure timeline (GET /api/v1/deployments/:id/events)",
	Example: "  instant deploy events my-app-x4f2 --limit 10",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return wrapJSONErr(cmd, runDeployEvents(args[0]))
	},
}

// deploymentEvent is one autopsy row (rule 27 read surface).
type deploymentEvent struct {
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
	Event     string `json:"event"`
	LastLines string `json:"last_lines"`
	Hint      string `json:"hint"`
	ExitCode  *int   `json:"exit_code"`
	CreatedAt string `json:"created_at"`
}

type deployEventsResult struct {
	OK     bool              `json:"ok"`
	Events []deploymentEvent `json:"events"`
	Count  int               `json:"count"`
}

func runDeployEvents(id string) error {
	url := fmt.Sprintf("%s%s/%s%s", APIBaseURL, deploymentsBasePath,
		neturl.PathEscape(id), deployEventsSuffix)
	if deployEventsLimit > 0 {
		url = fmt.Sprintf("%s?limit=%d", url, deployEventsLimit)
	}
	raw, err := doOperate(http.MethodGet, url, nil, true)
	if err != nil {
		return err
	}
	var res deployEventsResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if operateJSON {
		return emitJSON(res)
	}
	if len(res.Events) == 0 {
		fmt.Println("no events — no failures recorded for this deployment")
		return nil
	}
	for _, e := range res.Events {
		exit := "-"
		if e.ExitCode != nil {
			exit = fmt.Sprintf("%d", *e.ExitCode)
		}
		fmt.Printf("%s  %s  %s  exit=%s\n", e.CreatedAt, e.Kind, e.Reason, exit)
		if e.Hint != "" {
			fmt.Printf("  hint: %s\n", e.Hint)
		}
		if e.LastLines != "" {
			fmt.Printf("  logs: %s\n", e.LastLines)
		}
	}
	fmt.Printf("count %d\n", res.Count)
	return nil
}

// ── storage presign ──────────────────────────────────────────────────────────

// presign flags. --key is required; --operation defaults to GET; --expires-in
// 0 means "omit" (server default 600s, capped at 3600).
var (
	presignKey       string
	presignOperation string
	presignExpiresIn int
)

var storagePresignCmd = &cobra.Command{
	Use:     "presign <token> --key <object-key>",
	Short:   "Mint a short-lived presigned S3 URL (POST /storage/:token/presign)",
	Example: "  instant storage presign 7f3a… --key reports/q2.pdf --operation PUT --expires-in 900",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return wrapJSONErr(cmd, runStoragePresign(args[0]))
	},
}

type presignResult struct {
	OK        bool   `json:"ok"`
	URL       string `json:"url"`
	Method    string `json:"method"`
	Key       string `json:"key"`
	ObjectKey string `json:"object_key"`
	ExpiresAt string `json:"expires_at"`
}

func runStoragePresign(token string) error {
	payload := map[string]any{
		"operation": presignOperation,
		"key":       presignKey,
	}
	if presignExpiresIn > 0 {
		payload["expires_in"] = presignExpiresIn
	}
	url := fmt.Sprintf("%s%s/%s%s", APIBaseURL, storageBasePath,
		neturl.PathEscape(token), storagePresignSuffix)
	// Auth-OPTIONAL: the storage token in the URL is the credential (broker
	// mode) — an anonymous caller can presign a prefix it just provisioned.
	raw, err := doOperate(http.MethodPost, url, payload, false)
	if err != nil {
		return err
	}
	var res presignResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if operateJSON {
		return emitJSON(res)
	}
	fmt.Printf("url      %s\n", res.URL)
	fmt.Printf("method   %s\n", res.Method)
	fmt.Printf("key      %s\n", res.ObjectKey)
	fmt.Printf("expires  %s\n", res.ExpiresAt)
	return nil
}

// ── capabilities ─────────────────────────────────────────────────────────────

var capabilitiesCmd = &cobra.Command{
	Use:   "capabilities",
	Short: "Show the live per-tier capability matrix (GET /api/v1/capabilities)",
	Long: `Show the live per-tier capability matrix.

Auth-optional: an anonymous agent can read this BEFORE provisioning to plan
a call ("is a 1 GB Mongo within the hobby cap?") instead of hitting a 402
mid-flow. The matrix comes from the api's plans registry (api/plans.yaml).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return wrapJSONErr(cmd, runCapabilities())
	},
}

// tierCapability is the per-tier row subset the CLI renders. --json emits
// the full raw response so nothing is lost for agents.
type tierCapability struct {
	Tier                string  `json:"tier"`
	DisplayName         string  `json:"display_name"`
	PriceUSDMonthly     float64 `json:"price_usd_monthly"`
	DeploymentsApps     int     `json:"deployments_apps"`
	BackupRetentionDays int     `json:"backup_retention_days"`
	ManualBackupsPerDay int     `json:"manual_backups_per_day"`
}

type capabilitiesResult struct {
	OK    bool             `json:"ok"`
	Tiers []tierCapability `json:"tiers"`
	Docs  string           `json:"docs"`
}

func runCapabilities() error {
	raw, err := doOperate(http.MethodGet, APIBaseURL+capabilitiesPath, nil, false)
	if err != nil {
		return err
	}
	if operateJSON {
		// Emit the api's full response verbatim — the CLI's table subset
		// must not lossy-filter the machine surface.
		var full any
		if err := json.Unmarshal(raw, &full); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}
		return emitJSON(full)
	}
	var res capabilitiesResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TIER\tPRICE/MO\tDEPLOY APPS\tBACKUP RETENTION\tMANUAL BACKUPS/DAY")
	for _, t := range res.Tiers {
		apps := fmt.Sprintf("%d", t.DeploymentsApps)
		if t.DeploymentsApps < 0 {
			apps = "unlimited"
		}
		_, _ = fmt.Fprintf(w, "%s\t$%.0f\t%s\t%dd\t%d\n",
			t.Tier, t.PriceUSDMonthly, apps, t.BackupRetentionDays, t.ManualBackupsPerDay)
	}
	// Flush error is ignored deliberately (matches runResourceDetail) — a
	// stdout-write failure has no recovery path in a CLI.
	_ = w.Flush()
	if res.Docs != "" {
		fmt.Printf("docs  %s\n", res.Docs)
	}
	return nil
}

// ── resource operate verbs (dispatched from resourceCmd in extras.go) ────────

// resourceActionResult is the superset of the pause / resume /
// rotate-credentials / backup-create response shapes; only the fields the
// server set are rendered.
type resourceActionResult struct {
	OK            bool   `json:"ok"`
	Status        string `json:"status"`
	Message       string `json:"message"`
	ConnectionURL string `json:"connection_url"`
	BackupID      string `json:"backup_id"`
}

// resourceVerbSuffix maps the CLI verb to the api path suffix. "backups"
// (list) is dispatched separately — it is a GET with a different shape.
var resourceVerbSuffix = map[string]string{
	"pause":  resourcePauseSuffix,
	"resume": resourceResumeSuffix,
	"rotate": resourceRotateSuffix,
	"backup": resourceBackupSuffix,
}

// runResourceOperate dispatches `instant resource <verb> <token>` for the
// operate verbs (pause / resume / rotate / backup / backups).
func runResourceOperate(verb, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("token is required")
	}
	if verb == "backups" {
		return runResourceBackupsList(token)
	}
	url := fmt.Sprintf("%s%s/%s%s", APIBaseURL, resourcesBasePath,
		neturl.PathEscape(token), resourceVerbSuffix[verb])
	raw, err := doOperate(http.MethodPost, url, nil, true)
	if err != nil {
		return err
	}
	var res resourceActionResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if resourceDetailJSON || operateJSON {
		return emitJSON(res)
	}
	fmt.Printf("ok    %s  %s\n", verb, token)
	if res.Status != "" {
		fmt.Printf("status  %s\n", res.Status)
	}
	if res.BackupID != "" {
		fmt.Printf("backup_id  %s\n", res.BackupID)
	}
	if res.ConnectionURL != "" {
		fmt.Printf("url   %s\n", res.ConnectionURL)
		fmt.Println("note  old credentials are revoked — update every consumer of this resource")
	}
	if res.Message != "" {
		fmt.Printf("note  %s\n", res.Message)
	}
	return nil
}

// ── resource creds (re-fetch the connection URL) ─────────────────────────────

// resourceCredentialsResult mirrors GET /api/v1/resources/:id/credentials
// (api/internal/handlers/resource.go GetCredentials). The real endpoint
// returns connection_url plus the resource identity; ReceiveURL is decoded
// too so a webhook-shaped response (receiver URL) still renders.
type resourceCredentialsResult struct {
	OK            bool   `json:"ok"`
	ID            string `json:"id"`
	Token         string `json:"token"`
	ResourceType  string `json:"resource_type"`
	Env           string `json:"env"`
	ConnectionURL string `json:"connection_url"`
	ReceiveURL    string `json:"receive_url"`
}

// runResourceCredentials re-fetches a resource's connection URL by token —
// the recovery path for a provision whose `new` call hit the 60s client
// timeout before printing the URL (the URL is otherwise unrecoverable). GETs
// /api/v1/resources/:token/credentials and prints the connection_url (or the
// webhook receive_url fallback); --json emits the full structured response.
func runResourceCredentials(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("token is required")
	}
	url := fmt.Sprintf("%s%s/%s%s", APIBaseURL, resourcesBasePath,
		neturl.PathEscape(token), resourceCredsSuffix)
	raw, err := doOperate(http.MethodGet, url, nil, true)
	if err != nil {
		return err
	}
	var res resourceCredentialsResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if resourceDetailJSON || operateJSON {
		return emitJSON(res)
	}
	// Webhooks have no connection_url — fall back to receive_url so the
	// receiver URL still surfaces (mirrors the provision + detail paths).
	connURL := res.ConnectionURL
	if connURL == "" {
		connURL = res.ReceiveURL
	}
	tok := res.Token
	if tok == "" {
		tok = token
	}
	fmt.Printf("ok    creds  %s\n", tok)
	if connURL != "" {
		fmt.Printf("url   %s\n", connURL)
	}
	if res.ResourceType != "" {
		fmt.Printf("type  %s\n", res.ResourceType)
	}
	if res.Env != "" {
		fmt.Printf("env   %s\n", res.Env)
	}
	return nil
}

// backupRow is one row from GET /api/v1/resources/:token/backups.
type backupRow struct {
	BackupID     string `json:"backup_id"`
	Status       string `json:"status"`
	BackupKind   string `json:"backup_kind"`
	SizeBytes    *int64 `json:"size_bytes"`
	CreatedAt    string `json:"created_at"`
	ErrorSummary string `json:"error_summary"`
}

type backupListResult struct {
	OK    bool        `json:"ok"`
	Items []backupRow `json:"items"`
	Total int         `json:"total"`
}

func runResourceBackupsList(token string) error {
	url := fmt.Sprintf("%s%s/%s%s", APIBaseURL, resourcesBasePath,
		neturl.PathEscape(token), resourceBackupsList)
	raw, err := doOperate(http.MethodGet, url, nil, true)
	if err != nil {
		return err
	}
	var res backupListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if resourceDetailJSON || operateJSON {
		return emitJSON(res)
	}
	if len(res.Items) == 0 {
		fmt.Println("no backups — run `instant resource backup <token>` to create one")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "BACKUP_ID\tSTATUS\tKIND\tSIZE\tCREATED\tERROR")
	for _, b := range res.Items {
		size := "-"
		if b.SizeBytes != nil {
			size = fmt.Sprintf("%d", *b.SizeBytes)
		}
		errSum := b.ErrorSummary
		if errSum == "" {
			errSum = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			b.BackupID, b.Status, b.BackupKind, size, b.CreatedAt, errSum)
	}
	_ = w.Flush()
	fmt.Printf("total %d\n", res.Total)
	return nil
}

// ── wiring ───────────────────────────────────────────────────────────────────

func init() {
	for _, c := range []*cobra.Command{vaultSetCmd, vaultRotateCmd} {
		c.Flags().StringVar(&vaultValue, "value", "",
			"Secret value (omit to read from stdin — keeps secrets out of shell history)")
		c.Flags().BoolVar(&operateJSON, "json", false,
			"Emit the API response as JSON")
	}
	vaultCmd.AddCommand(vaultSetCmd, vaultRotateCmd)
	rootCmd.AddCommand(vaultCmd)

	deployEventsCmd.Flags().IntVar(&deployEventsLimit, "limit", 0,
		"Max events to return (server default 50)")
	for _, c := range []*cobra.Command{deployEnvCmd, deployWakeCmd, deployEventsCmd} {
		c.Flags().BoolVar(&operateJSON, "json", false, "Emit the API response as JSON")
	}
	// deployCmd (deploy_stub.go) keeps its build-verb stubs; env / wake /
	// events are REAL implementations registered alongside them.
	deployCmd.AddCommand(deployEnvCmd, deployWakeCmd, deployEventsCmd)

	stackEnvCmd.Flags().BoolVar(&operateJSON, "json", false, "Emit the API response as JSON")
	stackCmd.AddCommand(stackEnvCmd)
	rootCmd.AddCommand(stackCmd)

	storagePresignCmd.Flags().StringVar(&presignKey, "key", "",
		"Object key relative to the tenant prefix (required; no leading slash, no '..')")
	storagePresignCmd.Flags().StringVar(&presignOperation, "operation", "GET",
		"S3 verb the signed URL authorizes: GET | PUT | HEAD")
	storagePresignCmd.Flags().IntVar(&presignExpiresIn, "expires-in", 0,
		"TTL in seconds (server default 600, capped at 3600)")
	storagePresignCmd.Flags().BoolVar(&operateJSON, "json", false, "Emit the API response as JSON")
	_ = storagePresignCmd.MarkFlagRequired("key")
	storageCmd.AddCommand(storagePresignCmd)

	capabilitiesCmd.Flags().BoolVar(&operateJSON, "json", false, "Emit the API response as JSON")
	rootCmd.AddCommand(capabilitiesCmd)
}
