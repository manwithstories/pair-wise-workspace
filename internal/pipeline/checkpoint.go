// Package pipeline wires manifest parsing, clock normalisation, timeline
// ordering, audio rendering and atomic delivery together.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/media"
)

// Stats is the machine-readable summary printed by the CLI.
type Stats struct {
	TargetKbps      int            `json:"target_kbps"`
	ActualKbps      float64        `json:"actual_kbps"`
	BitrateErrorPct float64        `json:"bitrate_error_pct"`
	IntegratedLUFS  float64        `json:"integrated_lufs"`
	TruePeakDBTP    float64        `json:"true_peak_dbtp"`
	ClippedSamples  int64          `json:"clipped_samples"`
	SilentSeconds   float64        `json:"silent_seconds"`
	DriftPPM        float64        `json:"drift_ppm"`
	DriftAnomaly    bool           `json:"drift_anomaly"`
	LateEvents      int            `json:"late_events"`
	ReplayedEvents  int            `json:"replayed_events"`
	ManifestAlignUS float64        `json:"manifest_alignment_us"`
	OutputSHA256    string         `json:"output_sha256"`
	DurationSeconds float64        `json:"duration_seconds"`
	DamagedRanges   int            `json:"damaged_ranges"`
	Damaged         []DamageRecord `json:"damaged,omitempty"`
	OutputPath      string         `json:"output_path"`
	ExitCode        int            `json:"exit_code"`
}

// DamageRecord pins one skipped damaged interval to its source track and raw
// media time, so the report says exactly what was dropped.
type DamageRecord struct {
	Camera   string `json:"camera"`
	Track    string `json:"track"`
	StartRaw int64  `json:"start_raw"`
	EndRaw   int64  `json:"end_raw"`
	Reason   string `json:"reason"`
}

// Checkpoint records how far a run got so an interrupted job can resume
// without redoing verified work.
type Checkpoint struct {
	Version   int      `json:"version"`
	PlanHash  string   `json:"plan_hash"`
	Completed []string `json:"completed"`
	Partial   string   `json:"partial_output,omitempty"`
	Stats     *Stats   `json:"stats,omitempty"`
}

// SaveCheckpoint writes a checkpoint atomically.
func SaveCheckpoint(path string, cp Checkpoint) error {
	cp.Version = 1
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return media.NewError(media.ErrCheckpoint, "encoding checkpoint: %v", err)
	}
	return atomicWrite(path, data)
}

// LoadCheckpoint reads a checkpoint if present.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, media.NewError(media.ErrCheckpoint, "reading checkpoint: %v", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, media.NewError(media.ErrCheckpoint, "parsing checkpoint: %v", err)
	}
	return &cp, nil
}

// HashFile returns the SHA-256 of a file as lowercase hex.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", media.NewError(media.ErrIO, "hashing %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", media.NewError(media.ErrIO, "hashing %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// atomicWrite writes data to path via a sibling temp file and rename, so a
// crash never leaves a partially written file at the final path.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return media.NewError(media.ErrIO, "creating directory %s: %v", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".mediad-*.tmp")
	if err != nil {
		return media.NewError(media.ErrIO, "creating temp file: %v", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return media.NewError(media.ErrIO, "writing temp file: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return media.NewError(media.ErrIO, "syncing temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return media.NewError(media.ErrIO, "closing temp file: %v", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return media.NewError(media.ErrIO, "publishing %s: %v", path, err)
	}
	return nil
}

// AtomicReplace is the exported form used by callers that produce bytes.
func AtomicReplace(path string, data []byte) error { return atomicWrite(path, data) }

// VerifyDelivery checks that a published file exists, is non-empty and matches
// an expected hash (when non-empty).
func VerifyDelivery(path, wantHash string) error {
	info, err := os.Stat(path)
	if err != nil {
		return media.NewError(media.ErrIO, "verifying %s: %v", path, err)
	}
	if info.Size() == 0 {
		return media.NewError(media.ErrIO, "delivery %s is empty", path)
	}
	if wantHash != "" {
		got, err := HashFile(path)
		if err != nil {
			return err
		}
		if got != wantHash {
			return media.NewError(media.ErrIO, "delivery %s hash %s != expected %s", path, got, wantHash)
		}
	}
	return nil
}
