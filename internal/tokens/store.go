// Package tokens manages the local ~/.instant-tokens file that persists
// provisioned resource tokens between CLI invocations.
package tokens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Entry represents one saved resource token and metadata.
//
// B15-P1 (7) — Type and Env fields enable anonymous-tier `instant up`
// idempotency. The anonymous caller can't list resources from the API
// (no auth), so `up` falls back to this local cache to recognize a
// previously-provisioned (type, name, env) tuple and skip re-provisioning.
// Older config files that lack these fields deserialize as empty strings;
// the up code-path skips them via empty-string checks rather than
// re-provisioning blind. Tokens written by `instant <type> new` since this
// release populate Type; Env will only be populated when written by `up`.
type Entry struct {
	Token     string    `json:"token"`
	Name      string    `json:"name"`
	Type      string    `json:"type,omitempty"`     // resource_type: postgres|redis|mongodb|queue|storage|webhook|vector
	Env       string    `json:"env,omitempty"`      // environment: development|production|staging|...
	URL       string    `json:"url"`                // connection URL or other primary URL
	Schedule  string    `json:"schedule,omitempty"` // B15-P2-12 — omitempty so phantom `"schedule":""` rows don't bleed into `status --json`
	Source    string    `json:"source"`             // e.g. "provision"
	CreatedAt time.Time `json:"created_at"`
}

// FindByTypeNameEnv returns the entry matching (type, name, env) for the
// local idempotency cache used by anonymous `up`. Match is case-insensitive
// on type+name. An entry with an empty Type field is never returned (legacy
// rows from before B15-P1 — we can't safely claim them belong to a given
// type without a round trip). Returns nil if no entry matches.
func (s *Store) FindByTypeNameEnv(typ, name, env string) *Entry {
	wantType := normalize(typ)
	wantName := normalize(name)
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.Type == "" {
			continue
		}
		if normalize(e.Type) != wantType {
			continue
		}
		if normalize(e.Name) != wantName {
			continue
		}
		// env match: an empty cached env counts as the default development
		// env, matching the platform's resolved-env default (CLAUDE.md
		// rule 11). Otherwise, exact-match.
		if e.Env == "" && (env == "" || env == "development") {
			return e
		}
		if e.Env == env {
			return e
		}
	}
	return nil
}

// normalize is the canonical-form helper used by Entry-key lookups.
// Lowercase + trimmed whitespace.
func normalize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// Store is the in-memory representation of ~/.instant-tokens.
type Store struct {
	Entries []Entry `json:"entries"`
	path    string
}

// Load reads the store from disk, creating it if absent.
func Load() (*Store, error) {
	path, err := storePath()
	if err != nil {
		return nil, err
	}
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	s.path = path
	return s, nil
}

// Save writes the store back to disk.
func (s *Store) Save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

// Add appends a new entry and saves.
func (s *Store) Add(e Entry) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	s.Entries = append(s.Entries, e)
	return s.Save()
}

// Find returns the entry for a given token, or nil if not found.
func (s *Store) Find(token string) *Entry {
	for i := range s.Entries {
		if s.Entries[i].Token == token {
			return &s.Entries[i]
		}
	}
	return nil
}

// Remove drops the entry matching token and saves. Returns true when an
// entry was actually removed; false when the token was unknown. Callers
// MUST NOT depend on Remove for security — the source of truth is the API.
// Used by `instant resource delete` to keep `instant status` honest.
func (s *Store) Remove(token string) bool {
	out := s.Entries[:0]
	removed := false
	for _, e := range s.Entries {
		if e.Token == token {
			removed = true
			continue
		}
		out = append(out, e)
	}
	if !removed {
		return false
	}
	s.Entries = out
	_ = s.Save()
	return true
}

func storePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".instant-tokens"), nil
}
