// Package stream executes the aligned cutting plan under byte/sample memory
// budgets, emits per-segment checksums, persists atomic checkpoints and
// writes a deterministic delivery manifest.
package stream

import (
	"encoding/json"
	"os"
	"path/filepath"

	"mediaslice/media"
	"mediaslice/scan"
)

// ManifestVersion identifies the output manifest schema.
const ManifestVersion = "mediaslice-manifest/v1"

// SegmentEntry describes one emitted segment of one representation.
type SegmentEntry struct {
	RepID       string      `json:"rep_id"`
	Sequence    int         `json:"sequence"`
	FileName    string      `json:"file"`
	FromTick    media.Ticks `json:"from_tick"`
	ToTick      media.Ticks `json:"to_tick"`
	Bytes       int64       `json:"bytes"`
	SHA256      string      `json:"sha256"`
	SampleCount int         `json:"sample_count"`
}

// Manifest is the deterministic per-run segment list.
type Manifest struct {
	Version         string         `json:"version"`
	ParserVersion   string         `json:"parser_version"`
	SegmentTicks    media.Ticks    `json:"segment_ticks"`
	Representations []string       `json:"representations"`
	Segments        []SegmentEntry `json:"segments"`
	// SegmentGroups[sequence][repID] gives symmetric cross-rep lookup.
	SegmentCount int `json:"segment_count"`
	// InputHashes binds every output to its exact source bytes.
	InputHashes map[string]string `json:"input_hashes"`
}

// WriteManifest atomically serializes the manifest under workDir.
func WriteManifest(workDir string, m *Manifest) (string, error) {
	path, err := scan.SafeJoin(workDir, "manifest.json")
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", media.NewError(media.KindIO, path, err.Error())
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", media.NewError(media.KindIO, path, err.Error())
	}
	return path, nil
}

// ReadManifest loads a manifest for resume verification.
func ReadManifest(workDir string) (*Manifest, error) {
	path, err := scan.SafeJoin(workDir, "manifest.json")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, media.NewError(media.KindIO, path, err.Error())
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// segmentFileName is the stable, traversal-safe segment file name.
func segmentFileName(repID string, seq int) string {
	return filepath.Join("segments", repID, segmentBaseName(seq))
}

func segmentBaseName(seq int) string {
	return "seg-" + pad6(seq) + ".bin"
}

func pad6(n int) string {
	digits := []byte("000000")
	v := n
	for i := len(digits) - 1; i >= 0; i-- {
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return string(digits)
}
