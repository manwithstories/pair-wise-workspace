// Package checkpoint persists the processing boundary that has actually been
// merged and durably committed. Writes go to a temporary file, are synced, and
// then atomically replace the previous state. A corrupted or unreadable
// checkpoint safely rolls back to the last intact boundary; already-committed
// samples are never counted twice because only committed boundaries are
// recorded.
package checkpoint

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"mediacore/internal/fault"
)

// State is the persisted resume boundary.
type State struct {
	// Version allows future format migration.
	Version int `json:"version"`
	// CommittedInputSeq is the last input sequence merged and committed.
	CommittedInputSeq int `json:"committed_input_seq"`
	// CommittedSamples is the cumulative count of samples durably written.
	CommittedSamples int `json:"committed_samples"`
	// PerTrack tracks per-track committed sample counts keyed by mapped number.
	PerTrack map[uint32]int `json:"per_track"`
	// ResumeTick is the unified 90 kHz PTS to resume from.
	ResumeTick int64 `json:"resume_tick"`
	// InputDigest pins the checkpoint to one concrete input.
	InputDigest string `json:"input_digest"`
}

// Store owns one checkpoint file plus its temporary sibling.
type Store struct {
	path string
}

// NewStore creates a store for the given checkpoint path, rejecting traversal.
func NewStore(path string) (*Store, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	return &Store{path: path}, nil
}

func validatePath(path string) error {
	clean := filepath.Clean(path)
	if clean == "." || clean == string(filepath.Separator) ||
		filepath.IsAbs(clean) && filepath.Dir(clean) == string(filepath.Separator) {
		return fault.New(fault.KindCheckpoint, fault.Abort, -1,
			"checkpoint path must name a file, not a root")
	}
	base := filepath.Base(clean)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return fault.New(fault.KindCheckpoint, fault.Abort, -1,
			"invalid checkpoint file name")
	}
	return nil
}

// Save atomically persists state: write temp, fsync, rename over the target.
func (s *Store) Save(st State) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fault.New(fault.KindCheckpoint, fault.Abort, -1,
			"cannot create checkpoint dir: "+err.Error())
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fault.New(fault.KindCheckpoint, fault.Abort, -1, err.Error())
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fault.New(fault.KindCheckpoint, fault.Abort, -1, err.Error())
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fault.New(fault.KindCheckpoint, fault.Abort, -1, err.Error())
	}
	if err := f.Sync(); err != nil { // durable before rename
		_ = f.Close()
		_ = os.Remove(tmp)
		return fault.New(fault.KindCheckpoint, fault.Abort, -1, err.Error())
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fault.New(fault.KindCheckpoint, fault.Abort, -1, err.Error())
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fault.New(fault.KindCheckpoint, fault.Abort, -1, err.Error())
	}
	syncDir(dir)
	return nil
}

// Load reads the checkpoint. A missing file returns a zero state and no error
// (first run). A corrupted file returns a rollback fault so the caller can
// fall back to the last intact boundary rather than crash.
func (s *Store) Load() (State, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{Version: 1, PerTrack: map[uint32]int{}}, nil
		}
		return State{}, fault.New(fault.KindCheckpoint, fault.Rollback, -1, err.Error())
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fault.New(fault.KindCheckpoint, fault.Rollback, -1,
			"checkpoint corrupted: "+err.Error())
	}
	if st.PerTrack == nil {
		st.PerTrack = map[uint32]int{}
	}
	return st, nil
}

// LoadOrRollback loads a valid checkpoint, returning the provided fallback
// state when the file is missing or corrupted. Corruption is reported via
// onFault but never aborts the resumed run.
func (s *Store) LoadOrRollback(fallback State, onFault func(*fault.Fault)) State {
	st, err := s.Load()
	if err != nil {
		if f := fault.As(err); f != nil && onFault != nil {
			onFault(f)
		}
		return fallback
	}
	return st
}

// Remove deletes the checkpoint (used after a clean full completion).
func (s *Store) Remove() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fault.New(fault.KindCheckpoint, fault.Abort, -1, err.Error())
	}
	return nil
}

// Path exposes the checkpoint location.
func (s *Store) Path() string { return s.path }
