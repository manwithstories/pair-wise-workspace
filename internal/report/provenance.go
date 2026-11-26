// Package report builds deterministic provenance-rich JSON reports.
package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"

	"mediascan/internal/analyze"
	"mediascan/internal/media"
)

// SchemaVersion is the stable report schema identifier.
const SchemaVersion = "mediascan-report/v1"

// Parameters are run parameters recorded for provenance.
type Parameters struct {
	Workers int   `json:"workers"`
	Seed    int64 `json:"seed"`
}

// Input is one source asset and its idempotent task/checkpoint identity.
type Input struct {
	Index          int    `json:"index"`
	File           string `json:"file"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
	TaskKey        string `json:"task_key"`
	CheckpointPath string `json:"checkpoint_path,omitempty"`
	State          string `json:"state"`
}

// TrackReport joins stream structure with keyframe analysis.
type TrackReport struct {
	analyze.Stream
	Keyframes []analyze.Keyframe         `json:"keyframes"`
	Intervals []analyze.KeyframeInterval `json:"keyframe_intervals"`
}

// Metrics contains deterministic acceptance counters. It deliberately has no
// wall-clock duration fields.
type Metrics struct {
	Files                     int   `json:"files"`
	Tracks                    int   `json:"tracks"`
	Faults                    int   `json:"faults"`
	CorruptRegions            int   `json:"corrupt_regions"`
	UnknownBoxes              int   `json:"unknown_boxes"`
	ZeroTimescaleCount        int   `json:"zero_timescale_count"`
	OffsetErrorMaxBytes       int64 `json:"offset_error_max_bytes"`
	KeyframeTimeErrorMaxTicks int64 `json:"keyframe_time_error_max_ticks"`
	ResumeSkipped             int   `json:"resume_skipped"`
}

// Timing carries non-deterministic elapsed time. It is never serialized and
// therefore cannot affect the canonical report hash.
type Timing struct {
	ResumeMS int64 `json:"-"`
	ScanMS   int64 `json:"-"`
}

// Report is the canonical mediascan document.
type Report struct {
	SchemaVersion   string        `json:"schema_version"`
	AnalyzerVersion string        `json:"analyzer_version"`
	Parameters      Parameters    `json:"parameters"`
	Inputs          []Input       `json:"inputs"`
	Tracks          []TrackReport `json:"tracks"`
	Faults          []media.Fault `json:"faults"`
	Metrics         Metrics       `json:"metrics"`
	Timing          Timing        `json:"-"`
}

// CanonicalJSON encodes v with deterministic struct order, no HTML escaping,
// two-space indentation, and one trailing newline.
func CanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Hash returns the lowercase SHA-256 of data.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// CanonicalHash returns the hash of the canonical report bytes. Timing is
// excluded by its json:"-" tag.
func CanonicalHash(r *Report) ([]byte, string, error) {
	data, err := CanonicalJSON(r)
	if err != nil {
		return nil, "", err
	}
	return data, Hash(data), nil
}

// WriteCanonical writes report.json atomically-ish and returns its file hash.
// The returned hash is exactly what `shasum -a 256 report.json` prints.
func WriteCanonical(path string, r *Report) (string, error) {
	data, hash, err := CanonicalHash(r)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return hash, nil
}

// ReadHash recomputes the SHA-256 of an existing file.
func ReadHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return Hash(data), nil
}
