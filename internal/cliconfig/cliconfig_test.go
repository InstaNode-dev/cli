package cliconfig

// White-box tests: same package so we can set the unexported `path` field
// directly, letting us redirect all file I/O to a t.TempDir() location.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InstaNode-dev/cli/internal/secretstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain installs an in-memory secret backend so cliconfig_test never
// touches the real OS keychain. Mirrors the cmd-package test setup.
func TestMain(m *testing.M) {
	secretstore.UseMemoryBackend()
	os.Exit(m.Run())
}

// resetSecretStore re-installs a clean in-memory secret backend. Called at
// the start of each test that depends on "nothing has been stored yet".
func resetSecretStore(t *testing.T) {
	t.Helper()
	secretstore.UseMemoryBackend()
}

// newTempConfig returns a Config whose path is set to a file inside t.TempDir().
// The file does NOT yet exist on disk.
func newTempConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{path: filepath.Join(t.TempDir(), "instant-config")}
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

func TestLoad_NonExistentFileReturnsEmptyConfig(t *testing.T) {
	resetSecretStore(t)
	// Point configPath at a path that definitely doesn't exist.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfg, err := Load()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, cfg.APIKey)
	assert.Empty(t, cfg.Email)
	assert.Empty(t, cfg.Tier)
}

// ---------------------------------------------------------------------------
// IsAuthenticated
// ---------------------------------------------------------------------------

func TestIsAuthenticated_FalseWhenAPIKeyEmpty(t *testing.T) {
	cfg := &Config{}
	assert.False(t, cfg.IsAuthenticated())
}

func TestIsAuthenticated_FalseOnNilConfig(t *testing.T) {
	var cfg *Config
	assert.False(t, cfg.IsAuthenticated())
}

func TestIsAuthenticated_TrueWhenAPIKeySet(t *testing.T) {
	cfg := &Config{APIKey: "inst_live_abc123"}
	assert.True(t, cfg.IsAuthenticated())
}

// ---------------------------------------------------------------------------
// EffectiveTier
// ---------------------------------------------------------------------------

func TestEffectiveTier_ReturnsAnonymousWhenEmpty(t *testing.T) {
	cfg := &Config{}
	assert.Equal(t, "anonymous", cfg.EffectiveTier())
}

func TestEffectiveTier_ReturnsAnonymousOnNilConfig(t *testing.T) {
	var cfg *Config
	assert.Equal(t, "anonymous", cfg.EffectiveTier())
}

func TestEffectiveTier_ReturnsTierWhenSet(t *testing.T) {
	for _, tier := range []string{"hobby", "pro", "team"} {
		cfg := &Config{Tier: tier}
		assert.Equal(t, tier, cfg.EffectiveTier(), "tier=%q", tier)
	}
}

// ---------------------------------------------------------------------------
// Save
// ---------------------------------------------------------------------------

func TestSave_WritesFileWithMode0600(t *testing.T) {
	cfg := newTempConfig(t)
	cfg.APIKey = "inst_live_savetest"
	cfg.Email = "save@example.com"
	cfg.Tier = "pro"

	require.NoError(t, cfg.Save())

	info, err := os.Stat(cfg.path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(),
		"config file must be mode 0600")
}

func TestSave_FileIsValidJSON(t *testing.T) {
	cfg := newTempConfig(t)
	cfg.APIKey = "inst_live_jsontest"

	require.NoError(t, cfg.Save())

	data, err := os.ReadFile(cfg.path)
	require.NoError(t, err)
	// After the T16 P1-1 fix, the legacy `"api_key"` field is no longer
	// written to disk — the bearer token routes through secretstore (the
	// in-memory backend during tests). The on-disk file must contain ONLY
	// the non-secret display fields.
	assert.NotContains(t, string(data), `"api_key"`,
		"plaintext api_key must NOT be written to disk when secretstore is available")
	assert.NotContains(t, string(data), "inst_live_jsontest",
		"plaintext api_key value must not appear on disk")
	// And it must be syntactically valid JSON.
	assert.Contains(t, string(data), `"saved_at"`)
}

// TestSave_NoPlaintextAPIKeyOnDisk_KeychainBackend is the explicit T16 P1-1
// regression assertion: with a working secret backend in place, a saved
// config has zero traces of the bearer token in its on-disk JSON.
func TestSave_NoPlaintextAPIKeyOnDisk_KeychainBackend(t *testing.T) {
	cfg := newTempConfig(t)
	cfg.APIKey = "inst_live_secret_that_must_not_leak"
	cfg.Email = "x@example.com"

	require.NoError(t, cfg.Save())

	data, err := os.ReadFile(cfg.path)
	require.NoError(t, err)

	body := string(data)
	if strings.Contains(body, "inst_live_secret_that_must_not_leak") {
		t.Fatalf("BUG: bearer token leaked to disk: %s", body)
	}
	if strings.Contains(body, FallbackAPIKeyField) {
		t.Fatalf("BUG: fallback field written despite keychain being available: %s", body)
	}
}

