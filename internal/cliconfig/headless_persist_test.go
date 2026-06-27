package cliconfig

// White-box tests for the headless/detached login token-persistence fix.
//
// Root cause being guarded: on a detached/headless box the OS keychain's
// Available() probe is a false positive — Set() returns nil even though the
// write lands in an ephemeral/locked session keyring the next process can't
// read. Because Set didn't error, the old Save() never took the file-fallback
// branch, so `instant login` printed "✓ Logged in" while persisting NOTHING
// the next `instant whoami` could read.
//
// The fix verifies the keychain write with an immediate read-back and falls
// back to the 0600 ~/.instant-config file when the value doesn't round-trip.
// These tests inject the secretSet/secretGet seams to simulate that exact
// silent-write failure WITHOUT touching the real OS keychain.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSecretSeams installs replacement secretSet/secretGet implementations for
// the duration of one test and restores the originals on cleanup.
func withSecretSeams(t *testing.T, set func(string) error, get func() (string, error)) {
	t.Helper()
	prevSet, prevGet := secretSet, secretGet
	secretSet, secretGet = set, get
	t.Cleanup(func() { secretSet, secretGet = prevSet, prevGet })
}

// TestSave_SilentKeychainWriteFallsBackToFile is the core regression test for
// the detached/headless login bug. The injected keychain ACCEPTS the write
// (Set returns nil) but the read-back returns a DIFFERENT value (the
// ephemeral/locked-session keyring the next process can't read). Save must
// detect the non-durable write and persist the token to the 0600 file
// fallback, and a fresh Load must return it — i.e. the next `instant whoami`
// finds the token.
func TestSave_SilentKeychainWriteFallsBackToFile(t *testing.T) {
	// Set "succeeds" but Get never returns what we wrote — the silent
	// detached-keychain failure mode.
	withSecretSeams(t,
		func(string) error { return nil },         // Set: false success
		func() (string, error) { return "", nil }, // Get: empty (write evaporated)
	)

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, ".instant-config")

	cfg := &Config{path: path, APIKey: "inst_live_headless_secret", Email: "agent@example.com"}
	require.NoError(t, cfg.Save())

	// The token MUST have landed in the on-disk fallback field.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(data)
	assert.Contains(t, body, FallbackAPIKeyField,
		"silent keychain write must trigger the file fallback")
	assert.Contains(t, body, "inst_live_headless_secret",
		"token must be persisted to disk when the keychain write doesn't round-trip")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(),
		"file fallback must stay mode 0600")

	// And the next invocation (whoami / authed call) must read it back.
	// Load consults secretstore first; our seam still returns empty there, so
	// the value can only come from the on-disk fallback — exactly the path
	// `instant whoami` exercises after a headless login.
	loaded, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "inst_live_headless_secret", loaded.APIKey,
		"a file-persisted token must be found on the next load (no false 'Not logged in')")
	assert.True(t, loaded.IsAuthenticated())
	assert.Equal(t, "file-fallback", loaded.SecretBackendName())
}

// TestSave_KeychainSetErrorFallsBackToFile covers the OTHER headless mode:
// Set itself errors (no DBus / locked collection / no active backend). The
// file fallback must still capture the token.
func TestSave_KeychainSetErrorFallsBackToFile(t *testing.T) {
	withSecretSeams(t,
		func(string) error { return errors.New("keychain set: dbus unavailable") },
		func() (string, error) { return "", errors.New("unreachable") },
	)

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, ".instant-config")

	cfg := &Config{path: path, APIKey: "inst_live_set_errored", Email: "ci@example.com"}
	require.NoError(t, cfg.Save())

	loaded, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "inst_live_set_errored", loaded.APIKey)
}

// TestSave_DurableKeychainWriteSkipsFileFallback is the happy-path guard: when
// the keychain accepts the write AND it round-trips on read-back, the token
// must NOT be written to disk (no plaintext leak — the T16 P1-1 property), and
// persistSecret reports success.
func TestSave_DurableKeychainWriteSkipsFileFallback(t *testing.T) {
	var stored string
	withSecretSeams(t,
		func(v string) error { stored = v; return nil },
		func() (string, error) { return stored, nil }, // faithful round-trip
	)

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, ".instant-config")

	cfg := &Config{path: path, APIKey: "inst_live_durable", Email: "desktop@example.com"}
	require.NoError(t, cfg.Save())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(data)
	assert.NotContains(t, body, FallbackAPIKeyField,
		"a durable keychain write must NOT write the file-fallback field")
	assert.NotContains(t, body, "inst_live_durable",
		"a durable keychain write must NOT leave the token in plaintext on disk")
	assert.Empty(t, cfg.FallbackAPIKey)
}

// TestPersistSecret_TableDrivenOutcomes exercises persistSecret's three
// outcomes directly so each branch (Set error / readback mismatch / readback
// error / durable success) is unambiguously covered.
func TestPersistSecret_TableDrivenOutcomes(t *testing.T) {
	cases := []struct {
		name string
		set  func(string) error
		get  func() (string, error)
		want bool
	}{
		{
			name: "set error -> not durable",
			set:  func(string) error { return errors.New("boom") },
			get:  func() (string, error) { return "tok", nil },
			want: false,
		},
		{
			name: "readback mismatch -> not durable",
			set:  func(string) error { return nil },
			get:  func() (string, error) { return "different", nil },
			want: false,
		},
		{
			name: "readback error -> not durable",
			set:  func(string) error { return nil },
			get:  func() (string, error) { return "", errors.New("locked") },
			want: false,
		},
		{
			name: "faithful round-trip -> durable",
			set:  func(string) error { return nil },
			get:  func() (string, error) { return "tok", nil },
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withSecretSeams(t, tc.set, tc.get)
			assert.Equal(t, tc.want, persistSecret("tok"))
		})
	}
}
