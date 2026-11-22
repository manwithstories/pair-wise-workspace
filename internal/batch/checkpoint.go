// Package batch runs bounded-memory block processing with durable checkpoints
// and atomic local index writes.
package batch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"avscan/internal/model"
)

// Checkpoint is persisted next to the index and validated before a resume.
type Checkpoint struct {
	// InputDigest is the SHA-256 over the canonical input summary (not the
	// raw media), so any input change invalidates a resume.
	InputDigest   string `json:"input_digest"`
	BlockSize     int    `json:"block_size"`
	Completed     int    `json:"completed_blocks"`
	// ResultDigest chains block result hashes; a mismatch means the stored
	// index does not correspond to reprocessed blocks.
	ResultDigest  string `json:"result_digest"`
	TotalSamples  int    `json:"total_samples"`
	SchemaVersion int    `json:"schema_version"`
}

const schemaVersion = 1

// InputSummary is the canonical description whose hash binds a checkpoint to
// one concrete input. Source is excluded: identity depends on content, not on
// the file's location, so an index rebuilt at a different path still matches.
type InputSummary struct {
	Source       string `json:"-"`
	FileSize     int64  `json:"file_size"`
	TrackCount   int    `json:"track_count"`
	SampleCounts []int  `json:"sample_counts"`
	BlockSize    int    `json:"block_size"`
}

// Digest returns the SHA-256 of the canonical JSON input summary.
func (s InputSummary) Digest() (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func checkpointPath(indexPath string) string {
	return indexPath + ".checkpoint"
}

// SaveCheckpoint atomically writes a checkpoint via temp-file + rename.
func SaveCheckpoint(indexPath string, cp Checkpoint) error {
	cp.SchemaVersion = schemaVersion
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(indexPath)
	tmp := filepath.Join(dir, ".checkpoint.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("%w: %v", model.ErrIndexWrite, err)
	}
	if err := os.Rename(tmp, checkpointPath(indexPath)); err != nil {
		return fmt.Errorf("%w: %v", model.ErrIndexWrite, err)
	}
	return nil
}

// LoadCheckpoint reads and validates a checkpoint against the current input.
// A missing checkpoint is reported as (nil, nil) so a fresh run can start.
func LoadCheckpoint(indexPath string, summary InputSummary) (*Checkpoint, error) {
	raw, err := os.ReadFile(checkpointPath(indexPath))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", model.ErrIndexWrite, err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, fmt.Errorf("%w: corrupt checkpoint: %v",
			model.ErrCheckpointMismatch, err)
	}
	if cp.SchemaVersion != schemaVersion {
		return nil, fmt.Errorf("%w: schema %d != %d",
			model.ErrCheckpointMismatch, cp.SchemaVersion, schemaVersion)
	}
	digest, err := summary.Digest()
	if err != nil {
		return nil, err
	}
	if cp.InputDigest != digest || cp.BlockSize != summary.BlockSize {
		return nil, fmt.Errorf("%w: input summary changed since checkpoint",
			model.ErrCheckpointMismatch)
	}
	return &cp, nil
}

// chainHash folds one block's result hash into the running result digest.
func chainHash(prev, blockResult string) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte(blockResult))
	return hex.EncodeToString(h.Sum(nil))
}