// TestSave_FallbackOnDiskWhenKeychainMissing asserts the disk fallback path:
// if secretstore.Set fails (no backend), the key MUST land in
// FallbackAPIKey + survive a Load round-trip. Sanity-checks the disk
// fallback for headless Linux / CI environments.
func TestSave_FallbackOnDiskWhenKeychainMissing(t *testing.T) {
	prev := secretstore.Name()
	secretstore.Use(nil) // simulates "no keychain"
	t.Cleanup(func() {
		if prev == "memory" {
			secretstore.UseMemoryBackend()
		}
	})

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfg := &Config{path: filepath.Join(dir, ".instant-config"),
		APIKey: "inst_live_fallback_secret", Email: "fb@example.com"}
	require.NoError(t, cfg.Save())

	data, err := os.ReadFile(cfg.path)
	require.NoError(t, err)
	body := string(data)
	assert.Contains(t, body, FallbackAPIKeyField,
		"fallback field MUST be present when no keychain backend is wired")
	assert.Contains(t, body, "inst_live_fallback_secret",
		"fallback path must persist the key on disk (warning emitted to stderr)")
	// File mode must still be 0600 even on the fallback path.
	info, err := os.Stat(cfg.path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

	// Round-trip: Load must restore APIKey from FallbackAPIKey.
	loaded, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "inst_live_fallback_secret", loaded.APIKey)
	assert.Equal(t, "file-fallback", loaded.SecretBackendName())
}

// TestLegacyAPIKeyMigratesOnNextSave covers the upgrade path: an existing
// install with `"api_key"` on disk (pre-2026-05-20) must be migrated into
// secretstore on the next Save() and the legacy field cleared.
func TestLegacyAPIKeyMigratesOnNextSave(t *testing.T) {
	secretstore.UseMemoryBackend()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, ".instant-config")

	// Write a config file the OLD way (legacy api_key field).
	legacy := `{"api_key":"inst_live_legacy123","email":"legacy@example.com"}`
	require.NoError(t, os.WriteFile(path, []byte(legacy), 0600))

	loaded, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "inst_live_legacy123", loaded.APIKey,
		"Load must pick up the legacy api_key field")
	assert.Equal(t, "inst_live_legacy123", loaded.LegacyAPIKey,
		"the legacy field is preserved on the struct until next Save")

	// Save migrates: legacy field cleared, secretstore now holds the key.
	loaded.path = path
	require.NoError(t, loaded.Save())

	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(disk), `"api_key"`,
		"after Save, the legacy api_key field must be gone from disk")

	v, err := secretstore.Get()
	require.NoError(t, err)
	assert.Equal(t, "inst_live_legacy123", v,
		"after Save, secretstore must hold the migrated key")
}

func TestSave_UpdatesSavedAt(t *testing.T) {
	cfg := newTempConfig(t)
	before := time.Now().UTC().Add(-time.Second)

	require.NoError(t, cfg.Save())

	assert.True(t, cfg.SavedAt.After(before),
		"SavedAt must be set to approximately now after Save()")
}

// ---------------------------------------------------------------------------
// Clear
// ---------------------------------------------------------------------------

func TestClear_RemovesExistingFile(t *testing.T) {
	resetSecretStore(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Create the file first via Save.
	path := filepath.Join(dir, ".instant-config")
	cfg := &Config{path: path, APIKey: "inst_live_clear"}
	require.NoError(t, cfg.Save())

	_, err := os.Stat(path)
	require.NoError(t, err, "file must exist before Clear")

	// Clear uses configPath() → HOME/.instant-config, which is our temp path.
	require.NoError(t, Clear())

	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file must be gone after Clear")
}

func TestClear_NoErrorWhenFileAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// The file was never created; Clear must succeed silently.
	require.NoError(t, Clear())
}

// ---------------------------------------------------------------------------
// Round-trip: Save → Load
// ---------------------------------------------------------------------------

func TestRoundTrip_SaveThenLoadReturnsSameStruct(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	original := &Config{
		APIKey:     "inst_live_roundtrip",
		Email:      "rt@example.com",
		Tier:       "team",
		TeamName:   "Acme",
		APIBaseURL: "https://api.staging.instanode.dev",
	}
	// Use the HOME-based path so Load() can find it.
	original.path = filepath.Join(dir, ".instant-config")
	require.NoError(t, original.Save())

	loaded, err := Load()
	require.NoError(t, err)
	require.NotNil(t, loaded)

	assert.Equal(t, original.APIKey, loaded.APIKey)
	assert.Equal(t, original.Email, loaded.Email)
	assert.Equal(t, original.Tier, loaded.Tier)
	assert.Equal(t, original.TeamName, loaded.TeamName)
	assert.Equal(t, original.APIBaseURL, loaded.APIBaseURL)
	// SavedAt is set by Save(); it should be non-zero after the round-trip.
	assert.False(t, loaded.SavedAt.IsZero(), "SavedAt must survive the round-trip")
}

func TestRoundTrip_IsAuthenticatedAfterLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfg := &Config{
		path:   filepath.Join(dir, ".instant-config"),
		APIKey: "inst_live_auth",
	}
	require.NoError(t, cfg.Save())

	loaded, err := Load()
	require.NoError(t, err)
	assert.True(t, loaded.IsAuthenticated())
}

func TestRoundTrip_EffectiveTierAfterLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfg := &Config{
		path: filepath.Join(dir, ".instant-config"),
		Tier: "hobby",
	}
	require.NoError(t, cfg.Save())

	loaded, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "hobby", loaded.EffectiveTier())
}
