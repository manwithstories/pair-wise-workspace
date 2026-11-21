// Package checkpoint persists pipeline progress atomically so an
// interrupted run can resume without re-validating committed segments.
package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"mediaroute/internal/model"
)

// SegmentRecord marks one committed, validated segment.
type SegmentRecord struct {
	Index  int    `json:"index"`
	SHA256 string `json:"sha256"`
}

// Checkpoint is the resumable state of a pipeline run.
type Checkpoint struct {
	InputDigest string            `json:"input_digest"`
	Cursors     map[string]uint64 `json:"cursors"` // track -> next expected ext seq
	Committed   []SegmentRecord   `json:"committed"`
	FinalSHA256 string            `json:"final_sha256,omitempty"`
}

// Save writes the checkpoint atomically (temp file + rename).
func Save(path string, c *Checkpoint) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a checkpoint. A missing file is not an error: it returns
// (nil, nil) so callers can start fresh.
func Load(path string) (*Checkpoint, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, &model.MediaError{Code: model.ErrCheckpoint, Module: "checkpoint",
			Msg: "unreadable checkpoint", Cause: err}
	}
	return &c, nil
}

// VerifyDigest rejects a resume when the input changed since the
// checkpoint was written.
func (c *Checkpoint) VerifyDigest(digest string) error {
	if c.InputDigest != digest {
		return &model.MediaError{Code: model.ErrCheckpoint, Module: "checkpoint",
			Msg: fmt.Sprintf("input digest changed: checkpoint=%s run=%s",
				c.InputDigest, digest)}
	}
	return nil
}

// CommittedIndex reports whether a segment was already committed.
func (c *Checkpoint) CommittedIndex(i int) (SegmentRecord, bool) {
	for _, s := range c.Committed {
		if s.Index == i {
			return s, true
		}
	}
	return SegmentRecord{}, false
}
