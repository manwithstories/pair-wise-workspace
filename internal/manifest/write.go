// Package manifest writes and verifies the deterministic clip package index.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"fieldcut/internal/clock"
	"fieldcut/internal/events"
	"fieldcut/internal/fingerprint"
	"fieldcut/internal/graph"
	"fieldcut/internal/recover"
)

// Segment is one delivered clip fragment.
type Segment struct {
	Index          int                 `json:"index"`
	File           string              `json:"file"`
	AudioFile      string              `json:"audio_file,omitempty"`
	Interval       clock.Interval      `json:"interval_ns"`
	SHA256         string              `json:"sha256"`
	AudioSHA256    string              `json:"audio_sha256,omitempty"`
	SizeBytes      int64               `json:"size_bytes"`
	RandomAccess   bool                `json:"random_access"`
	BoundarySkewNs clock.Ns            `json:"boundary_skew_ns"`
	EventIDs       []string            `json:"event_ids,omitempty"`
	Assignments    []events.Assignment `json:"assignments,omitempty"`
}

// Metrics contains the terminal acceptance fields.
type Metrics struct {
	TargetBPS         int      `json:"target_bps"`
	ActualBPS         float64  `json:"actual_bps"`
	BitrateDeltaPct   float64  `json:"bitrate_delta_pct"`
	RMSDBFS           float64  `json:"rms_dbfs"`
	PeakDBFS          float64  `json:"peak_dbfs"`
	ClippedSamples    int      `json:"clipped_samples"`
	SegmentCount      int      `json:"segment_count"`
	DedupeCount       int      `json:"dedupe_count"`
	ManifestMaxSkewNs clock.Ns `json:"manifest_max_skew_ns"`
	ReusedNodes       int      `json:"reused_nodes,omitempty"`
	OutputSHA256      string   `json:"output_sha256,omitempty"`
	ErrorCode         string   `json:"error_code,omitempty"`
	TrackID           int      `json:"track_id,omitempty"`
	IntervalNs        clock.Ns `json:"interval_ns,omitempty"`
}

// Manifest is the complete reviewable package description.
type Manifest struct {
	Version         int                  `json:"version"`
	Case            string               `json:"case"`
	Seed            int64                `json:"seed"`
	OutputDir       string               `json:"-"`
	Metrics         Metrics              `json:"metrics"`
	Segments        []Segment            `json:"segments"`
	Events          []events.Event       `json:"events,omitempty"`
	Assignments     []events.Assignment  `json:"assignments,omitempty"`
	Duplicates      []fingerprint.Pair   `json:"duplicates,omitempty"`
	ColorDecision   *graph.ColorDecision `json:"color_decision,omitempty"`
	Errors          []*clock.Error       `json:"errors,omitempty"`
	FirstError      *clock.Error         `json:"first_error,omitempty"`
	CutPointErrorNs []clock.Ns           `json:"cut_point_error_ns,omitempty"`
}

// Write atomically writes manifest.json.
func Write(outDir string, m *Manifest) (string, error) {
	if m.Version == 0 {
		m.Version = 1
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	path := filepath.Join(outDir, "manifest.json")
	if err := recover.AtomicWriteFile(path, b); err != nil {
		return "", err
	}
	return path, nil
}

// Read loads and parses a manifest.
func Read(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Verify checks segment hashes, bitrate tolerance and alignment skew.
func Verify(outDir string) (*Manifest, error) {
	m, err := Read(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	for _, seg := range m.Segments {
		if err := verifyHash(filepath.Join(outDir, seg.File), seg.SHA256); err != nil {
			return m, fmt.Errorf("segment %d: %w", seg.Index, err)
		}
		if seg.AudioFile != "" {
			if err := verifyHash(filepath.Join(outDir, seg.AudioFile), seg.AudioSHA256); err != nil {
				return m, fmt.Errorf("segment %d audio: %w", seg.Index, err)
			}
		}
	}
	if m.Metrics.BitrateDeltaPct > 5 || m.Metrics.BitrateDeltaPct < -5 {
		return m, fmt.Errorf("bitrate delta %.3f%% exceeds 5%%", m.Metrics.BitrateDeltaPct)
	}
	if m.Metrics.ManifestMaxSkewNs > clock.NsPerMs {
		return m, fmt.Errorf("max skew %d ns exceeds %d", m.Metrics.ManifestMaxSkewNs, clock.NsPerMs)
	}
	return m, nil
}

func verifyHash(path, want string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("hash mismatch: got %s want %s", got, want)
	}
	return nil
}

// HashOutputs hashes the deterministic delivered artifact files. It excludes
// journals, reports and temporary files so recovery status does not change the
// content identity.
func HashOutputs(outDir string) (string, error) {
	names := make([]string, 0)
	err := filepath.WalkDir(outDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(outDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "manifest.json" || rel == "metrics.txt" || rel == "journal.json" ||
			rel == "damage.json" || rel == "request.json" || filepath.Ext(rel) == ".tmp" {
			return nil
		}
		names = append(names, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, rel := range names {
		b, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		var lenBuf [8]byte
		fmt.Fprintf(h, "%s\n", rel)
		for i := 0; i < 8; i++ {
			lenBuf[i] = byte(uint64(len(b)) >> (56 - 8*i))
		}
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
