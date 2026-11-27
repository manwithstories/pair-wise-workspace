// Package checkpoint atomically persists resume state so an
// interrupted run can continue from the last commit boundary.
package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FileName is the checkpoint file inside a run directory.
const FileName = "checkpoint.json"

// State is the committed resume state. Committed counts events
// confirmed at a commit boundary (at most CommitBatch apart), and
// FrameSums holds the checksums of fully rendered frames.
type State struct {
	Committed int64    `json:"committed"`
	FrameSums []string `json:"frame_sums"`
}

// Save writes state atomically: temp file, fsync, rename.
func Save(dir string, st State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	tmp := filepath.Join(dir, FileName+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if err := json.NewEncoder(f).Encode(st); err != nil {
		f.Close()
		return fmt.Errorf("checkpoint: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("checkpoint: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, FileName)); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}

// Load reads the checkpoint. ok=false means no checkpoint exists;
// a corrupt checkpoint is an error, never silently ignored.
func Load(dir string) (st State, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("checkpoint: %w", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, false, fmt.Errorf("checkpoint: corrupt state: %w", err)
	}
	if st.Committed < 0 {
		return State{}, false, fmt.Errorf("checkpoint: negative committed count")
	}
	return st, true, nil
}
