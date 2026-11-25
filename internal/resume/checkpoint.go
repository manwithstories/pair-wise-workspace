// Package resume persists pipeline progress atomically so an interrupted or
// retransmitted run can reuse completed segments. Every checkpoint binds the
// input checksum, track timeline and committed segments; a corrupted or
// mismatching checkpoint rolls back to the last valid boundary.
package resume

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"medpipe/internal/clock"
)

// schemaVersion lets the on-disk format evolve.
const schemaVersion = 1

// SegmentState records one committed output and its fingerprint.
type SegmentState struct {
	Index       int    `json:"index"`
	Output      string `json:"output"`
	Sha256      string `json:"sha256"`
	Fingerprint string `json:"fingerprint"`
	StartMillis int    `json:"start_millis"`
	EndMillis   int    `json:"end_millis"`
	DuplicateOf string `json:"duplicate_of,omitempty"`
}

// TrackState captures the timeline a checkpoint was computed against.
type TrackState struct {
	ID        uint32 `json:"id"`
	Kind      int    `json:"kind"`
	Timescale uint32 `json:"timescale"`
	Duration  uint64 `json:"duration"`
	Channels  uint16 `json:"channels,omitempty"`
	Rate      uint32 `json:"rate,omitempty"`
}

// Checkpoint is the full resumable state for one input file.
type Checkpoint struct {
	Schema      int            `json:"schema"`
	InputPath   string         `json:"input_path"`
	InputSha256 string         `json:"input_sha256"`
	Tracks      []TrackState   `json:"tracks"`
	Segments    []SegmentState `json:"segments"`
}

// Store loads and saves checkpoints next to the working directory.
type Store struct {
	path    string
	tmpPath string
}

// NewStore points at <dir>/checkpoint.json.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, clock.E(clock.KindArgument, "resume.store",
			clock.WithDetail("empty checkpoint directory"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, clock.E(clock.KindOther, "resume.store", clock.WithWrap(err))
	}
	return &Store{
		path:    filepath.Join(dir, "checkpoint.json"),
		tmpPath: filepath.Join(dir, "checkpoint.json.tmp"),
	}, nil
}

// Path returns the checkpoint file location.
func (s *Store) Path() string { return s.path }

// Save writes the checkpoint atomically (temp + fsync + rename).
func (s *Store) Save(cp *Checkpoint) error {
	if cp.InputSha256 == "" || len(cp.InputSha256) != 64 {
		return clock.E(clock.KindArgument, "resume.store",
			clock.WithDetail("checkpoint missing a valid input checksum"))
	}
	cp.Schema = schemaVersion
	sort.SliceStable(cp.Segments, func(i, j int) bool {
		return cp.Segments[i].Index < cp.Segments[j].Index
	})
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return clock.E(clock.KindOther, "resume.store", clock.WithWrap(err))
	}
	if err := os.WriteFile(s.tmpPath, data, 0o644); err != nil {
		return clock.E(clock.KindOther, "resume.store", clock.WithWrap(err))
	}
	f, err := os.Open(s.tmpPath)
	if err != nil {
		return clock.E(clock.KindOther, "resume.store", clock.WithWrap(err))
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return clock.E(clock.KindOther, "resume.store", clock.WithWrap(err))
	}
	if err := f.Close(); err != nil {
		return clock.E(clock.KindOther, "resume.store", clock.WithWrap(err))
	}
	if err := os.Rename(s.tmpPath, s.path); err != nil {
		return clock.E(clock.KindOther, "resume.store", clock.WithWrap(err))
	}
	return nil
}

// Load reads and validates the checkpoint. A missing file returns
// (nil, nil) so callers start fresh; corruption or schema mismatch returns
// KindInconsistent.
func (s *Store) Load() (*Checkpoint, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, clock.E(clock.KindOther, "resume.store", clock.WithWrap(err))
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, clock.E(clock.KindInconsistent, "resume.store",
			clock.WithDetail("checkpoint is not valid JSON"), clock.WithWrap(err))
	}
	if cp.Schema != schemaVersion {
		return nil, clock.E(clock.KindInconsistent, "resume.store",
			clock.WithDetail("checkpoint schema %d, want %d", cp.Schema, schemaVersion))
	}
	if len(cp.InputSha256) != 64 {
		return nil, clock.E(clock.KindInconsistent, "resume.store",
			clock.WithDetail("checkpoint lacks a valid input checksum"))
	}
	return &cp, nil
}

// Verify ensures the checkpoint matches the current input and timeline. On
// mismatch it returns KindInconsistent and the caller must roll back.
func (s *Store) Verify(cp *Checkpoint, inputChecksum string, tracks []TrackState) error {
	if cp.InputSha256 != inputChecksum {
		return clock.E(clock.KindInconsistent, "resume.verify",
			clock.WithDetail("input checksum changed: checkpoint=%s current=%s",
				cp.InputSha256, inputChecksum))
	}
	if len(cp.Tracks) != len(tracks) {
		return clock.E(clock.KindInconsistent, "resume.verify",
			clock.WithDetail("track count changed: %d vs %d",
				len(cp.Tracks), len(tracks)))
	}
	for i := range tracks {
		a, b := cp.Tracks[i], tracks[i]
		if a.ID != b.ID || a.Timescale != b.Timescale || a.Duration != b.Duration {
			return clock.E(clock.KindInconsistent, "resume.verify",
				clock.WithDetail("track %d timeline changed", b.ID))
		}
	}
	return nil
}

// Rollback truncates the segment list to the longest contiguous prefix whose
// output files still exist and match their recorded checksums. It returns the
// number of segments retained (the resume boundary).
func (s *Store) Rollback(cp *Checkpoint) int {
	kept := 0
	for _, seg := range cp.Segments {
		// Contiguity: a gap means everything after it is invalid.
		if seg.Index != kept {
			break
		}
		if !fileMatches(seg.Output, seg.Sha256) {
			break
		}
		kept++
	}
	cp.Segments = cp.Segments[:kept]
	return kept
}

// fileMatches recomputes the SHA-256 of an output and compares it.
func fileMatches(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}

// CommittedCount returns how many segments are already safely on disk.
func (cp *Checkpoint) CommittedCount() int { return len(cp.Segments) }

// HashFile streams a file's SHA-256 without loading it fully into memory.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", clock.E(clock.KindOther, "resume.hash", clock.WithWrap(err))
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", clock.E(clock.KindOther, "resume.hash", clock.WithWrap(err))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
