// Package index persists incremental processing checkpoints so an
// interrupted build can resume from the last committed boundary instead of
// rerunning from scratch. Checkpoints are written atomically every fixed
// number of processed samples and validated against the input fingerprint
// and configuration before any resume is allowed.
package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mediapack/internal/manifest"
)

// FileName is the canonical checkpoint file inside the index directory.
const FileName = "checkpoint.json"

// ErrMismatch rejects a resume when the checkpoint does not belong to the
// current input and configuration (exit code 4).
var ErrMismatch = errors.New("checkpoint does not match input or configuration")

// TrackStat accumulates per-track processing progress.
type TrackStat struct {
	TrackID uint32 `json:"track_id"`
	Samples uint64 `json:"samples"`
	Bytes   uint64 `json:"bytes"`
	Skipped uint64 `json:"skipped"`
}

// Checkpoint is the resumable processing state.
type Checkpoint struct {
	Seq              uint64                  `json:"seq"`
	InputFingerprint string                  `json:"input_fingerprint"`
	ConfigHash       string                  `json:"config_hash"`
	SamplesProcessed uint64                  `json:"samples_processed"`
	Segments         []manifest.SegmentEntry `json:"segments"`
	TrackStats       []TrackStat             `json:"track_stats"`
	Missing          []manifest.Interval     `json:"missing"`
}

// Writer saves checkpoints atomically into an index directory.
type Writer struct {
	dir string
	seq uint64
}

// NewWriter creates (or reuses) the index directory under outDir.
func NewWriter(outDir string) (*Writer, error) {
	dir := filepath.Join(outDir, "index")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Writer{dir: dir}, nil
}

// Save persists the checkpoint with a monotonically increasing sequence
// number via tmp-file + rename.
func (w *Writer) Save(cp *Checkpoint) error {
	cp.Seq = w.seq
	w.seq++
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(w.dir, FileName+".tmp")
	final := filepath.Join(w.dir, FileName)
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Load reads the checkpoint from the index directory under outDir. It
// returns os.ErrNotExist wrapped in the error when none exists.
func Load(outDir string) (*Checkpoint, error) {
	raw, err := os.ReadFile(filepath.Join(outDir, "index", FileName))
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, fmt.Errorf("parse checkpoint: %w", err)
	}
	return &cp, nil
}

// Validate checks the checkpoint against the current input fingerprint and
// configuration hash. Any mismatch is reported as ErrMismatch with the
// specific reason, and the caller must refuse to resume.
func (c *Checkpoint) Validate(fingerprint, configHash string) error {
	if c.InputFingerprint != fingerprint {
		return fmt.Errorf("%w: input fingerprint changed (%s != %s)",
			ErrMismatch, c.InputFingerprint, fingerprint)
	}
	if c.ConfigHash != configHash {
		return fmt.Errorf("%w: configuration changed", ErrMismatch)
	}
	return nil
}

// CommittedBoundaries returns the number of segment boundaries already
// committed in this checkpoint.
func (c *Checkpoint) CommittedBoundaries() int { return len(c.Segments) }
