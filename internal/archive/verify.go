package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"archiver/internal/clock"
)

// Verify recomputes every segment hash listed in the manifest and
// reports the match ratio plus the first mismatch, if any.
func Verify(dir string) (matched, total int, firstErr error, err error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return 0, 0, nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, 0, nil, fmt.Errorf("parse manifest: %w", err)
	}
	for _, seg := range m.Segments {
		total++
		content, err := os.ReadFile(filepath.Join(dir, seg.File))
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("read %s: %w", seg.File, err)
			}
			continue
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) == seg.SHA256 {
			matched++
		} else if firstErr == nil {
			firstErr = fmt.Errorf("hash mismatch: %s", seg.File)
		}
	}
	return matched, total, firstErr, nil
}

// Checkpoint captures the state needed to resume an interrupted run:
// input digest, parameters, clock state and the committed watermark.
type Checkpoint struct {
	InputDigest       string                    `json:"input_digest"`
	ParamsHash        string                    `json:"params_hash"`
	SegmentsCommitted int                       `json:"segments_committed"`
	WatermarkUs       int64                     `json:"watermark_us"`
	Clock             map[uint32]clock.Snapshot `json:"clock"`
	SegmentHashes     map[int64]string          `json:"segment_hashes"`
}

// SaveCheckpoint atomically writes checkpoint.json.
func SaveCheckpoint(dir string, cp Checkpoint) error {
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "checkpoint.json"), b)
}

// LoadCheckpoint reads checkpoint.json; os.IsNotExist applies when absent.
func LoadCheckpoint(dir string) (Checkpoint, error) {
	var cp Checkpoint
	b, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		return cp, err
	}
	if err := json.Unmarshal(b, &cp); err != nil {
		return cp, fmt.Errorf("parse checkpoint: %w", err)
	}
	return cp, nil
}

// CheckResume validates a checkpoint against the current run and
// returns a human-readable refusal reason, or "" when resume is legal.
func CheckResume(cp Checkpoint, inputDigest string, p Params) string {
	if cp.InputDigest != inputDigest {
		return fmt.Sprintf("input digest mismatch: checkpoint %s, current %s",
			cp.InputDigest, inputDigest)
	}
	if cp.ParamsHash != p.Hash() {
		return fmt.Sprintf("processing parameters differ from checkpoint (hash %s != %s)",
			cp.ParamsHash, p.Hash())
	}
	return ""
}
