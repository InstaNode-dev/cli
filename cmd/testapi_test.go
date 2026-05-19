package cmd

// testapi_test.go — a hermetic, stateful mock of the instanode.dev agent API.
//
// It emulates the documented contract from https://api.instanode.dev/openapi.json
// closely enough to drive every CLI command end-to-end with ZERO network
// access. The server is stateful: provisioning records a resource, the
// resources list reflects it, credentials are retrievable by token, and a
// DELETE removes it. This statefulness is what lets the integration suite
// PROVE that resources are torn down — the final sweep asserts the server
// has no leftover resources.
//
// Endpoints emulated:
//   POST   /db/new, /cache/new, /nosql/new, /queue/new, /storage/new, /webhook/new
//   GET    /api/v1/resources                  (optionally ?env=)
//   GET    /api/v1/resources/:token/credentials
//   DELETE /api/v1/resources/:token           (cleanup)
//   POST   /auth/cli, GET /auth/cli/:id        (login flow)
//   GET    /auth/me                            (tier poll)

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockResource is one provisioned resource held by the fake API.
type mockResource struct {
	ID            string `json:"id"`
	Token         string `json:"token"`
	ResourceType  string `json:"resource_type"`
	Name          string `json:"name"`
	Env           string `json:"env"`
	Tier          string `json:"tier"`
	Status        string `json:"status"`
	ConnectionURL string `json:"-"`
	ReceiveURL    string `json:"-"`
}

// mockAPI is a stateful in-memory fake of the agent API. It is safe for
// concurrent use — the CLI's HTTP client may issue overlapping requests.
type mockAPI struct {
	mu        sync.Mutex
	resources map[string]*mockResource // keyed by token
	// requireAuth, when true, makes every provisioning endpoint return 401
	// unless an Authorization header is present — used by auth tests.
	requireAuth bool
	// failProvision, when set, makes the next provision return this status.
	failProvisionStatus int
	// failListStatus, when set, makes GET /api/v1/resources return this
	// status (without consuming it — every list request fails until the
	// caller resets it). Used to drive T16 P1-4 regression tests where the
	// CLI MUST abort rather than silently re-provision on list failure.
	failListStatus int
	// connURLOverride lets a test mint a custom connection_url (hostile
	// values for the T16 P1-5 shell-quoting regression). Empty == default.
	connURLOverride string
	// authToken is the bearer token the server expects when requireAuth is on.
	authToken string
	// authComplete drives the /auth/cli/:id poll: false => 202 pending,
	// true => 200 with an authResult.
	authComplete bool
}

// newMockAPI starts an httptest.Server backed by a fresh mockAPI and returns
// both. The caller closes srv (t.Cleanup recommended).
func newMockAPI(t *testing.T) (*mockAPI, *httptest.Server) {
	t.Helper()
	m := &mockAPI{resources: map[string]*mockResource{}}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return m, srv
}

// resourceTypeForEndpoint maps a provisioning endpoint to its stored type.
var endpointResourceType = map[string]string{
	"/db/new":      "postgres",
	"/cache/new":   "redis",
	"/nosql/new":   "mongodb",
	"/queue/new":   "queue",
	"/storage/new": "storage",
	"/webhook/new": "webhook",
}

// count returns the number of resources currently held (used by the sweep).
func (m *mockAPI) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.resources)
}

// names returns the names of all held resources (for diagnostics).
func (m *mockAPI) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.resources))
	for _, r := range m.resources {
		out = append(out, r.ResourceType+":"+r.Name)
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *mockAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// ── provisioning endpoints ────────────────────────────────────────────
	if rtype, ok := endpointResourceType[path]; ok && r.Method == http.MethodPost {
		m.handleProvision(w, r, rtype)
		return
	}

	// ── resources list ────────────────────────────────────────────────────
	if path == "/api/v1/resources" && r.Method == http.MethodGet {
		m.handleListResources(w, r)
		return
	}

	// ── credentials / delete by token ─────────────────────────────────────
	if strings.HasPrefix(path, "/api/v1/resources/") {
		rest := strings.TrimPrefix(path, "/api/v1/resources/")
		if strings.HasSuffix(rest, "/credentials") && r.Method == http.MethodGet {
			m.handleCredentials(w, strings.TrimSuffix(rest, "/credentials"))
			return
		}
		if r.Method == http.MethodDelete && !strings.Contains(rest, "/") {
			m.handleDelete(w, rest)
			return
		}
	}

	// ── auth/cli session ──────────────────────────────────────────────────
	if path == "/auth/cli" && r.Method == http.MethodPost {
		writeJSON(w, http.StatusOK, map[string]string{
			"session_id": "sess_" + randHex(8),
			"auth_url":   "https://instanode.dev/cli-auth?s=test",
		})
		return
	}
	if strings.HasPrefix(path, "/auth/cli/") && r.Method == http.MethodGet {
		m.mu.Lock()
		done := m.authComplete
		m.mu.Unlock()
		if !done {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"api_key":        "inst_test_" + randHex(12),
			"email":          "tester@instanode.dev",
			"tier":           "hobby",
			"team_name":      "Test Team",
			"claimed_tokens": []string{},
		})
		return
	}
	if path == "/auth/me" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{
			"tier": "pro", "email": "tester@instanode.dev", "team_name": "Test Team",
		})
		return
	}

	http.NotFound(w, r)
}

