// Package manifest writes the segment listing using a temporary file plus
// an atomic rename and a completion marker, so a crashed run never leaves a
// half-written manifest behind and repeated runs produce identical output.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Interval is a half-open gap on the media timeline, in milliseconds.
type Interval struct {
	StartMs float64 `json:"start_ms"`
	EndMs   float64 `json:"end_ms"`
	Reason  string  `json:"reason"`
}

// SegmentEntry describes one committed segment.
type SegmentEntry struct {
	Index          int     `json:"index"`
	StartMs        float64 `json:"start_ms"`
	EndMs          float64 `json:"end_ms"`
	KeyframeSample uint32  `json:"keyframe_sample"`
}

// Representation is one bitrate ladder rung.
type Representation struct {
	ID                  string  `json:"id"`
	TrackID             uint32  `json:"track_id"`
	TargetBitrate       uint64  `json:"target_bitrate"`
	ActualBitrate       uint64  `json:"actual_bitrate"`
	BitrateDeviationPct float64 `json:"bitrate_deviation_pct"`
	MaxAlignmentErrorMs float64 `json:"max_alignment_error_ms"`
	Aligned             bool    `json:"aligned"`
}

// Quality tallies sample-level quality statistics.
type Quality struct {
	Samples      uint64 `json:"quality_samples"`
	Skipped      uint64 `json:"quality_skipped"`
	Unmeasurable uint64 `json:"quality_unmeasurable"`
	Bytes        uint64 `json:"bytes"`
}

// Manifest is the top-level segment listing.
type Manifest struct {
	Version             int              `json:"version"`
	Input               string           `json:"input"`
	Fingerprint         string           `json:"fingerprint"`
	SegmentTargetMs     int64            `json:"segment_target_ms"`
	ToleranceMs         int64            `json:"tolerance_ms"`
	Segments            []SegmentEntry   `json:"segments"`
	Representations     []Representation `json:"representations"`
	Quality             Quality          `json:"quality"`
	MissingIntervals    []Interval       `json:"missing_intervals"`
	UntrustedIntervals  []Interval       `json:"untrusted_intervals"`
	MaxAlignmentErrorMs float64          `json:"max_alignment_error_ms"`
	CheckpointRecovered bool             `json:"checkpoint_recovered"`
	Digest              string           `json:"digest"`
}

// Name is the canonical manifest file name; CompleteName is the marker
// written after the manifest rename succeeded.
const (
	Name         = "manifest.json"
	CompleteName = "manifest.complete"
)

// digest computes a stable content hash over the fields that must be
// identical across repeated runs of the same input and configuration.
func (m *Manifest) digest() string {
	core := struct {
		Segments        []SegmentEntry   `json:"segments"`
		Representations []Representation `json:"representations"`
		Missing         []Interval       `json:"missing_intervals"`
	}{m.Segments, m.Representations, m.MissingIntervals}
	raw, _ := json.Marshal(core)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Write stores the manifest atomically: stale temporary files are removed,
// the payload goes to a tmp file, is fsynced, renamed into place, and a
// completion marker is written last.
func Write(dir string, m *Manifest) error {
	if err := Clean(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m.Digest = m.digest()
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, Name+".tmp")
	final := filepath.Join(dir, Name)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
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
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, CompleteName), []byte(m.Digest+"\n"), 0o644)
}

// Load reads a manifest file.
func Load(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// Clean removes uncommitted temporary files and stale completion markers
// from an output directory. It never touches the manifest itself or any
// path outside dir.
func Clean(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || name == CompleteName {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}
