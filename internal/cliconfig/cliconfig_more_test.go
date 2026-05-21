package cliconfig

// More targeted cliconfig coverage to close the Save / Load gap.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/InstaNode-dev/cli/internal/secretstore"
)

// TestLoad_LegacyAPIKeyPath covers the LegacyAPIKey branch in Load that
// triggers when the secretstore is empty and there's no FallbackAPIKey but
// the file does have a legacy `api_key` field.
func TestLoad_LegacyAPIKeyPathOnly(t *testing.T) {
	// No secretstore (force file fallback path).
	secretstore.Use(nil)
	t.Cleanup(func() { secretstore.UseMemoryBackend() })

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, ".instant-config")
	if err := os.WriteFile(path, []byte(`{"api_key":"legacy-only"}`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKey != "legacy-only" {
		t.Errorf("APIKey = %q", cfg.APIKey)
	}
}

// TestSave_PathResolveAndAtomicRename exercises the temp-file -> rename path.
func TestSave_AtomicRename(t *testing.T) {
	secretstore.UseMemoryBackend()
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfg := &Config{APIKey: "atomic"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Confirm tmp file was renamed away (only final file remains).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == ".instant-config.tmp" {
			t.Errorf(".tmp file should be renamed away")
		}
	}
}
