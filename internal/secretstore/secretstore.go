// Package secretstore provides a pluggable backend for storing the user's
// CLI bearer token. The default backend is the OS keychain (macOS Keychain,
// Linux Secret Service / libsecret, Windows Credential Manager); a plaintext
// file fallback is used when the keychain is unavailable (headless Linux,
// disabled service, sandboxed environment).
//
// Why two backends?
//
//   - The keychain is the right place for a long-lived production credential.
//     A user's API key has no expiry and full team scope today, so a copy of
//     it sitting in `~/.instant-config` mode 0600 is read by anything running
//     as that user (backup tools, IDE extensions, malware, …). T16 P1-1.
//
//   - We can't refuse to run in CI / SSH / Linux-no-DBus environments — the
//     CLI must still work. So when the keychain reports "unavailable" we
//     fall back to the existing file mechanism, with a clear stderr warning
//     the first time so the user knows their token is on disk.
//
// The interface deliberately stays tiny — Get / Set / Delete — so the test
// suite can swap in an in-memory implementation without touching the real
// keychain.
package secretstore

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/zalando/go-keyring"
)

// ServiceName is the keychain "service" name we register under. macOS shows
// this as the Keychain item's Name; Linux secret-service treats it as the
// schema's "service" attribute; Windows Credential Manager shows it as the
// target name's prefix.
const ServiceName = "instanode.dev"

// AccountName is the keychain "account" we register the bearer token under.
// One CLI install -> one keychain entry (no per-profile multi-tenancy today).
const AccountName = "api_key"

// ErrNotFound is returned when no secret has been stored yet. Callers should
// treat it as "anonymous mode" (no token), not as a fatal error.
var ErrNotFound = errors.New("secretstore: no secret found")

// Backend is the minimal interface every secret store must satisfy. The
// concrete types are kept inside this package so tests in the same package
// can swap them.
type Backend interface {
	// Get returns the stored secret, or ErrNotFound if none exists.
	Get() (string, error)
	// Set persists the secret. An empty value is equivalent to Delete.
	Set(value string) error
	// Delete removes any stored secret. It must be idempotent (no error
	// when the key was already absent).
	Delete() error
	// Name returns a short label ("keychain", "memory", "file-fallback").
	// Surfaced by `whoami` so the user knows where their key lives.
	Name() string
	// Available reports whether the backend is usable in the current
	// environment. A false here causes the chain to walk to the next backend.
	Available() bool
}

// active is the package-global Backend. Set on init via Use() (default is
// the keychain backend with file fallback). Tests can call UseMemoryBackend
// to install the in-memory variant.
var (
	mu     sync.RWMutex
	active Backend
)

// Get returns the stored secret using the active backend. ErrNotFound means
// no secret has been written (anonymous mode).
func Get() (string, error) {
	mu.RLock()
	b := active
	mu.RUnlock()
	if b == nil {
		return "", ErrNotFound
	}
	return b.Get()
}

// Set persists the secret using the active backend.
func Set(value string) error {
	mu.RLock()
	b := active
	mu.RUnlock()
	if b == nil {
		return errors.New("secretstore: no active backend")
	}
	return b.Set(value)
}

// Delete removes any stored secret. Idempotent.
func Delete() error {
	mu.RLock()
	b := active
	mu.RUnlock()
	if b == nil {
		return nil
	}
	return b.Delete()
}

// Name returns a label for the active backend ("keychain", "memory",
// "file-fallback"). Used by `whoami` so the user can see where their secret
// lives.
func Name() string {
	mu.RLock()
	b := active
	mu.RUnlock()
	if b == nil {
		return "none"
	}
	return b.Name()
}

// Use installs a specific backend. Primarily used by tests; production code
// calls UseDefault.
func Use(b Backend) {
	mu.Lock()
	active = b
	mu.Unlock()
}

// UseMemoryBackend installs an in-memory backend. Tests should call this in
// TestMain so the real OS keychain is never touched by the suite.
func UseMemoryBackend() *MemoryBackend {
	m := &MemoryBackend{}
	Use(m)
	return m
}

// UseDefault selects the keychain if available, otherwise leaves any
// already-installed backend in place — the caller (cliconfig) handles file
// fallback when this returns nil. We split the "keychain backend" from
// "file fallback" because the file is owned by the cliconfig package (it
// already does atomic writes and 0600 mode); we don't duplicate that here.
//
// IMPORTANT: UseDefault does NOT clobber a previously-installed backend
// (e.g. the in-memory backend that the test suite installs in TestMain).
// If you want to force a reset, call Use(nil) first.
func UseDefault() Backend {
	mu.RLock()
	current := active
	mu.RUnlock()
	if current != nil {
		// A backend is already wired (likely tests using UseMemoryBackend
		// or a previous UseDefault call). Don't stomp it.
		return current
	}
	b := &keychainBackend{}
	if b.Available() {
		Use(b)
		return b
	}
	return nil
}

