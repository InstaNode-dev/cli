package tokens

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setupTempHome redirects HOME to a temp dir for the test, ensuring
// storePath() lands in an isolated location.
func setupTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestLoad_AbsentFile(t *testing.T) {
	setupTempHome(t)
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Entries) != 0 {
		t.Errorf("expected empty entries, got %d", len(s.Entries))
	}
}

func TestAdd_Find_Remove_Save(t *testing.T) {
	dir := setupTempHome(t)
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.Add(Entry{Token: "tok1", Name: "db1", Type: "postgres", URL: "postgres://x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(Entry{Token: "tok2", Name: "c1", Type: "redis", URL: "redis://x", CreatedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Find roundtrips
	if e := s.Find("tok1"); e == nil || e.Name != "db1" {
		t.Errorf("Find(tok1) = %+v", e)
	}
	if e := s.Find("missing"); e != nil {
		t.Errorf("Find(missing) = %+v, want nil", e)
	}

	// File written
	if _, err := os.Stat(filepath.Join(dir, ".instant-tokens")); err != nil {
		t.Errorf("expected file written, got %v", err)
	}

	// Reload preserves data
	s2, err := Load()
	if err != nil {
		t.Fatalf("Load2: %v", err)
	}
	if len(s2.Entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(s2.Entries))
	}

	// Remove existing & missing
	if !s2.Remove("tok1") {
		t.Error("Remove(existing) should return true")
	}
	if s2.Remove("nope") {
		t.Error("Remove(missing) should return false")
	}
	if s2.Find("tok1") != nil {
		t.Error("tok1 should be gone")
	}
}

func TestFindByTypeNameEnv(t *testing.T) {
	setupTempHome(t)
	s, _ := Load()
	_ = s.Add(Entry{Token: "tok1", Name: "DB One", Type: "Postgres", Env: "production"})
	_ = s.Add(Entry{Token: "tok2", Name: "cache", Type: "redis", Env: ""}) // legacy empty env
	_ = s.Add(Entry{Token: "tok3", Name: "x", Type: ""})                   // legacy empty type — ignored

	// Case-insensitive type+name match.
	if e := s.FindByTypeNameEnv("postgres", "db one", "production"); e == nil || e.Token != "tok1" {
		t.Errorf("expected tok1, got %+v", e)
	}
	// Empty cached env matches default "development".
	if e := s.FindByTypeNameEnv("redis", "cache", "development"); e == nil || e.Token != "tok2" {
		t.Errorf("expected tok2, got %+v", e)
	}
	// Empty cached env also matches empty.
	if e := s.FindByTypeNameEnv("redis", "cache", ""); e == nil {
		t.Errorf("empty env should match")
	}
	// Different env → no match.
	if e := s.FindByTypeNameEnv("postgres", "db one", "staging"); e != nil {
		t.Errorf("staging should not match production entry, got %+v", e)
	}
	// Empty type entry is never returned.
	if e := s.FindByTypeNameEnv("", "x", ""); e != nil {
		t.Errorf("empty-type entry returned: %+v", e)
	}
	// Unknown combo.
	if e := s.FindByTypeNameEnv("kafka", "nope", "dev"); e != nil {
		t.Errorf("expected nil for unknown, got %+v", e)
	}
}

func TestNormalize_Internal(t *testing.T) {
	cases := map[string]string{
		"":         "",
		"  Foo  ":  "foo",
		"a\tB\nC":  "abc",
		"POSTGRES": "postgres",
	}
	for in, want := range cases {
		got := normalize(in)
		if got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAdd_AutoTimestamps(t *testing.T) {
	setupTempHome(t)
	s, _ := Load()
	before := time.Now().UTC()
	if err := s.Add(Entry{Token: "auto1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	e := s.Find("auto1")
	if e == nil {
		t.Fatal("Find returned nil")
	}
	if e.CreatedAt.IsZero() {
		t.Error("CreatedAt should be auto-populated")
	}
	if e.CreatedAt.Before(before.Add(-time.Second)) {
		t.Errorf("CreatedAt %v predates test start %v", e.CreatedAt, before)
	}
}

func TestStorePath_Internal(t *testing.T) {
	dir := setupTempHome(t)
	p, err := storePath()
	if err != nil {
		t.Fatalf("storePath: %v", err)
	}
	if p != filepath.Join(dir, ".instant-tokens") {
		t.Errorf("storePath = %q, want %q", p, filepath.Join(dir, ".instant-tokens"))
	}
}

func TestSave_BadPath(t *testing.T) {
	s := &Store{path: "/nonexistent-directory/no/such/file"}
	if err := s.Save(); err == nil {
		t.Error("expected error writing to bogus path")
	}
}
