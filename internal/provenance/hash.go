// Package provenance computes per-segment hashes and source traceability lists.
package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"

	"bcast/internal/model"
)

// FrameHash binds one exported/reviewed frame to its source interval.
type FrameHash struct {
	Index       int                   `json:"index"`
	PTS         model.Rational        `json:"pts"`
	SHA256      string                `json:"sha256"`
	SourceAsset string                `json:"source_asset,omitempty"`
	SourceFrame int64                 `json:"source_frame,omitempty"`
	SourceRange *model.SourceInterval `json:"source_range,omitempty"`
}

// SegmentManifest is the evidence record for one output segment.
type SegmentManifest struct {
	Version       string                 `json:"version"`
	Segment       int                    `json:"segment"`
	Name          string                 `json:"name"`
	Mode          string                 `json:"mode"`
	InputSHA256   map[string]string      `json:"input_sha256"`
	TimelineSHA   string                 `json:"timeline_sha256"`
	ParamsSHA     string                 `json:"params_sha256"`
	Sources       []model.SourceInterval `json:"sources"`
	Frames        []FrameHash            `json:"frames"`
	OutputFiles   map[string]string      `json:"output_files"`
	FrameHashRoot string                 `json:"frame_hash_root"`
	SHA256        string                 `json:"segment_sha256"`
}

// HashBytes returns the hex SHA-256 of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashCanonical hashes the deterministic JSON encoding of v.
func HashCanonical(v any) (string, []byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	return HashBytes(b), b, nil
}

// HashReader consumes r through SHA-256 without reading it all into memory.
func HashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// HashFile streams a file and returns its SHA-256.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return HashReader(f)
}

// FrameRoot hashes the ordered concatenation of frame hashes. Order is part of
// the evidence, so changing frame order changes the root.
func FrameRoot(frames []FrameHash) string {
	h := sha256.New()
	for _, f := range frames {
		_, _ = io.WriteString(h, f.SHA256)
		_, _ = io.WriteString(h, "|")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeHash calculates and stores the segment's canonical hash. The hash
// field itself is excluded by clearing it before marshalling.
func (m *SegmentManifest) ComputeHash() (string, error) {
	m.Version = "bcast.provenance/v1"
	m.FrameHashRoot = FrameRoot(m.Frames)
	m.SHA256 = ""
	sum, _, err := HashCanonical(m)
	if err != nil {
		return "", err
	}
	m.SHA256 = sum
	return sum, nil
}

// Verify recomputes the segment hash and reports whether it matches.
func (m SegmentManifest) Verify() (bool, error) {
	want := m.SHA256
	got, err := m.ComputeHash()
	if err != nil {
		return false, err
	}
	return got == want, nil
}
