package pipeline

import (
	"encoding/json"
	"errors"
	"os"
)

// Interval is a committed time range in milliseconds on the unified
// timeline.
type Interval struct {
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`
}

// Checkpoint records which intervals are fully committed (outputs and
// manifest atomically on disk). It is only advanced after the outputs
// for an interval have landed.
type Checkpoint struct {
	SourceSHA string     `json:"source_sha"`
	Done      []Interval `json:"done"`
}

// Covers reports whether [startMs, endMs) is fully committed.
func (c *Checkpoint) Covers(startMs, endMs int64) bool {
	for _, iv := range c.Done {
		if iv.StartMs <= startMs && iv.EndMs >= endMs {
			return true
		}
	}
	return false
}

// End returns the highest committed end in milliseconds.
func (c *Checkpoint) End() int64 {
	var end int64
	for _, iv := range c.Done {
		if iv.EndMs > end {
			end = iv.EndMs
		}
	}
	return end
}

// LoadCheckpoint reads the checkpoint file. A missing file yields a
// fresh checkpoint. A corrupted file or one belonging to a different
// source is quarantined (renamed aside) and processing resumes from
// the last verifiable boundary — a fresh checkpoint.
func LoadCheckpoint(path, sourceSHA string) (cp *Checkpoint, quarantined bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Checkpoint{SourceSHA: sourceSHA}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var c Checkpoint
	if json.Unmarshal(data, &c) != nil || c.SourceSHA != sourceSHA {
		if qerr := os.Rename(path, path+".corrupt"); qerr != nil {
			return nil, false, qerr
		}
		return &Checkpoint{SourceSHA: sourceSHA}, true, nil
	}
	return &c, false, nil
}

// SaveCheckpoint persists the checkpoint atomically.
func SaveCheckpoint(path string, c *Checkpoint) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}
