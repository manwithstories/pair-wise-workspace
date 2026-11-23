package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"os"
	"path/filepath"

	"mediaroute/internal/fault"
)

// openOutput opens the output stream. On resume it re-reads the existing
// prefix, rebuilds its hash and verifies it against the checkpoint before
// appending; on a fresh run it truncates and starts an empty hash.
func openOutput(path string, cp *Checkpoint, resuming bool) (*os.File, hash.Hash, int64, error) {
	if resuming {
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			return nil, nil, 0, fault.New(fault.CatCheckpointMismatch,
				"output missing on resume: %v", err)
		}
		all, err := io.ReadAll(f)
		if err != nil {
			return nil, nil, 0, fault.New(fault.CatIO, "rehash output: %v", err)
		}
		h := sha256.New()
		h.Write(all)
		if hex.EncodeToString(h.Sum(nil)) != cp.OutputPrefixHash {
			return nil, nil, 0, fault.New(fault.CatCheckpointMismatch,
				"output prefix hash does not match checkpoint")
		}
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return nil, nil, 0, fault.New(fault.CatIO, "seek output: %v", err)
		}
		return f, h, cp.LastSample, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, 0, fault.New(fault.CatIO, "output dir: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, 0, fault.New(fault.CatIO, "open output: %v", err)
	}
	return f, sha256.New(), -1, nil
}

// verifyCheckpoint enforces the three-way resume identity: input digest,
// last committed sample range and a non-empty output prefix hash.
func verifyCheckpoint(cp *Checkpoint, cfg StreamConfig) *fault.Error {
	if cp.InputSHA256 != cfg.Report.InputSHA256 {
		return fault.New(fault.CatCheckpointMismatch,
			"checkpoint input digest differs; refusing to resume")
	}
	if cp.LastSample < -1 || cp.LastSample >= int64(cfg.Spec.SampleCount) {
		return fault.New(fault.CatCheckpointMismatch,
			"checkpoint last sample %d out of range", cp.LastSample)
	}
	if cp.OutputPrefixHash == "" {
		return fault.New(fault.CatCheckpointMismatch,
			"checkpoint missing output prefix hash")
	}
	return nil
}

// loadCheckpoint reads a checkpoint; a missing file is a normal fresh run.
func loadCheckpoint(path string) (*Checkpoint, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fault.New(fault.CatIO, "checkpoint read: %v", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, fault.New(fault.CatCheckpointMismatch,
			"checkpoint corrupt: %v", err)
	}
	return &cp, nil
}

// writeCheckpoint commits via temp-file + rename for crash safety.
func writeCheckpoint(path string, cp *Checkpoint) error {
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fault.New(fault.CatIO, "checkpoint marshal: %v", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fault.New(fault.CatIO, "checkpoint write: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fault.New(fault.CatIO, "checkpoint commit: %v", err)
	}
	return nil
}

// ResetFromSafeBoundary discards an unverifiable checkpoint and its output
// so the next Stream call recomputes from the beginning (the only safe
// boundary when the three-way checkpoint verification fails).
func ResetFromSafeBoundary(cfg StreamConfig) error {
	if err := os.Remove(cfg.Checkpoint); err != nil && !os.IsNotExist(err) {
		return fault.New(fault.CatIO, "remove checkpoint: %v", err)
	}
	if err := os.Remove(cfg.OutputPath); err != nil && !os.IsNotExist(err) {
		return fault.New(fault.CatIO, "remove partial output: %v", err)
	}
	return nil
}