// ── keychain backend ────────────────────────────────────────────────────────

// keyringProvider is the minimal subset of github.com/zalando/go-keyring we
// need. Extracting this as an interface lets the test suite exercise the
// keychainBackend wrapping logic (error mapping, idempotency, etc.) without
// hitting the real OS keychain — which is itself environment-dependent and
// can't be reliably probed in CI.
type keyringProvider interface {
	Get(service, account string) (string, error)
	Set(service, account, value string) error
	Delete(service, account string) error
}

// realKeyring is the production implementation backed by go-keyring.
type realKeyring struct{}

func (realKeyring) Get(s, a string) (string, error)    { return keyring.Get(s, a) }
func (realKeyring) Set(s, a, v string) error           { return keyring.Set(s, a, v) }
func (realKeyring) Delete(s, a string) error           { return keyring.Delete(s, a) }

// keyringErrNotFound is the sentinel returned by realKeyring on miss.
// Extracted so tests can wrap their fake's "not found" return through the
// same Is() check.
var keyringErrNotFound = keyring.ErrNotFound

// keychainBackend uses a keyringProvider, which in production routes to:
//   - macOS:   Keychain (security framework)
//   - Linux:   libsecret / Secret Service (org.freedesktop.secrets, DBus)
//   - Windows: Credential Manager (wincred)
type keychainBackend struct {
	// provider may be nil; nil => use the real OS keychain.
	provider keyringProvider
}

func (k *keychainBackend) ring() keyringProvider {
	if k.provider != nil {
		return k.provider
	}
	return realKeyring{}
}

func (k *keychainBackend) Get() (string, error) {
	v, err := k.ring().Get(ServiceName, AccountName)
	if errors.Is(err, keyringErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("keychain get: %w", err)
	}
	return v, nil
}

func (k *keychainBackend) Set(value string) error {
	if value == "" {
		return k.Delete()
	}
	if err := k.ring().Set(ServiceName, AccountName, value); err != nil {
		return fmt.Errorf("keychain set: %w", err)
	}
	return nil
}

func (k *keychainBackend) Delete() error {
	err := k.ring().Delete(ServiceName, AccountName)
	if err == nil {
		return nil
	}
	if errors.Is(err, keyringErrNotFound) {
		return nil // idempotent
	}
	return fmt.Errorf("keychain delete: %w", err)
}

func (k *keychainBackend) Name() string { return "keychain" }

// Available probes the keychain with a no-op Get. Any non-ErrNotFound error
// means the backend can't be used (no DBus / locked / etc.). We treat
// ErrNotFound as "available, just empty".
//
// The probe is cheap on macOS / Windows and uses ~5-15ms on Linux Secret
// Service (one DBus round trip). It runs once at init time.
func (k *keychainBackend) Available() bool {
	// Environment override — useful for CI (CI=true) and for the unit-test
	// suite in this repo (INSTANT_DISABLE_KEYCHAIN=1) so we never poke the
	// real OS keychain during `go test`.
	if os.Getenv("INSTANT_DISABLE_KEYCHAIN") == "1" {
		return false
	}
	_, err := k.ring().Get(ServiceName, AccountName)
	if err == nil {
		return true
	}
	if errors.Is(err, keyringErrNotFound) {
		return true
	}
	return false
}

// ── in-memory backend (tests) ───────────────────────────────────────────────

// MemoryBackend keeps the secret in a process-local string. Use only in tests.
type MemoryBackend struct {
	mu  sync.Mutex
	val string
	has bool
}

func (m *MemoryBackend) Get() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.has {
		return "", ErrNotFound
	}
	return m.val, nil
}

func (m *MemoryBackend) Set(value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if value == "" {
		m.val, m.has = "", false
		return nil
	}
	m.val, m.has = value, true
	return nil
}

func (m *MemoryBackend) Delete() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.val, m.has = "", false
	return nil
}

func (m *MemoryBackend) Name() string   { return "memory" }
func (m *MemoryBackend) Available() bool { return true }

// ── helpers ─────────────────────────────────────────────────────────────────

// TruncateForDisplay returns a safe-to-print prefix of a credential — at
// most 8 characters followed by an ellipsis. Never use this for anything
// security-sensitive; it's purely for "is the right account logged in?" UX.
//
// Callers MUST use this for any UI that surfaces the key (whoami, list, ...).
// The previous CLI printed the first 16 characters of the key directly; the
// audit (T16 P1-1) flagged that as material disclosure.
func TruncateForDisplay(s string) string {
	const max = 8
	if len(s) <= max {
		if s == "" {
			return ""
		}
		return s + "…"
	}
	return s[:max] + "…"
}
