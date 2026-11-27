package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CheckpointVersion is bumped if the on-disk shape changes incompatibly.
const CheckpointVersion = 1

// Checkpoint records durable progress. It is written only after the
// corresponding index output has been validated and fsynced.
type Checkpoint struct {
	Version       int    `json:"version"`
	// CompletedBlocks are block IDs already fully processed and indexed.
	CompletedBlocks []string `json:"completed_blocks"`
	// LastBlock is the highest completed block ID in processing order.
	LastBlock string `json:"last_block"`
	// IndexHash pins the index content the checkpoint was advanced against.
	IndexHash string `json:"index_hash"`
	// RunID identifies the generating run; a mismatch rejects stale resumes.
	RunID string `json:"run_id"`
}

// CheckpointManager loads, validates and advances a single checkpoint file.
type CheckpointManager struct {
	path string
	cp   Checkpoint
	// failNext, when true, makes the next Advance simulate a disk failure and
	// must leave the previous checkpoint untouched.
	failNext bool
}

// NewCheckpointManager targets workDir/index/checkpoint.json.
func NewCheckpointManager(workDir, runID string) (*CheckpointManager, error) {
	root := filepath.Join(workDir, "index")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint: mkdir: %w", err)
	}
	cm := &CheckpointManager{
		path: filepath.Join(root, "checkpoint.json"),
		cp:   Checkpoint{Version: CheckpointVersion, RunID: runID},
	}
	return cm, nil
}

// Load reads and validates an existing checkpoint. A missing file is normal for
// a fresh run and yields an empty checkpoint without error. Corrupt or
// mismatched content is rejected so an invalid checkpoint is never resumed.
func (cm *CheckpointManager) Load() (*Checkpoint, error) {
	data, err := os.ReadFile(cm.path)
	if os.IsNotExist(err) {
		return &cm.cp, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read: %w", err)
	}
	var loaded Checkpoint
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("checkpoint: corrupt: %w", err)
	}
	if loaded.Version != CheckpointVersion {
		return nil, fmt.Errorf("checkpoint: unsupported version %d", loaded.Version)
	}
	if loaded.RunID != cm.cp.RunID {
		return nil, fmt.Errorf("checkpoint: run id mismatch (stale checkpoint)")
	}
	cm.cp = loaded
	return &cm.cp, nil
}

// IsComplete reports whether a block ID is already checkpointed.
func (cm *CheckpointManager) IsComplete(blockID string) bool {
	return contains(cm.cp.CompletedBlocks, blockID)
}

// FailNext arms a simulated disk failure for the next Advance call.
func (cm *CheckpointManager) FailNext() { cm.failNext = true }

// Advance marks one more block complete. It refuses to move unless the supplied
// index hash is non-empty. On a simulated or real write failure the on-disk
// checkpoint is left unchanged and the error is surfaced.
func (cm *CheckpointManager) Advance(blockID, indexHash string) error {
	if indexHash == "" {
		return fmt.Errorf("checkpoint: refusing to advance without validated index hash")
	}
	if cm.IsComplete(blockID) {
		return nil // idempotent
	}

	next := Checkpoint{
		Version:         CheckpointVersion,
		CompletedBlocks: append(append([]string{}, cm.cp.CompletedBlocks...), blockID),
		LastBlock:       blockID,
		IndexHash:       indexHash,
		RunID:           cm.cp.RunID,
	}

	if cm.failNext {
		cm.failNext = false
		return fmt.Errorf("checkpoint: simulated disk write failure")
	}

	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: marshal: %w", err)
	}
	if err := atomicWrite(cm.path, append(data, '\n')); err != nil {
		// In-memory state is intentionally not mutated on failure.
		return err
	}
	cm.cp = next
	return nil
}

// Current returns a copy of the in-memory checkpoint.
func (cm *CheckpointManager) Current() Checkpoint { return cm.cp }

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
