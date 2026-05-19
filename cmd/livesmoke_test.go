//go:build livesmoke

package cmd

// livesmoke_test.go — OPTIONAL live-production smoke test.
//
// This file is excluded from normal builds and CI by the `livesmoke` build
// tag. It is NOT part of the mandatory gate; the hermetic suite is. Run it
// only when you want to verify the CLI against the real agent API:
//
//   go test ./cmd/... -tags livesmoke -run TestLiveSmoke -v -count=1
//
// It provisions a single anonymous-tier resource against the real API and
// then IMMEDIATELY tears it down. RESOURCE CLEANUP IS MANDATORY here too:
// the teardown runs in a defer, and the test fails if it cannot confirm the
// resource is gone. Anonymous resources also auto-expire (24h TTL) as a
// backstop, but the test never relies on that — it deletes explicitly.
//
// Override the target with INSTANT_API_URL (defaults to production).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// liveBaseURL resolves the API the smoke test runs against.
func liveBaseURL() string {
	if u := os.Getenv("INSTANT_API_URL"); u != "" {
		return u
	}
	return "https://api.instanode.dev"
}

// TestLiveSmoke_ProvisionThenTeardown provisions a real Postgres resource and
// deletes it. Failure of EITHER half fails the test.
func TestLiveSmoke_ProvisionThenTeardown(t *testing.T) {
	base := liveBaseURL()
	client := &http.Client{Timeout: 30 * time.Second}
	name := fmt.Sprintf("cli-smoke-%d", time.Now().UnixNano())

	// ── provision ──────────────────────────────────────────────────────────
	token := liveProvision(t, client, base, "/db/new", name)
	if token == "" {
		t.Fatal("live smoke: provision returned no token")
	}
	t.Logf("live smoke: provisioned %s (token=%s…)", name, safePrefix(token, 10))

	// ── MANDATORY teardown (defer) ─────────────────────────────────────────
	cleaned := false
	defer func() {
		if cleaned {
			return
		}
		// Last-ditch cleanup attempt if the explicit delete below was skipped.
		liveDelete(t, client, base, token)
	}()

	// Explicit delete + verification.
	if err := liveDeleteErr(client, base, token); err != nil {
		t.Errorf("live smoke: teardown failed for %s: %v", token, err)
		return
	}
	cleaned = true
	t.Logf("live smoke: torn down %s", safePrefix(token, 10))
}

func liveProvision(t *testing.T, client *http.Client, base, endpoint, name string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "env": "development"})
	resp, err := client.Post(base+endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("live smoke: provision request failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("live smoke: provision returned %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("live smoke: cannot parse provision response: %v", err)
	}
	if !out.OK {
		t.Fatalf("live smoke: provision ok=false: %s", raw)
	}
	return out.Token
}

func liveDeleteErr(client *http.Client, base, token string) error {
	req, err := http.NewRequest(http.MethodDelete, base+"/api/v1/resources/"+token, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete returned %d: %s", resp.StatusCode, raw)
	}
	return nil
}

func liveDelete(t *testing.T, client *http.Client, base, token string) {
	if err := liveDeleteErr(client, base, token); err != nil {
		t.Logf("live smoke: best-effort cleanup of %s failed: %v", token, err)
	}
}

func safePrefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
