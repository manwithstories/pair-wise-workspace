package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"tsqc/internal/clock"
)

const checkpointVersion = 2

// Checkpoint is the resumable state of a batch run. It records which
// segments were committed with which digest so a restart neither re-submits
// a committed fragment nor trusts stale digests.
type Checkpoint struct {
	Version       int             `json:"version"`
	RunID         string          `json:"run_id"`
	InputDigest   string          `json:"input_sha256"`
	Segments      []SegmentRecord `json:"segments"`
	Recovered     int             `json:"gaps_recovered"`
	Unrecoverable int             `json:"gaps_unrecoverable"`
	NextSegment   int             `json:"next_segment"`
}

// SegmentRecord is one committed fragment.
type SegmentRecord struct {
	Index    int    `json:"index"`
	StartPTS int64  `json:"start_pts"`
	EndPTS   int64  `json:"end_pts"`
	SHA256   string `json:"sha256"`
	Size     int    `json:"size"`
}

// CheckpointPath is the canonical filename inside a state directory.
func CheckpointPath(dir string) string { return filepath.Join(dir, "checkpoint.json") }

// LoadCheckpoint reads a checkpoint. A missing file is a clean start; a
// corrupt or version-mismatched file is reported so the caller can discard
// it and recompute from input.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, clock.FatalError(clock.CodeCheckpoint, "pipeline.LoadCheckpoint", "%v", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, clock.RecoverableError(clock.CodeCheckpoint, "pipeline.LoadCheckpoint", "corrupt checkpoint: %v", err)
	}
	if cp.Version != checkpointVersion {
		return nil, clock.RecoverableError(clock.CodeCheckpoint, "pipeline.LoadCheckpoint", "checkpoint version %d != %d", cp.Version, checkpointVersion)
	}
	sort.Slice(cp.Segments, func(i, j int) bool { return cp.Segments[i].Index < cp.Segments[j].Index })
	for i, rec := range cp.Segments {
		if rec.Index != i {
			return nil, clock.RecoverableError(clock.CodeCheckpoint, "pipeline.LoadCheckpoint", "non-contiguous checkpoint at %d", i)
		}
	}
	return &cp, nil
}

// Save writes the checkpoint atomically via rename.
func (c *Checkpoint) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	c.Version = checkpointVersion
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Commit appends one finished segment. Re-committing the same index with the
// same digest is a no-op and does not advance NextSegment twice.
func (c *Checkpoint) Commit(seg *Segment) bool {
	rec := SegmentRecord{Index: seg.Index, StartPTS: seg.StartPTS, EndPTS: seg.EndPTS, SHA256: seg.Digest(), Size: seg.Size}
	for i := range c.Segments {
		if c.Segments[i].Index == seg.Index {
			if c.Segments[i].SHA256 == rec.SHA256 {
				return false
			}
			c.Segments[i] = rec
			return true
		}
	}
	c.Segments = append(c.Segments, rec)
	sort.Slice(c.Segments, func(i, j int) bool { return c.Segments[i].Index < c.Segments[j].Index })
	c.NextSegment = len(c.Segments)
	return true
}

// Compatible reports whether a loaded checkpoint belongs to the same input
// and run identity and can be resumed.
func (c *Checkpoint) Compatible(runID, inputDigest string) bool {
	return c != nil && c.RunID == runID && c.InputDigest == inputDigest && c.Version == checkpointVersion
}

// Digests returns the committed segment digests in index order.
func (c *Checkpoint) Digests() []string {
	out := make([]string, len(c.Segments))
	for i, s := range c.Segments {
		out[i] = s.SHA256
	}
	return out
}
