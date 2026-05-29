package tokens

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSave_AtomicNoTempLeak ensures Save() leaves no .tmp file on disk on a
// successful write. SEC-CLI FINDING-18.
func TestSave_AtomicNoTempLeak(t *testing.T) {
	dir := setupTempHome(t)
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Add(Entry{Token: "tok-atomic", Name: "x", Type: "postgres", URL: "postgres://x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	tmpPath := filepath.Join(dir, ".instant-tokens.tmp")
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf(".tmp file should not linger after successful Save, got err=%v", err)
	}
	// And the real file should exist + be readable.
	if _, err := os.Stat(filepath.Join(dir, ".instant-tokens")); err != nil {
		t.Errorf("expected .instant-tokens to exist, got: %v", err)
	}
}

// TestSave_RenameOverwritesExistingFile verifies the rename idiom replaces
// an existing file (not appended). Cross-platform — works on Linux + macOS.
func TestSave_RenameOverwritesExistingFile(t *testing.T) {
	dir := setupTempHome(t)
	path := filepath.Join(dir, ".instant-tokens")

	// Pre-seed the path with garbage so Save() must clobber it.
	if err := os.WriteFile(path, []byte("GARBAGE_PRESEED"), 0600); err != nil {
		t.Fatalf("pre-seed: %v", err)
	}

	s, err := Load()
	if err == nil {
		t.Fatalf("Load should have failed on garbage seed, got s=%+v", s)
	}
	// Build a fresh store and Save — overwrites the bad bytes.
	fresh := &Store{path: path}
	if err := fresh.Add(Entry{Token: "real", Name: "n", Type: "postgres", URL: "postgres://x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) == "GARBAGE_PRESEED" {
		t.Errorf("Save did not overwrite seeded garbage")
	}
}
