// Package checkpoint persists pipeline progress to a local JSON file so an
// interrupted run can resume. The file carries a CRC-32 checksum; a
// corrupted checkpoint is reported as a typed fault, never silently used.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"

	"mediad/internal/fault"
)

// State is the recoverable progress of a pipeline run.
type State struct {
	Completed     map[string]bool `json:"completed"` // DAG idempotency keys
	SegmentHashes []string        `json:"segment_hashes,omitempty"`
}

// Store reads and writes one checkpoint file inside the workspace.
type Store struct {
	Path string
}

type envelope struct {
	State State  `json:"state"`
	CRC   uint32 `json:"crc"`
}

// NewStore builds a Store at path, refusing paths outside dir.
func NewStore(dir, name string) (*Store, error) {
	clean := filepath.Clean(filepath.Join(dir, name))
	rel, err := filepath.Rel(dir, clean)
	if err != nil || rel == ".." || len(rel) > 2 && rel[:3] == "../" {
		return nil, fault.New(fault.KindCheckpoint, "checkpoint",
			"path %q escapes workspace %q", name, dir)
	}
	return &Store{Path: clean}, nil
}

// Save writes the state atomically (temp file + rename) with a checksum.
func (s *Store) Save(st State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	env, err := json.Marshal(envelope{State: st, CRC: crc32.ChecksumIEEE(raw)})
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, env, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// Load reads the state, verifying the checksum. A missing file yields an
// empty state; a corrupted file yields a KindCheckpoint fault.
func (s *Store) Load() (State, error) {
	data, err := os.ReadFile(s.Path)
	if os.IsNotExist(err) {
		return State{Completed: map[string]bool{}}, nil
	}
	if err != nil {
		return State{}, err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return State{}, fault.New(fault.KindCheckpoint, "checkpoint",
			"undecodable checkpoint %s: %v", s.Path, err)
	}
	raw, err := json.Marshal(env.State)
	if err != nil {
		return State{}, err
	}
	if crc32.ChecksumIEEE(raw) != env.CRC {
		return State{}, fault.New(fault.KindCheckpoint, "checkpoint",
			"checksum mismatch in %s: state corrupted", s.Path)
	}
	if env.State.Completed == nil {
		env.State.Completed = map[string]bool{}
	}
	return env.State, nil
}

// Corrupt overwrites the store with garbage; used by fault-suite fixtures.
func (s *Store) Corrupt() error {
	return os.WriteFile(s.Path, []byte(fmt.Sprintf(`{"state":{},"crc":%d}`, 1)), 0o644)
}
