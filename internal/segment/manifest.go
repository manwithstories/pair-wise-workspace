package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"mediac/internal/clock"
	"mediac/internal/model"
)

// SegmentEntry describes one segment of one representation.
type SegmentEntry struct {
	Index        int    `json:"index"`
	Start        string `json:"start"` // exact rational "value/timescale"
	End          string `json:"end"`
	StartMs      int64  `json:"start_ms"` // exact; never rounded
	EndMs        int64  `json:"end_ms"`
	SHA256       string `json:"sha256"`
	Bytes        int64  `json:"bytes"`
	RTPTimestamp uint32 `json:"rtp_timestamp"`
}

// RepManifest is the per-representation section of the manifest.
type RepManifest struct {
	ID           string         `json:"id"`
	Source       string         `json:"source"`
	SourceSHA256 string         `json:"source_sha256"`
	Segments     []SegmentEntry `json:"segments"`
}

// Manifest is the deterministic delivery manifest. It contains no
// wall-clock fields, so repeated runs produce identical bytes.
type Manifest struct {
	Version           int           `json:"version"`
	ConfigSHA256      string        `json:"config_sha256"`
	SegmentDurationMs int64         `json:"segment_duration_ms"`
	Representations   []RepManifest `json:"representations"`
}

// Name is the manifest file name inside the output directory.
const Name = "manifest.json"

// Entry builds a SegmentEntry; cut times must be exact whole
// milliseconds, otherwise the run fails instead of rounding.
func Entry(index int, start, end clock.Rational, sha string, bytes int64, rtp uint32) (SegmentEntry, *model.MediaError) {
	sms, ok := start.MillisExact()
	if !ok {
		return SegmentEntry{}, model.New(model.StageSegment, "inexact_cut",
			"segment start is not an exact millisecond: "+start.String(), 1)
	}
	ems, ok := end.MillisExact()
	if !ok {
		return SegmentEntry{}, model.New(model.StageSegment, "inexact_cut",
			"segment end is not an exact millisecond: "+end.String(), 1)
	}
	return SegmentEntry{Index: index, Start: start.String(), End: end.String(),
		StartMs: sms, EndMs: ems, SHA256: sha, Bytes: bytes, RTPTimestamp: rtp}, nil
}

// Write stores the manifest deterministically and returns its SHA-256.
func Write(dir string, m *Manifest) (string, *model.MediaError) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", model.New(model.StageSegment, "marshal_failed", err.Error(), 1)
	}
	b = append(b, '\n')
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", model.New(model.StageSegment, "write_failed", err.Error(), 1)
	}
	tmp := filepath.Join(dir, Name+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", model.New(model.StageSegment, "write_failed", err.Error(), 1)
	}
	if err := os.Rename(tmp, filepath.Join(dir, Name)); err != nil {
		return "", model.New(model.StageSegment, "write_failed", err.Error(), 1)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Load reads the manifest and its SHA-256 back from disk.
func Load(dir string) (*Manifest, string, *model.MediaError) {
	b, err := os.ReadFile(filepath.Join(dir, Name))
	if err != nil {
		return nil, "", model.New(model.StageVerify, "manifest_missing", err.Error(), 1)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, "", model.New(model.StageVerify, "manifest_corrupt", err.Error(), 1)
	}
	sum := sha256.Sum256(b)
	return &m, hex.EncodeToString(sum[:]), nil
}
