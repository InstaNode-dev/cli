package cliconfig

// coverage_test.go — exercises the Load / Save / Clear / SecretBackendName
// branches the existing suite leaves uncovered. White-box (same package) so
// we can redirect file I/O via HOME and the unexported path field, and uses
// the in-memory secret backend installed by TestMain so the real OS keychain
// is never touched.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/InstaNode-dev/cli/internal/secretstore"
)

func TestSecretBackendName_Branches(t *testing.T) {
	resetSecretStore(t)

	// No API key -> "none".
	if got := (&Config{}).SecretBackendName(); got != "none" {
		t.Errorf("empty -> %q", got)
	}
	// Nil config -> "none".
	if got := (*Config)(nil).SecretBackendName(); got != "none" {
		t.Errorf("nil -> %q", got)
	}
	// Fallback key present -> "file-fallback".
	if got := (&Config{APIKey: "k", FallbackAPIKey: "k"}).SecretBackendName(); got != "file-fallback" {
		t.Errorf("fallback -> %q", got)
	}
	// Otherwise -> the backend's Name() (in tests: the memory backend).
	if got := (&Config{APIKey: "k"}).SecretBackendName(); got != secretstore.Name() {
		t.Errorf("backend -> %q want %q", got, secretstore.Name())
	}
}

func TestLoad_ResolvesKeyFromSecretStore(t *testing.T) {
	resetSecretStore(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Write a config with no key on disk; stash the key in the secretstore.
	cfg := &Config{path: filepath.Join(dir, ".instant-config"), Tier: "pro"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	if err := secretstore.Set("sk_from_store"); err != nil {
		t.Fatalf("set: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.APIKey != "sk_from_store" {
		t.Errorf("APIKey = %q, want from secretstore", loaded.APIKey)
	}
	if loaded.Tier != "pro" {
		t.Errorf("Tier = %q", loaded.Tier)
	}
}

func TestLoad_FallbackKeyWhenStoreEmpty(t *testing.T) {
	resetSecretStore(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// On-disk fallback key, empty secretstore -> fallback wins.
	path := filepath.Join(dir, ".instant-config")
	if err := os.WriteFile(path, []byte(`{"api_key_fallback":"sk_fallback","tier":"hobby"}`), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.APIKey != "sk_fallback" {
		t.Errorf("APIKey = %q, want fallback", loaded.APIKey)
	}
}

func TestLoad_ParseErrorOnCorruptFile(t *testing.T) {
	resetSecretStore(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path := filepath.Join(dir, ".instant-config")
	if err := os.WriteFile(path, []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("expected parse error on corrupt config")
	}
}

func TestSave_LogoutClearsSecretStore(t *testing.T) {
	resetSecretStore(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Seed a stored key, then Save a config with an empty APIKey: the empty
	// branch must Delete from the secretstore.
	if err := secretstore.Set("sk_old"); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{path: filepath.Join(dir, ".instant-config")}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if v, err := secretstore.Get(); err == nil && v != "" {
		t.Errorf("expected secretstore cleared, got %q", v)
	}
}

func TestClear_ClearsStoreAndFile(t *testing.T) {
	resetSecretStore(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfg := &Config{path: filepath.Join(dir, ".instant-config"), APIKey: "sk"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := os.Stat(cfg.path); !os.IsNotExist(err) {
		t.Error("config file should be removed after Clear")
	}
	// Clear again is idempotent (no file present).
	if err := Clear(); err != nil {
		t.Errorf("second Clear should be a no-op, got %v", err)
	}
}
