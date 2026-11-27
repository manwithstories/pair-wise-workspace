// Package checkpoint persists resumable pipeline state atomically.
//
// A checkpoint is only written after the repaired byte prefix it describes has
// been validated, and it records that prefix's SHA-256. Saving is atomic
// (temp file, fsync, rename) and monotonic: an already-committed position and
// its digest are never overwritten by older or conflicting state.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"relayaudit/internal/fault"
)

// Version is the checkpoint format version.
const Version = 1

// State is the committed resume position.
type State struct {
	Version int `json:"version"`

	// SourceSHA256 pins the checkpoint to one input.
	SourceSHA256 string `json:"source_sha256"`
	// NextIndex is the number of input records already consumed.
	NextIndex int `json:"next_index"`
	// Kept and Dropped count output and discarded records so far.
	Kept    int `json:"kept"`
	Dropped int `json:"dropped"`
	// EmittedBytes is the validated length of the staged output prefix.
	EmittedBytes int64 `json:"emitted_bytes"`
	// PrefixSHA256 digests exactly EmittedBytes of staged output.
	PrefixSHA256 string `json:"prefix_sha256"`
	// Done marks a fully finalised output.
	Done bool `json:"done"`
	// OutputSHA256 is set only when Done is true.
	OutputSHA256 string `json:"output_sha256"`

	// Digest is a checksum over every other field.
	Digest string `json:"digest"`
}

// Store owns a checkpoint directory.
type Store struct {
	dir string
}

// NewStore binds a store to dir, creating it if needed.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("checkpoint: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint: create %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the backing directory.
func (s *Store) Dir() string { return s.dir }

// StatePath returns the state file path.
func (s *Store) StatePath() string { return filepath.Join(s.dir, "state.json") }

// StagingPath returns the staged repaired stream path.
func (s *Store) StagingPath() string { return filepath.Join(s.dir, "repaired.part") }

// OutputPath returns the final output path.
func (s *Store) OutputPath() string { return filepath.Join(s.dir, "repaired.rly1") }

// digest computes the state checksum with Digest cleared.
func (st State) digest() (string, error) {
	cp := st
	cp.Digest = ""
	body, err := json.Marshal(cp)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// Valid reports whether the state's checksum matches its body.
func (st State) Valid() bool {
	want, err := st.digest()
	return err == nil && want == st.Digest
}

// Load reads and validates the stored state. A missing state returns
// (State{}, false, nil).
func (s *Store) Load() (State, bool, error) {
	body, err := os.ReadFile(s.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fault.Wrap(fault.Checkpoint, 0, err, "read checkpoint: %v", err)
	}
	var st State
	if err := json.Unmarshal(body, &st); err != nil {
		return State{}, false, fault.Wrap(fault.Checkpoint, 0, err, "decode checkpoint: %v", err)
	}
	if st.Version != Version {
		return State{}, false, fault.New(fault.Checkpoint, 0, "checkpoint version %d unsupported", st.Version)
	}
	if !st.Valid() {
		return State{}, false, fault.New(fault.Checkpoint, 0, "checkpoint checksum mismatch")
	}
	if st.NextIndex < 0 || st.Kept < 0 || st.Dropped < 0 || st.EmittedBytes < 0 {
		return State{}, false, fault.New(fault.Checkpoint, 0, "checkpoint has negative counters")
	}
	if st.EmittedBytes > 0 && len(st.PrefixSHA256) != 64 {
		return State{}, false, fault.New(fault.Checkpoint, 0, "checkpoint prefix digest malformed")
	}
	if st.Done && len(st.OutputSHA256) != 64 {
		return State{}, false, fault.New(fault.Checkpoint, 0, "done checkpoint lacks output digest")
	}
	return st, true, nil
}

// Save commits st atomically. It refuses to overwrite a committed position
// with earlier or conflicting state, and refuses to reopen a finished run.
// When the store already holds a committed prefix, the persisted staging file
// is verified against it: a checkpoint whose staged bytes are missing or
// different is unusable, because resuming would silently change the output.
func (s *Store) Save(st State) error {
	prev, exists, err := s.Load()
	if err != nil {
		return err
	}
	if exists {
		if err := checkMonotonic(prev, st); err != nil {
			return err
		}
	}
	if st.EmittedBytes > 0 {
		got, err := s.stagedPrefixDigest(st.EmittedBytes)
		if err != nil {
			return err
		}
		if got != st.PrefixSHA256 {
			return fault.New(fault.Checkpoint, 0,
				"staged output no longer matches checkpoint prefix digest")
		}
	}
	st.Version = Version
	dg, err := st.digest()
	if err != nil {
		return fmt.Errorf("checkpoint: digest: %w", err)
	}
	st.Digest = dg
	body, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: encode: %w", err)
	}
	body = append(body, '\n')
	return s.writeAtomic(s.StatePath(), body)
}

func checkMonotonic(prev, next State) error {
	if prev.Done && !next.Done {
		return fault.New(fault.Checkpoint, 0,
			"refusing to reopen finished checkpoint (next_index %d)", prev.NextIndex)
	}
	if prev.Done && next.Done && prev.OutputSHA256 != next.OutputSHA256 {
		return fault.New(fault.Checkpoint, 0, "refusing to change committed output digest")
	}
	if next.NextIndex < prev.NextIndex {
		return fault.New(fault.Checkpoint, 0,
			"refusing to rewind commit %d to %d", prev.NextIndex, next.NextIndex)
	}
	if next.NextIndex == prev.NextIndex && next.PrefixSHA256 != prev.PrefixSHA256 &&
		prev.EmittedBytes > 0 && next.EmittedBytes > 0 {
		return fault.New(fault.Checkpoint, 0,
			"refusing to rewrite committed prefix at index %d", next.NextIndex)
	}
	return nil
}

func (s *Store) stagedPrefixDigest(n int64) (string, error) {
	got, err := HashPrefix(s.StagingPath(), n)
	if err != nil {
		return "", fault.Wrap(fault.Checkpoint, 0, err, "verify staged output: %v", err)
	}
	return got, nil
}