func (m *mockAPI) handleProvision(w http.ResponseWriter, r *http.Request, rtype string) {
	m.mu.Lock()
	if m.failProvisionStatus != 0 {
		status := m.failProvisionStatus
		m.failProvisionStatus = 0
		m.mu.Unlock()
		writeJSON(w, status, map[string]any{
			"ok":           false,
			"error":        "simulated provisioning failure",
			"agent_action": "retry or upgrade",
		})
		return
	}
	requireAuth := m.requireAuth
	wantToken := m.authToken
	m.mu.Unlock()

	if requireAuth {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || (wantToken != "" && got != wantToken) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"ok": false, "error": "authentication required",
			})
			return
		}
	}

	var body struct {
		Name string `json:"name"`
		Env  string `json:"env"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)

	if strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "name is required",
		})
		return
	}

	// Resolved-env default: empty env lands in "development" (CLAUDE.md rule 11
	// — migration 026). The mock echoes the resolved env so tests can assert it.
	env := body.Env
	if env == "" {
		env = "development"
	}

	token := "tok_" + randHex(10)
	id := "res_" + randHex(8)
	res := &mockResource{
		ID: id, Token: token, ResourceType: rtype, Name: body.Name,
		Env: env, Tier: "anonymous", Status: "active",
	}

	resp := map[string]any{
		"ok": true, "token": token, "name": body.Name,
		"tier": "anonymous", "env": env,
	}
	if rtype == "webhook" {
		res.ReceiveURL = "https://hooks.instanode.dev/" + token
		resp["receive_url"] = res.ReceiveURL
	} else {
		m.mu.Lock()
		override := m.connURLOverride
		m.mu.Unlock()
		if override != "" {
			res.ConnectionURL = override
		} else {
			res.ConnectionURL = mockConnURL(rtype, token)
		}
		resp["connection_url"] = res.ConnectionURL
	}

	m.mu.Lock()
	m.resources[token] = res
	m.mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

// mockConnURL produces a plausible per-type connection URL.
func mockConnURL(rtype, token string) string {
	switch rtype {
	case "postgres":
		return "postgres://u:p@db.instanode.dev:5432/" + token
	case "redis":
		return "redis://u:p@cache.instanode.dev:6379"
	case "mongodb":
		return "mongodb://u:p@nosql.instanode.dev:27017/" + token
	case "queue":
		return "nats://queue.instanode.dev:4222"
	case "storage":
		return "https://s3.instanode.dev/" + token
	default:
		return "https://instanode.dev/" + token
	}
}

func (m *mockAPI) handleListResources(w http.ResponseWriter, r *http.Request) {
	// Forced-failure path (T16 P1-4 regression): the test wants to assert
	// that `up` aborts on a list-fetch failure instead of provisioning blind.
	m.mu.Lock()
	failStatus := m.failListStatus
	m.mu.Unlock()
	if failStatus != 0 {
		writeJSON(w, failStatus, map[string]any{
			"ok": false, "error": "simulated list failure",
		})
		return
	}
	if m.requireAuth {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || (m.authToken != "" && got != m.authToken) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"ok": false, "error": "authentication required",
			})
			return
		}
	}
	wantEnv := r.URL.Query().Get("env")

	m.mu.Lock()
	items := make([]map[string]any, 0, len(m.resources))
	for _, res := range m.resources {
		if wantEnv != "" && res.Env != wantEnv {
			continue
		}
		items = append(items, map[string]any{
			"id": res.ID, "token": res.Token, "resource_type": res.ResourceType,
			"name": res.Name, "env": res.Env, "tier": res.Tier, "status": res.Status,
		})
	}
	m.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": len(items), "items": items,
	})
}

func (m *mockAPI) handleCredentials(w http.ResponseWriter, token string) {
	m.mu.Lock()
	res := m.resources[token]
	m.mu.Unlock()
	if res == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}
	if res.ConnectionURL == "" {
		// Webhooks have no connection_url — mirror the real API.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": "resource has no connection_url",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "connection_url": res.ConnectionURL,
	})
}

func (m *mockAPI) handleDelete(w http.ResponseWriter, token string) {
	m.mu.Lock()
	_, existed := m.resources[token]
	delete(m.resources, token)
	m.mu.Unlock()
	if !existed {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": token})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── deadline guard ──────────────────────────────────────────────────────────
// pollInterval / pollTimeout in login.go are long; tests that exercise the
// poll path keep authComplete=true so the first poll succeeds immediately.
var _ = time.Second
