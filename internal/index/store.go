package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"aircheck/internal/fault"
)

// StoreVersion is the on-disk index schema version. A mismatch is a
// cache miss (rebuild), never a silent reuse.
const StoreVersion = 1

// Envelope wraps any persisted payload with schema version and key.
type Envelope struct {
	Version int             `json:"version"`
	Key     string          `json:"key"` // Digest of inputs+config+parser
	Payload json.RawMessage `json:"payload"`
}

// Store persists envelopes under a cache directory.
type Store struct{ Dir string }

func NewStore(dir string) *Store { return &Store{Dir: dir} }

func (s *Store) path(name string) string { return filepath.Join(s.Dir, name+".json") }

// Save writes payload atomically (temp file + rename).
func (s *Store) Save(name, key string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env, err := json.MarshalIndent(Envelope{Version: StoreVersion, Key: key, Payload: raw}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	tmp := s.path(name) + ".tmp"
	if err := os.WriteFile(tmp, env, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(name))
}

// Load reads and validates an envelope. A corrupt file is a fault with
// CodeIndexCorrupt; a version or key mismatch is CodeIndexStale so the
// caller rebuilds instead of reusing stale data.
func (s *Store) Load(name, key string, out any) error {
	raw, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return fault.New(fault.CodeIndexStale, "", -1, true, "no cached index %q", name)
	}
	if err != nil {
		return err
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fault.New(fault.CodeIndexCorrupt, "", -1, true,
			"index %q is not valid JSON: %v", name, err)
	}
	if env.Version != StoreVersion {
		return fault.New(fault.CodeIndexStale, "", -1, true,
			"index %q version %d, want %d", name, env.Version, StoreVersion)
	}
	if env.Key != key {
		return fault.New(fault.CodeIndexStale, "", -1, true, "index %q key mismatch", name)
	}
	if err := json.Unmarshal(env.Payload, out); err != nil {
		return fault.New(fault.CodeIndexCorrupt, "", -1, true,
			"index %q payload undecodable: %v", name, err)
	}
	return nil
}

// KeyFor builds the cache key for an input root and config string.
func KeyFor(root, config string) (string, error) {
	files, err := HashTree(root)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("input directory %s is empty", root)
	}
	return Digest(DigestInput{Files: files, Config: config, ParserVersion: ParserVersion}), nil
}
