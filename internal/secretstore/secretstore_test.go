package secretstore

import (
	"errors"
	"strings"
	"testing"
)

// TestMemoryBackend_RoundTrip is the basic happy-path: Set → Get → Delete.
func TestMemoryBackend_RoundTrip(t *testing.T) {
	m := UseMemoryBackend()
	defer Use(nil)

	// Empty store: Get returns ErrNotFound.
	if _, err := Get(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store: expected ErrNotFound, got %v", err)
	}

	// Set then Get must round-trip.
	const secret = "inst_live_abcdef0123456789"
	if err := Set(secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := Get()
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if got != secret {
		t.Errorf("Get: want %q, got %q", secret, got)
	}

	// Delete clears the value.
	if err := Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := Get(); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete: expected ErrNotFound, got %v", err)
	}

	// Sanity: backend wired correctly.
	if Name() != "memory" {
		t.Errorf("Name() = %q, want %q", Name(), "memory")
	}
	if !m.Available() {
		t.Error("MemoryBackend must always be Available()")
	}
}

// TestMemoryBackend_SetEmptyDeletes confirms Set("") == Delete().
func TestMemoryBackend_SetEmptyDeletes(t *testing.T) {
	UseMemoryBackend()
	defer Use(nil)

	_ = Set("something")
	if err := Set(""); err != nil {
		t.Fatalf("Set(\"\"): %v", err)
	}
	if _, err := Get(); !errors.Is(err, ErrNotFound) {
		t.Errorf("Set(\"\") must clear the value, got err=%v", err)
	}
}

// TestNoActiveBackend asserts the package degrades gracefully (no panic)
// when Use(nil) has been called.
func TestNoActiveBackend(t *testing.T) {
	Use(nil)
	if _, err := Get(); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get with no backend: expected ErrNotFound, got %v", err)
	}
	if err := Set("x"); err == nil {
		t.Error("Set with no backend must error")
	}
	if err := Delete(); err != nil {
		t.Errorf("Delete with no backend must be a no-op, got %v", err)
	}
	if Name() != "none" {
		t.Errorf("Name() = %q, want %q", Name(), "none")
	}
}

// ── TruncateForDisplay (T16 P1-1 fix) ──────────────────────────────────────
// The previous whoami printed the first 16 characters of a long-lived
// production credential. The audit flagged that as material disclosure.
// TruncateForDisplay caps any displayed key at 8 chars + ellipsis.

func TestTruncateForDisplay(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"short", "short…"},
		{"abcdefgh", "abcdefgh…"},                       // exactly 8 → 8 + ellipsis
		{"abcdefghi", "abcdefgh…"},                      // 9 → cut to 8
		{"inst_live_abcdef0123456789", "inst_liv…"},     // realistic long key
	}
	for _, tc := range cases {
		got := TruncateForDisplay(tc.in)
		if got != tc.want {
			t.Errorf("TruncateForDisplay(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTruncateForDisplay_NeverLeaksFullKey is the regression assertion:
// no matter the input length, the output must NOT contain more than 8
// characters of the original string. This is the canonical "does the API
// key end up on the user's terminal" test.
func TestTruncateForDisplay_NeverLeaksFullKey(t *testing.T) {
	// A 64-char production-style key. The whoami output before this fix
	// dumped its first 16 chars; we cap at 8.
	key := "inst_live_" + strings.Repeat("x", 54)
	out := TruncateForDisplay(key)

	// The output must not contain more than the first 8 chars of the key.
	for cut := 9; cut <= len(key); cut++ {
		if strings.Contains(out, key[:cut]) {
			t.Errorf("TruncateForDisplay leaks %d chars of the key: %q", cut, out)
			break
		}
	}
	// And the first 8 must be preserved (the user needs *some* signal).
	if !strings.HasPrefix(out, key[:8]) {
		t.Errorf("TruncateForDisplay dropped the leading 8 chars: %q", out)
	}
}
