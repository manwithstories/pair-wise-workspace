// Package checkpoint persists chunked restore progress so a run can resume
// from an interruption without reprocessing verified shards. The cursor only
// advances after a durable write; corrupt or parameter-mismatched state is
// rejected with a locatable error rather than silently repaired.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrParamsChanged means a resume request used different parameters than the
// run that wrote the checkpoint. It maps to exit code 3.
var ErrParamsChanged = errors.New("checkpoint: parameter digest changed, refusing to resume")

// ErrCheckpointCorrupt means the state file or index cannot be parsed or
// trusted. It maps to exit code 3.
var ErrCheckpointCorrupt = errors.New("checkpoint: state file corrupt or unreadable")

// stateName is the single atomically-replaced state document.
const stateName = "checkpoint.json"

// Params is the canonically hashed run-parameter summary.
type Params struct {
	Seed            int64  `json:"seed"`
	LossPercent     int    `json:"loss_percent"`
	ReorderPercent  int    `json:"reorder_percent"`
	WrapAt          uint64 `json:"wrap_at"`
	DriftPPM        int64  `json:"drift_ppm"`
	VFR             bool   `json:"vfr"`
	CheckpointEvery int    `json:"checkpoint_every"`
}

// Digest returns the SHA-256 of the canonical JSON parameter document.
func (p Params) Digest() string {
	b, _ := json.Marshal(p) // deterministic for a fixed struct
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ShardInfo records one complete fragment and its content hash.
type ShardInfo struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// State is the persisted resume document. No wall-clock fields: identical
// inputs and parameters must reproduce an identical document.
type State struct {
	Cursor          int64       `json:"cursor"`
	TimeBaseVersion int         `json:"time_base_version"`
	ParamDigest     string      `json:"param_digest"`
	Shards          []ShardInfo `json:"shards"`
}

// Store owns the output directory, its current state and the tick counter
// that gates checkpoint writes.
type Store struct {
	dir   string
	every int
	state State
	since int // ticks accumulated since the last durable checkpoint
}

// Open creates a fresh store, failing if a checkpoint already exists so an
// unconfirmed input/output directory is never overwritten.
func Open(dir string, every int, params Params) (*Store, error) {
	if every <= 0 {
		every = 256
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint: mkdir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, stateName)); err == nil {
		return nil, fmt.Errorf("checkpoint: %s already exists; refuse to overwrite", stateName)
	}
	s := &Store{dir: dir, every: every, state: State{ParamDigest: params.Digest()}}
	if err := SaveParams(dir, params); err != nil {
		return nil, err
	}
	return s, s.persist(&s.state)
}

// Resume opens an existing store, verifies the parameter digest and every
// shard hash, and returns the state after discarding any incomplete tail.
func Resume(dir string, every int, params Params) (*Store, State, error) {
	raw, err := os.ReadFile(filepath.Join(dir, stateName))
	if err != nil {
		return nil, State{}, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, State{}, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	if st.ParamDigest != params.Digest() {
		return nil, State{}, fmt.Errorf("%w: stored=%s requested=%s",
			ErrParamsChanged, st.ParamDigest, params.Digest())
	}
	s := &Store{dir: dir, every: every, state: st}
	kept, err := s.verifyShards()
	if err != nil {
		return nil, State{}, err
	}
	s.state.Shards = kept
	return s, s.state, nil
}

// WriteShard streams one fragment to a temp name, hashes its content, then
// renames it into place. The returned info is not trusted until the next
// checkpoint persists successfully.
func (s *Store) WriteShard(index int, r io.Reader) (ShardInfo, error) {
	name := fmt.Sprintf("shard-%06d.bin", index)
	final := filepath.Join(s.dir, name)
	tmp := final + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return ShardInfo{}, fmt.Errorf("checkpoint: create shard: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return ShardInfo{}, fmt.Errorf("checkpoint: write shard %d: %w", index, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return ShardInfo{}, fmt.Errorf("checkpoint: rename shard %d: %w", index, err)
	}
	info := ShardInfo{Index: index, Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
	s.state.Shards = append(s.state.Shards, info)
	return info, nil
}

// Advance records processed ticks. Every configured interval it durably
// checkpoints cursor and time-base version; the in-memory cursor only moves
// after the write succeeds, so a failure leaves resumable state untouched.
func (s *Store) Advance(cursor int64, timeBaseVersion int, ticks int) error {
	s.since += ticks
	if s.since < s.every {
		return nil
	}
	next := s.state
	next.Cursor = cursor
	next.TimeBaseVersion = timeBaseVersion
	if err := s.persist(&next); err != nil {
		return err
	}
	s.state = next
	s.since = 0
	return nil
}

// Finalize forces the last checkpoint at end of stream.
func (s *Store) Finalize(cursor int64, timeBaseVersion int) error {
	next := s.state
	next.Cursor = cursor
	next.TimeBaseVersion = timeBaseVersion
	if err := s.persist(&next); err != nil {
		return err
	}
	s.state = next
	return nil
}

// State returns a copy of the current in-memory state.
func (s *Store) State() State { return s.state }

// persist writes state to a temp file and renames it atomically.
func (s *Store) persist(st *State) error {
	final := filepath.Join(s.dir, stateName)
	tmp := final + ".tmp"
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("checkpoint: persist: %w", err) // cursor must not advance
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("checkpoint: rename state: %w", err)
	}
	return nil
}

// verifyShards re-hashes every listed shard and keeps only the intact prefix.
func (s *Store) verifyShards() ([]ShardInfo, error) {
	var kept []ShardInfo
	for _, info := range s.state.Shards {
		sum, size, err := hashFile(filepath.Join(s.dir, info.Name))
		if err != nil || sum != info.SHA256 || size != info.Size {
			// Torn tail or missing fragment: reuse only complete shards.
			break
		}
		kept = append(kept, info)
	}
	return kept, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
