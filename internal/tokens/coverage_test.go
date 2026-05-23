package tokens

// coverage_test.go — closes the Load-with-existing-data and Load-parse-error
// branches the existing suite leaves uncovered. CI-safe: all file I/O is
// redirected to a temp HOME.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ParsesExistingFile(t *testing.T) {
	dir := setupTempHome(t)
	path := filepath.Join(dir, ".instant-tokens")
	if err := os.WriteFile(path, []byte(`{"entries":[{"token":"t1","name":"db","type":"postgres","url":"postgres://x"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Entries) != 1 || s.Entries[0].Token != "t1" {
		t.Fatalf("Load entries = %+v", s.Entries)
	}
	// The loaded store can round-trip a Save back to the same path.
	if err := s.Add(Entry{Token: "t2", Name: "c", Type: "redis", URL: "redis://y"}); err != nil {
		t.Fatalf("Add after Load: %v", err)
	}
	reload, err := Load()
	if err != nil || len(reload.Entries) != 2 {
		t.Fatalf("reload = %+v / %v", reload, err)
	}
}

func TestLoad_ParseErrorOnCorruptFile(t *testing.T) {
	dir := setupTempHome(t)
	path := filepath.Join(dir, ".instant-tokens")
	if err := os.WriteFile(path, []byte("{not valid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("expected parse error on corrupt tokens file")
	}
}
