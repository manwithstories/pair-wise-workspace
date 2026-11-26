// Package pipeline runs the bounded, resumable execution phase: it
// streams segments from the scan index, analyzes structure, keyframes
// and hashes, and persists an atomic checkpoint after every segment.
package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediascan/internal/core"
	"mediascan/internal/index"
)

// SegState is the lifecycle state of one segment. Only three states
// exist: pending, processing, or a terminal state (done / failed).
type SegState string

const (
	StatePending    SegState = "pending"
	StateProcessing SegState = "processing"
	StateDone       SegState = "done"   // terminal: analyzed cleanly
	StateFailed     SegState = "failed" // terminal: damaged, range marked
)

// Terminal reports whether the state is final.
func (s SegState) Terminal() bool { return s == StateDone || s == StateFailed }

// SegResult is the terminal analysis record for one segment.
type SegResult struct {
	ID        string       `json:"id"`
	Path      string       `json:"path"`
	State     SegState     `json:"state"`
	SHA256    string       `json:"sha256,omitempty"`
	Boxes     []string     `json:"boxes,omitempty"`
	Keyframes []int        `json:"keyframes,omitempty"`
	Gaps      []int        `json:"gaps,omitempty"`
	Start     core.Rat     `json:"start"`
	End       core.Rat     `json:"end"`
	Errors    []string     `json:"errors,omitempty"`
	Damage    []core.Range `json:"damage,omitempty"`
}

// Checkpoint is the atomically persisted execution state.
type Checkpoint struct {
	InputDigest  string                `json:"input_digest"`
	ConfigDigest string                `json:"config_digest"`
	States       map[string]SegState   `json:"states"`
	Results      map[string]*SegResult `json:"results"`
	Processed    int                   `json:"processed"` // terminal-state count
}

// CheckpointFileName is the state-file name for the checkpoint.
const CheckpointFileName = "checkpoint.json"

// NewCheckpoint creates a fresh checkpoint for the index.
func NewCheckpoint(idx *index.Index) *Checkpoint {
	c := &Checkpoint{
		InputDigest:  idx.InputDigest,
		ConfigDigest: idx.ConfigDigest,
		States:       map[string]SegState{},
		Results:      map[string]*SegResult{},
	}
	for _, s := range idx.Segments {
		c.States[s.ID] = StatePending
	}
	return c
}

// LoadCheckpoint reads the stored checkpoint when its digests match the
// index; otherwise a fresh checkpoint is returned (the stale one is
// discarded, mirroring index invalidation). Segments left "processing"
// by an interrupted run are reset to "pending".
func LoadCheckpoint(stateDir string, idx *index.Index) (*Checkpoint, error) {
	path := filepath.Join(stateDir, CheckpointFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return NewCheckpoint(idx), nil
	}
	var c Checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("corrupt checkpoint %s: %w", path, err)
	}
	if c.InputDigest != idx.InputDigest || c.ConfigDigest != idx.ConfigDigest {
		return NewCheckpoint(idx), nil
	}
	for id, st := range c.States {
		if st == StateProcessing {
			c.States[id] = StatePending
		}
	}
	for _, s := range idx.Segments {
		if _, ok := c.States[s.ID]; !ok {
			c.States[s.ID] = StatePending
		}
	}
	return &c, nil
}

// Save persists the checkpoint atomically.
func (c *Checkpoint) Save(stateDir string) error {
	c.Processed = 0
	for _, st := range c.States {
		if st.Terminal() {
			c.Processed++
		}
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	return index.AtomicWriteJSON(filepath.Join(stateDir, CheckpointFileName), c)
}
