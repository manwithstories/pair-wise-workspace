package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"bcast/internal/fault"
	"bcast/internal/safe"
)

// Checkpoint is the resumable batch commit marker.
type Checkpoint struct {
	Version       string            `json:"version"`
	KeyHash       string            `json:"key_hash"`
	InputDigests  map[string]string `json:"input_digests"`
	LastCompleted int               `json:"last_completed"`
	SegmentSHA256 map[int]string    `json:"segment_sha256"`
	ResumedCount  int               `json:"resumed_count,omitempty"`
}

func checkpointRel() string { return "state/checkpoint.json" }

// LoadCheckpoint reads and validates a checkpoint. Missing is not an error.
func LoadCheckpoint(root *safe.Root) (*Checkpoint, bool, error) {
	b, err := root.ReadFile(checkpointRel())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		var fe *fault.Error
		if errors.As(err, &fe) {
			for cause := fe.Err; cause != nil; cause = errors.Unwrap(cause) {
				if os.IsNotExist(cause) {
					return nil, false, nil
				}
			}
		}
		return nil, false, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, false, fault.E(fault.CheckpointInvalid, "engine", "LoadCheckpoint", err).WithPath(checkpointRel())
	}
	if cp.SegmentSHA256 == nil {
		cp.SegmentSHA256 = map[int]string{}
	}
	if cp.LastCompleted < -1 {
		cp.LastCompleted = -1
	}
	return &cp, true, nil
}

// NewCheckpoint creates an empty contiguous checkpoint.
func NewCheckpoint(keyHash string, inputs map[string]string) *Checkpoint {
	return &Checkpoint{
		KeyHash:       keyHash,
		InputDigests:  inputs,
		LastCompleted: -1,
		SegmentSHA256: map[int]string{},
	}
}

// Save atomically persists the checkpoint after a segment manifest commits.
func (c *Checkpoint) Save(root *safe.Root) error {
	c.Version = "bcast.checkpoint/v1"
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fault.E(fault.CheckpointInvalid, "engine", "Save.marshal", err)
	}
	rel := checkpointRel()
	p, err := root.Resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fault.E(fault.WriteFailed, "engine", "Save.mkdir", err).WithPath(rel)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fault.E(fault.WriteFailed, "engine", "Save.write", err).WithPath(rel)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fault.E(fault.WriteFailed, "engine", "Save.rename", err).WithPath(rel)
	}
	return nil
}

// MarkCompleted records one committed segment in contiguous order.
func (c *Checkpoint) MarkCompleted(index int, sha string, inputs map[string]string, keyHash string) error {
	if c.SegmentSHA256 == nil {
		c.SegmentSHA256 = map[int]string{}
	}
	want := c.LastCompleted + 1
	if index != want {
		return fault.E(fault.CheckpointInvalid, "engine", "MarkCompleted",
			errors.New("non-contiguous segment commit")).WithSegment(index)
	}
	c.KeyHash = keyHash
	c.InputDigests = inputs
	c.SegmentSHA256[index] = sha
	c.LastCompleted = index
	return nil
}
