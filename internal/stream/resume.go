// Package stream runs the processing pipeline incrementally and supports
// resuming an interrupted run. Checkpoints record the input digest and a
// per-segment checksum; resume is allowed only when both still match, and
// processing restarts at the nearest complete segment boundary.
package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediaflow/internal/clock"
	"mediaflow/internal/remux"
	"mediaflow/internal/timeline"
)

// Checkpoint is the persisted resume state for one output stream.
type Checkpoint struct {
	Version    int      `json:"version"`
	InputSHA   string   `json:"input_sha256"`
	NextSeg    int      `json:"next_segment"` // index of the next segment to write
	SegSHA     []string `json:"segment_sha"`  // checksum of every completed segment
	WrittenUS  int64    `json:"written_us"`
	OutputName string   `json:"output_name"`
}

const checkpointVersion = 2

// Checkpointer loads and atomically stores resume state under a directory.
type Checkpointer struct {
	path string
}

// NewCheckpointer validates the directory and checkpoint file location.
func NewCheckpointer(dir, name string) (*Checkpointer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, timeline.GlobalError("stream.checkpoint", false, err)
	}
	abs, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		return nil, timeline.GlobalError("stream.checkpoint", false, err)
	}
	return &Checkpointer{path: abs}, nil
}

// Save writes the checkpoint atomically.
func (c *Checkpointer) Save(cp Checkpoint) error {
	cp.Version = checkpointVersion
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return timeline.GlobalError("stream.checkpoint", false, err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return timeline.GlobalError("stream.checkpoint", false, err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return timeline.GlobalError("stream.checkpoint", false, err)
	}
	return nil
}

// Load reads a checkpoint; a missing file returns (zero, nil).
func (c *Checkpointer) Load() (Checkpoint, error) {
	data, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return Checkpoint{}, nil
	}
	if err != nil {
		return Checkpoint{}, timeline.GlobalError("stream.checkpoint", false, err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return Checkpoint{}, timeline.GlobalError("stream.checkpoint", true,
			fmt.Errorf("%w: %v", timeline.ErrCorruptIndex, err))
	}
	if cp.Version != checkpointVersion {
		return Checkpoint{}, timeline.GlobalError("stream.checkpoint", true,
			fmt.Errorf("%w: checkpoint version", timeline.ErrCorruptIndex))
	}
	return cp, nil
}

// Remove deletes the checkpoint after a successful, complete run.
func (c *Checkpointer) Remove() error {
	if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
		return timeline.GlobalError("stream.checkpoint", false, err)
	}
	return nil
}

// VerifyResume validates that a checkpoint still corresponds to the same input
// and that every recorded segment checksum is present in actualSegSHA. Any
// mismatch forces a full restart rather than trusting stale output.
func VerifyResume(cp Checkpoint, inputSHA string, actualSegSHA []string) error {
	if cp.InputSHA != inputSHA {
		return timeline.GlobalError("stream.resume", true,
			fmt.Errorf("%w: input digest changed", timeline.ErrCorruptIndex))
	}
	if cp.NextSeg < 0 || cp.NextSeg > len(actualSegSHA) {
		return timeline.GlobalError("stream.resume", true,
			fmt.Errorf("%w: next segment out of range", timeline.ErrCorruptIndex))
	}
	for i := 0; i < cp.NextSeg; i++ {
		if i >= len(cp.SegSHA) || i >= len(actualSegSHA) || cp.SegSHA[i] != actualSegSHA[i] {
			return timeline.GlobalError("stream.resume", true,
				fmt.Errorf("%w: segment %d checksum mismatch", timeline.ErrCorruptIndex, i))
		}
	}
	return nil
}

// hashSegment returns the SHA-256 of an encoded segment's frame bytes.
func hashSegment(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Segment groups the normalized frames belonging to one cut segment.
type Segment struct {
	Index  int
	Frames []clock.Normalized
}

// ensure remux stays referenced for the writer-driven run in runner.go.
var _ = remux.MaxOutputBytes
