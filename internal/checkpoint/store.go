// Package checkpoint persists pipeline progress so an interrupted run can be
// resumed without re-reading the archive and without changing output hashes.
package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Schema is bumped whenever the on-disk layout changes incompatibly.
const Schema = 1

// ErrNotFound reports a writer directory without a state file.
var ErrNotFound = errors.New("checkpoint: no state file")

// State is the durable resume cursor. InputOffset is the number of source
// records already folded into the timeline.
type State struct {
	Schema           int               `json:"schema"`
	InputOffset      int64             `json:"input_offset"`
	ConfigDigest     string            `json:"config_digest"`
	Committed        map[string]int    `json:"committed"`     // rep -> highest committed index
	CommittedSHA     map[string]string `json:"committed_sha"` // rep/index -> output hash
	ManifestChain    string            `json:"manifest_chain"`
	ManifestCount    int               `json:"manifest_count"`
	LedgerDigest     string            `json:"ledger_digest"`
	TimelineVersions map[string]int    `json:"timeline_versions"`
	MaxVersion       int               `json:"max_version"`
	ReplayedSegments int               `json:"replayed_segments"`
	SourceSHA256     string            `json:"source_sha256"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

// ConfigDigest computes the stable digest of everything that changes output:
// parameters, ladder and codec configuration. Runs with different digests are
// never resumed into each other.
func ConfigDigest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Store atomically replaces the state file. Concurrent writers are serialised
// and only the newest State is kept.
type Store struct {
	dir  string
	mu   sync.Mutex
	last State
}

// NewStore validates the directory and returns a store. The directory is
// created if missing.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("checkpoint: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("checkpoint: %s is not a directory", dir)
	}
	s := &Store{dir: dir}
	if st, err := s.Load(); err == nil {
		s.last = st
	}
	return s, nil
}

// Path returns the state file location.
func (s *Store) Path() string { return filepath.Join(s.dir, "state.json") }

// Load reads the last state. A missing file yields ErrNotFound.
func (s *Store) Load() (State, error) {
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, ErrNotFound
		}
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, fmt.Errorf("checkpoint: corrupt state: %w", err)
	}
	if st.Schema != Schema {
		return State{}, fmt.Errorf("checkpoint: schema %d is not supported (want %d)", st.Schema, Schema)
	}
	return st, nil
}

// Save writes st atomically (temp file then rename) unless ctx is cancelled.
func (s *Store) Save(ctx context.Context, st State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st.Schema = Schema
	st.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp, err := os.CreateTemp(s.dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, s.Path()); err != nil {
		cleanup()
		return err
	}
	s.last = st
	return nil
}

// Last returns the most recently saved state.
func (s *Store) Last() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}
