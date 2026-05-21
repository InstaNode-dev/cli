package secretstore

import (
	"testing"
)

// TestUseDefault_LeavesExistingBackend confirms UseDefault is non-clobbering.
func TestUseDefault_LeavesExistingBackend(t *testing.T) {
	m := UseMemoryBackend()
	defer Use(nil)

	if got := UseDefault(); got != m {
		t.Errorf("UseDefault should preserve in-memory backend, got %T", got)
	}
}

// TestUseDefault_NoBackend_DisabledKeychain — when the env var disables the
// keychain and no backend is active, UseDefault returns nil.
func TestUseDefault_DisabledKeychain(t *testing.T) {
	Use(nil)
	t.Setenv("INSTANT_DISABLE_KEYCHAIN", "1")
	got := UseDefault()
	if got != nil {
		t.Errorf("UseDefault with disabled keychain should return nil, got %T", got)
	}
}

// TestKeychainBackend_Available_DisabledByEnv verifies the env override.
func TestKeychainBackend_Available_DisabledByEnv(t *testing.T) {
	t.Setenv("INSTANT_DISABLE_KEYCHAIN", "1")
	k := &keychainBackend{}
	if k.Available() {
		t.Error("keychain should report unavailable when INSTANT_DISABLE_KEYCHAIN=1")
	}
	if k.Name() != "keychain" {
		t.Errorf("Name() = %q", k.Name())
	}
}

// TestKeychainBackend_SetEmptyDeletes covers the "Set("") -> Delete" branch
// even when the keychain itself is unavailable (calling Delete is idempotent
// and safe to invoke against a missing/unavailable backend).
func TestKeychainBackend_SetEmptyDeletes(t *testing.T) {
	t.Setenv("INSTANT_DISABLE_KEYCHAIN", "1")
	k := &keychainBackend{}
	// Set("") routes to Delete(); Delete must be idempotent, so this should
	// either return nil or a non-fatal error from go-keyring on hosts without
	// a keychain. Just ensure it doesn't panic.
	_ = k.Set("")
}

// TestMemoryBackend_Direct exercises *MemoryBackend methods through the type.
func TestMemoryBackend_Direct(t *testing.T) {
	m := &MemoryBackend{}
	if _, err := m.Get(); err != ErrNotFound {
		t.Errorf("empty Get should return ErrNotFound, got %v", err)
	}
	if err := m.Set("x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, _ := m.Get(); v != "x" {
		t.Errorf("Get = %q", v)
	}
	if err := m.Set(""); err != nil {
		t.Fatalf("Set empty: %v", err)
	}
	if _, err := m.Get(); err != ErrNotFound {
		t.Error("Set(\"\") should clear")
	}
	if !m.Available() {
		t.Error("MemoryBackend always available")
	}
	if m.Name() != "memory" {
		t.Errorf("Name = %q", m.Name())
	}
	_ = m.Delete()
}
