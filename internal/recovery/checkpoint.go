// Package recovery persists per-representation checkpoints so an
// interrupted run can resume independently. Checkpoints are written
// atomically (tmp + fsync + rename); a resume is refused when either
// the source digest or the config digest has changed.
package recovery

import (
	"encoding/json"
	"os"
	"path/filepath"

	"mediac/internal/model"
	"mediac/internal/segment"
)

// Checkpoint is the recoverable state of one representation.
type Checkpoint struct {
	RepID        string                 `json:"rep_id"`
	SourceSHA256 string                 `json:"source_sha256"`
	ConfigSHA256 string                 `json:"config_sha256"`
	Completed    []segment.SegmentEntry `json:"completed"`
}

// Path returns the checkpoint file path of a representation.
func Path(dir, repID string) string {
	return filepath.Join(dir, "checkpoints", repID+".json")
}

// Save atomically persists the checkpoint.
func Save(dir string, c *Checkpoint) *model.MediaError {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return model.New(model.StageRecovery, "marshal_failed", err.Error(), 1)
	}
	p := Path(dir, c.RepID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return model.New(model.StageRecovery, "write_failed", err.Error(), 1)
	}
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return model.New(model.StageRecovery, "write_failed", err.Error(), 1)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return model.New(model.StageRecovery, "write_failed", err.Error(), 1)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return model.New(model.StageRecovery, "fsync_failed", err.Error(), 1)
	}
	f.Close()
	if err := os.Rename(tmp, p); err != nil {
		return model.New(model.StageRecovery, "rename_failed", err.Error(), 1)
	}
	return nil
}

// Load reads a checkpoint; a missing checkpoint means a fresh run.
func Load(dir, repID string) (*Checkpoint, *model.MediaError) {
	b, err := os.ReadFile(Path(dir, repID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, model.New(model.StageRecovery, "read_failed", err.Error(), 1)
	}
	var c Checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, model.New(model.StageRecovery, "checkpoint_corrupt",
			repID+": "+err.Error(), model.ExitCheckpointMismatch)
	}
	return &c, nil
}

// Validate refuses resuming when source or config digests changed.
func (c *Checkpoint) Validate(srcSHA, cfgSHA string) *model.MediaError {
	if c.SourceSHA256 != srcSHA {
		return model.New(model.StageRecovery, "source_changed",
			c.RepID+": source digest changed since checkpoint, refusing to resume",
			model.ExitCheckpointMismatch)
	}
	if c.ConfigSHA256 != cfgSHA {
		return model.New(model.StageRecovery, "config_changed",
			c.RepID+": config digest changed since checkpoint, refusing to resume",
			model.ExitCheckpointMismatch)
	}
	return nil
}

// Has reports whether a segment index is already completed.
func (c *Checkpoint) Has(index int) bool {
	for _, e := range c.Completed {
		if e.Index == index {
			return true
		}
	}
	return false
}
