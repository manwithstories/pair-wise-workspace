// Package checkpoint persists resumable delivery state. A checkpoint is only
// committed after a whole segment has landed on disk; on rerun, the stored
// input digest must match, otherwise resume is refused. Parse failures never
// leave a resumable partial state.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"mediadeliver/internal/model"
)

// State is the on-disk resumable state for one delivery run.
type State struct {
	// Version allows future format evolution.
	Version int `json:"version"`
	// Input identifies the manifest+source digest a run was built from.
	Input model.InputDigest `json:"input"`
	// Completed segments, indexed by segment name, in completion order.
	Completed map[string]*model.SegmentRecord `json:"completed"`
	// Order preserves deterministic segment sequence across reruns.
	Order []string `json:"order"`
}

// Store serializes State to a JSON file atomically.
type Store struct {
	mu     sync.Mutex
	path   string
	state  *State
	loaded bool
}

// NewStore points at a checkpoint file (it need not exist yet).
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Load reads the checkpoint if present. A missing file yields an empty state,
// not an error, so a first run starts clean.
func (s *Store) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		s.state = s.fresh()
		s.loaded = true
		return s.state, nil
	}
	if err != nil {
		return nil, model.Wrap(model.KindIO, "checkpoint", "load", -1,
			"cannot read checkpoint", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, model.Wrap(model.KindCorruptMedia, "checkpoint", "load", -1,
			"checkpoint is not valid JSON; refusing to resume", err)
	}
	if st.Completed == nil {
		st.Completed = map[string]*model.SegmentRecord{}
	}
	s.state = &st
	s.loaded = true
	return s.state, nil
}

func (s *Store) fresh() *State {
	return &State{Version: 1, Completed: map[string]*model.SegmentRecord{}}
}

// EnsureInput binds the run's input digest. On first run it records it; on
// resume it requires an exact match and returns KindConflict on mismatch.
func (s *Store) EnsureInput(digest model.InputDigest) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return nil, model.New(model.KindIO, "checkpoint", "ensure input",
			"Load must be called before EnsureInput")
	}
	if s.state.Input.ManifestSHA256 == "" && len(s.state.Input.SourceSHA256) == 0 {
		s.state.Input = digest
		return s.state, s.persistLocked()
	}
	if !digestsEqual(s.state.Input, digest) {
		return nil, model.At(model.KindConflict, "checkpoint", "ensure input", -1,
			"input digest changed since the checkpoint; refusing to resume")
	}
	return s.state, nil
}

// IsComplete reports whether a segment was already durably committed.
func (s *Store) IsComplete(segment string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.state.Completed[segment]
	return ok
}

// Commit records a fully-written segment and atomically persists the state.
// It must be called only after the segment's output has been flushed/closed;
// committing a partial segment is a caller error.
func (s *Store) Commit(rec *model.SegmentRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return model.New(model.KindIO, "checkpoint", "commit",
			"Load must be called before Commit")
	}
	if _, exists := s.state.Completed[rec.OutputFile]; exists {
		return model.At(model.KindConflict, "checkpoint", "commit", -1,
			"segment already committed: "+rec.OutputFile)
	}
	cp := *rec
	s.state.Completed[rec.OutputFile] = &cp
	s.state.Order = append(s.state.Order, rec.OutputFile)
	return s.persistLocked()
}

// Snapshot returns a deep-enough copy for reporting.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := State{
		Version:   s.state.Version,
		Input:     s.state.Input,
		Completed: map[string]*model.SegmentRecord{},
		Order:     append([]string(nil), s.state.Order...),
	}
	for k, v := range s.state.Completed {
		cp := *v
		out.Completed[k] = &cp
	}
	return out
}

// persistLocked writes the file atomically: temp file + rename, so a crash
// never leaves a half-written (resumable) checkpoint behind.
func (s *Store) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return model.Wrap(model.KindIO, "checkpoint", "persist", -1,
			"cannot create checkpoint dir", err)
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return model.Wrap(model.KindInvalidInput, "checkpoint", "persist", -1,
			"cannot encode state", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return model.Wrap(model.KindIO, "checkpoint", "persist", -1,
			"cannot write temp checkpoint", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return model.Wrap(model.KindIO, "checkpoint", "persist", -1,
			"cannot atomically replace checkpoint", err)
	}
	return nil
}

// Discard removes the checkpoint so a failed run cannot be resumed.
func (s *Store) Discard() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return model.Wrap(model.KindIO, "checkpoint", "discard", -1,
			"cannot remove checkpoint", err)
	}
	s.state = s.fresh()
	return nil
}

func digestsEqual(a, b model.InputDigest) bool {
	if a.ManifestSHA256 != b.ManifestSHA256 {
		return false
	}
	if len(a.SourceSHA256) != len(b.SourceSHA256) {
		return false
	}
	for k, v := range a.SourceSHA256 {
		if b.SourceSHA256[k] != v {
			return false
		}
	}
	return true
}

// String gives a compact human-readable summary for the demo output.
func (s *State) String() string {
	return fmt.Sprintf("checkpoint{segments=%d, order=%v}", len(s.Completed), s.Order)
}
