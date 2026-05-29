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

// TestSave_RenameFailureReturnsError exercises the rename-failure branch.
// Pointing the store at a path whose parent directory is a regular file
// makes os.WriteFile of the .tmp succeed (we're writing inside the parent
// of `path`, which is the temp dir) BUT — wait, no: WriteFile would fail
// upstream. To hit the rename-failure branch specifically we craft a path
// where the .tmp can be written but the rename target is a directory: on
// POSIX, os.Rename(file, existingDir) returns ENOTEMPTY / EISDIR, which is
// exactly the failure mode our cleanup branch handles.
func TestSave_RenameFailureReturnsError(t *testing.T) {
	dir := setupTempHome(t)
	storePath := filepath.Join(dir, ".instant-tokens")
	// Make storePath a non-empty directory so os.Rename(tmp, storePath)
	// returns an error and we exercise the cleanup branch.
	if err := os.Mkdir(storePath, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storePath, "block"), []byte("x"), 0600); err != nil {
		t.Fatalf("seed inside dir: %v", err)
	}

	s := &Store{path: storePath}
	err := s.Add(Entry{Token: "tok-rename-fail", Name: "x", Type: "postgres", URL: "postgres://x"})
	if err == nil {
		t.Fatalf("expected Save to return error when target path is a non-empty dir")
	}
	// The .tmp sibling MUST be cleaned up by the failure-cleanup branch.
	if _, statErr := os.Stat(storePath + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf(".tmp file should have been removed after rename failure, got err=%v", statErr)
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
