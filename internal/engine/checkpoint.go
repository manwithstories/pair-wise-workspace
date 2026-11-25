package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/fieldcut/fieldcut/internal/base"
)

// Checkpoint is the durable resume state, committed atomically beside the
// segments it describes.
type Checkpoint struct {
	Seed          uint64            `json:"seed"`
	NextSegment   map[string]int    `json:"next_segment"`
	SegmentHashes map[string]string `json:"segment_hashes"` // key: rendition/index
	Faults        []FaultRecord     `json:"faults"`
}

// FaultRecord is the JSON form of a base.Fault for checkpoints and reports.
type FaultRecord struct {
	Kind        string `json:"kind"`
	Camera      string `json:"camera"`
	Rendition   string `json:"rendition"`
	PTS         uint64 `json:"pts"`
	ByteOffset  int64  `json:"byte_offset"`
	Recoverable bool   `json:"recoverable"`
	Detail      string `json:"detail"`
}

// Record converts a base.Fault for persistence.
func Record(f *base.Fault) FaultRecord {
	return FaultRecord{
		Kind: string(f.Kind), Camera: f.Camera, Rendition: f.Rendition,
		PTS: f.PTS, ByteOffset: f.ByteOffset, Recoverable: f.Recoverable, Detail: f.Detail,
	}
}

func (b *Builder) checkpointPath() string {
	return filepath.Join(b.Dir, "checkpoint.json")
}

// loadCheckpoint resumes state from an existing checkpoint, refusing to mix
// a different seed into the current run.
func (b *Builder) loadCheckpoint() error {
	raw, err := os.ReadFile(b.checkpointPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return err
	}
	if cp.Seed != b.Seed {
		return errors.New("checkpoint seed mismatch; refusing to resume")
	}
	b.cp = &cp
	if b.cp.NextSegment == nil {
		b.cp.NextSegment = map[string]int{}
	}
	if b.cp.SegmentHashes == nil {
		b.cp.SegmentHashes = map[string]string{}
	}
	return nil
}

// persistCheckpoint writes the checkpoint atomically: temp file plus rename,
// so a crash never leaves a half-written manifest.
func (b *Builder) persistCheckpoint() error {
	raw, err := json.MarshalIndent(b.cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.checkpointPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.checkpointPath())
}
