// Package cliconfig manages the user's CLI credentials in ~/.instant-config.
//
// SECURITY MODEL (post-2026-05-20, T16 P1-1 fix)
//
// The user's API key is stored in the OS keychain via the secretstore
// package — macOS Keychain, Linux Secret Service / libsecret, or Windows
// Credential Manager. When no keychain is available (headless Linux, CI,
// sandboxed environments) we fall back to writing the key into
// ~/.instant-config mode 0600 and print a one-time stderr warning so the
// user knows their bearer token is on disk.
//
// ~/.instant-config itself always stores the non-secret display fields
// (email, plan tier, team name, API base URL, last-saved timestamp) so
// `instant whoami` can still answer offline without prompting the keychain.
package cliconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/instant-dev/cli/internal/secretstore"
)

// ErrNotLoggedIn is returned when an action requires authentication
// but no API key is found.
var ErrNotLoggedIn = errors.New("not logged in — run `instant login` to authenticate")

// FallbackAPIKeyField is the JSON field name we use when the keychain is
// unavailable and we have to write the bearer token to ~/.instant-config.
// Tests assert that this field is ABSENT when the keychain backend is in
// use (T16 P1-1 regression).
const FallbackAPIKeyField = "api_key_fallback"

// Config holds the authenticated user's non-secret display data plus an
// in-memory copy of the API key loaded from the secretstore (or, on the
// file-fallback path, from disk).
//
// The on-disk JSON shape includes only:
//   - non-secret fields (email, tier, team_name, api_base_url, saved_at)
//   - api_key_fallback (set ONLY when the keychain is unavailable)
//
// The legacy `api_key` JSON field is no longer written. We still READ it
// on Load() so existing installs are migrated transparently into the
// keychain on the next Save().
type Config struct {
	// APIKey is the bearer token sent with every authenticated API request.
	// In-memory only; persistence routes through secretstore.
	// Empty = anonymous mode.
	APIKey string `json:"-"`

	// LegacyAPIKey captures any value found in the old `api_key` field on
	// disk so we can migrate it to the keychain on Save().
	LegacyAPIKey string `json:"api_key,omitempty"`

	// FallbackAPIKey is written ONLY when the keychain is unavailable and
	// we must store the secret on disk. Tests assert this is empty when
	// the keychain is in use.
	FallbackAPIKey string `json:"api_key_fallback,omitempty"`

	// Email is the account email, stored for display in `instant whoami`.
	Email string `json:"email,omitempty"`

	// Tier is the current plan: "anonymous", "hobby", "pro", "team".
	Tier string `json:"tier,omitempty"`

	// TeamName is the team name, if the user belongs to a team.
	TeamName string `json:"team_name,omitempty"`

	// APIBaseURL overrides the default https://api.instanode.dev endpoint.
	// Populated when INSTANT_API_URL env var was set at login time.
	APIBaseURL string `json:"api_base_url,omitempty"`

	// SavedAt is when the config was last written (for staleness checks).
	SavedAt time.Time `json:"saved_at,omitempty"`

	path string // resolved path, not serialised
}

// fallbackWarnedOnce ensures the "key stored on disk" warning is printed
// at most once per process.
var fallbackWarnedOnce sync.Once

// warnFileFallback writes a one-time stderr warning explaining that the
// user's API key is on disk because the OS keychain is unavailable.
func warnFileFallback() {
	fallbackWarnedOnce.Do(func() {
		fmt.Fprintln(os.Stderr,
			"warning: OS keychain unavailable — API key stored in ~/.instant-config (mode 0600). "+
				"On headless Linux install libsecret or set DBUS_SESSION_BUS_ADDRESS. "+
				"To suppress this warning, set INSTANT_DISABLE_KEYCHAIN=1.")
	})
}

// IsAuthenticated reports whether the config holds valid credentials.
func (c *Config) IsAuthenticated() bool {
	return c != nil && c.APIKey != ""
}

// EffectiveTier returns the tier string, defaulting to "anonymous".
func (c *Config) EffectiveTier() string {
	if c == nil || c.Tier == "" {
		return "anonymous"
	}
	return c.Tier
}

// SecretBackendName surfaces which secret backend is in use (for `whoami`
// to truthfully report "Key stored in: macOS Keychain" vs "file").
func (c *Config) SecretBackendName() string {
	if c == nil || c.APIKey == "" {
		return "none"
	}
	if c.FallbackAPIKey != "" {
		return "file-fallback"
	}
	return secretstore.Name()
}

// Load reads ~/.instant-config from disk and, where the keychain is
// available, also reads the API key out of the secretstore.
//
// Migration: if the legacy `api_key` field is present on disk (a config
// written before this fix) we load it into APIKey and clear it on the
// next Save() — keychain takes over.
func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return &Config{path: ""}, nil
	}

	cfg := &Config{path: path}
	data, fileErr := os.ReadFile(path)
	switch {
	case os.IsNotExist(fileErr):
		// no file is fine — anonymous mode
	case fileErr != nil:
		return nil, fmt.Errorf("reading %s: %w", path, fileErr)
	default:
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		cfg.path = path
	}

	// Resolve the API key from, in priority order:
	//   1. secretstore (OS keychain) when available
	//   2. on-disk fallback field (keychain was unavailable last write)
	//   3. legacy on-disk field (pre-2026-05-20 installs — migrated on next Save)
	if val, err := secretstore.Get(); err == nil && val != "" {
		cfg.APIKey = val
	} else if cfg.FallbackAPIKey != "" {
		cfg.APIKey = cfg.FallbackAPIKey
	} else if cfg.LegacyAPIKey != "" {
		cfg.APIKey = cfg.LegacyAPIKey
	}

	return cfg, nil
}

// Save writes the non-secret fields to disk and routes the API key into
// the secretstore. If the secretstore Set fails (no keychain), the key
// falls back to ~/.instant-config and a one-time stderr warning is emitted.
//
// The on-disk file is always written mode 0600 via an atomic temp+rename.
func (c *Config) Save() error {
	if c.path == "" {
		path, err := configPath()
		if err != nil {
			return err
		}
		c.path = path
	}
	c.SavedAt = time.Now().UTC()

	// Decide where the secret lives.
	persisted := false
	c.FallbackAPIKey = ""
	if c.APIKey != "" {
		if err := secretstore.Set(c.APIKey); err == nil {
			persisted = true
		} else {
			// Keychain unavailable — fall back to writing it on disk.
			c.FallbackAPIKey = c.APIKey
			warnFileFallback()
		}
	} else {
		// Logged out — clear both backends.
		_ = secretstore.Delete()
	}

	// Always clear the legacy field — keychain (or fallback) is now the
	// source of truth.
	c.LegacyAPIKey = ""

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return err
	}
	_ = persisted // (kept for readability of the keychain-success branch)
	return nil
}

// Clear removes the config file AND clears the secretstore (logout).
func Clear() error {
	// Always clear the keychain — independent of the on-disk file state.
	_ = secretstore.Delete()

	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".instant-config"), nil
}
