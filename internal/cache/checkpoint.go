package cache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Checkpoint records one completed stage. Checkpoints are written
// atomically (temp file + rename + sync) so an interrupted process
// never leaves a half-written checkpoint behind.
type Checkpoint struct {
	Stage          string `json:"stage"`
	Key            string `json:"key"`
	ResultDigest   string `json:"result_digest"`
	DurationMicros int64  `json:"duration_micros"`
}

// CheckpointPath returns the checkpoint path for a stage.
func CheckpointPath(dir, stage string) string {
	return filepath.Join(dir, stage+".json")
}

// WriteCheckpoint atomically writes a stage checkpoint.
func WriteCheckpoint(dir, stage string, cp *Checkpoint) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := CheckpointPath(dir, stage) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, CheckpointPath(dir, stage))
}

// ReadCheckpoint loads a stage checkpoint; nil, nil when absent.
func ReadCheckpoint(dir, stage string) (*Checkpoint, error) {
	data, err := os.ReadFile(CheckpointPath(dir, stage))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// ValidCheckpoint reports whether a usable checkpoint exists for the
// stage with the given key: the checkpoint must parse, match the key,
// and its recorded result digest must match the cache entry for that
// key. Any failure means the stage must be recomputed.
func ValidCheckpoint(dir, stage, key string, store *Store) (*Checkpoint, bool) {
	cp, err := ReadCheckpoint(dir, stage)
	if err != nil || cp == nil {
		return nil, false
	}
	if cp.Key != key || cp.Stage != stage {
		return nil, false
	}
	entry, err := store.Get(key)
	if err != nil || entry == nil {
		return nil, false
	}
	if entry.ResultDigest != cp.ResultDigest {
		return nil, false
	}
	return cp, true
}
