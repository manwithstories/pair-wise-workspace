// Package report emits the JSON delivery manifest and a small MP4
// sidecar (via github.com/abema/go-mp4) that pins the manifest digest,
// so every artifact is traceable to its inputs and parameters.
package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abema/go-mp4"
)

// TrackMap records one track of the unified timeline.
type TrackMap struct {
	ID   uint64 `json:"id"`
	Kind string `json:"kind"`
}

// FrameRef pins one exported frame.
type FrameRef struct {
	Index  int    `json:"index"`
	TimeNs int64  `json:"time_ns"`
	SHA256 string `json:"sha256"`
}

// Interval is a committed checkpoint range in milliseconds.
type Interval struct {
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`
}

// ErrorInterval records an unrecoverable time range.
type ErrorInterval struct {
	Track   uint64 `json:"track"`
	StartNs int64  `json:"start_ns"`
	EndNs   int64  `json:"end_ns"`
	Reason  string `json:"reason"`
}

// Manifest is the deterministic delivery record. It contains no
// wall-clock fields, so repeated runs produce identical bytes.
type Manifest struct {
	SourceSHA256    string          `json:"source_sha256"`
	Tracks          []TrackMap      `json:"tracks"`
	ParamsSHA256    string          `json:"params_sha256"`
	Checkpoints     []Interval      `json:"checkpoints"`
	Frames          []FrameRef      `json:"frames"`
	TargetBps       int64           `json:"target_bps"`
	ActualBps       int64           `json:"actual_bps"`
	BitrateDeltaPct float64         `json:"bitrate_delta_pct"`
	QualityPSNRdB   float64         `json:"quality_psnr_db"`
	AlignmentMs     float64         `json:"manifest_alignment_ms"`
	Errors          []ErrorInterval `json:"errors,omitempty"`
}

// Digest returns the canonical SHA-256 of the manifest JSON.
func (m *Manifest) Digest() (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// CheckPathInside verifies that name resolves inside dir before any
// write happens.
func CheckPathInside(dir, name string) (string, error) {
	clean := filepath.Clean(dir)
	p := filepath.Join(clean, name)
	if p != clean && !strings.HasPrefix(p, clean+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes output directory %q", name, dir)
	}
	return p, nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Write atomically persists manifest.json and the index.mp4 sidecar
// inside dir.
func Write(dir string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	digest, err := m.Digest()
	if err != nil {
		return err
	}
	mp, err := CheckPathInside(dir, "manifest.json")
	if err != nil {
		return err
	}
	sp, err := CheckPathInside(dir, "index.mp4")
	if err != nil {
		return err
	}
	tmp := sp + ".tmp"
	if err := buildSidecar(tmp, digest); err != nil {
		return err
	}
	if err := os.Rename(tmp, sp); err != nil {
		return err
	}
	return writeAtomic(mp, data)
}

// Load reads manifest.json from dir.
func Load(dir string) (*Manifest, error) {
	p, err := CheckPathInside(dir, "manifest.json")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// buildSidecar writes an MP4 file whose free box carries the digest.
func buildSidecar(path, digest string) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	w := mp4.NewWriter(f)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 0,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		},
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFree()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Free{Data: []byte(digest)}, mp4.Context{}); err != nil {
		return err
	}
	_, err = w.EndBox()
	return err
}

// VerifySidecar checks that dir/index.mp4 pins the current manifest
// digest.
func VerifySidecar(dir string, m *Manifest) error {
	sp, err := CheckPathInside(dir, "index.mp4")
	if err != nil {
		return err
	}
	data, err := os.ReadFile(sp)
	if err != nil {
		return err
	}
	want, err := m.Digest()
	if err != nil {
		return err
	}
	var got string
	_, err = mp4.ReadBoxStructure(bytes.NewReader(data), func(h *mp4.ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == mp4.BoxTypeFree() {
			payload, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			if free, ok := payload.(*mp4.Free); ok {
				got = string(free.Data)
			}
		}
		return nil, nil
	})
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("sidecar digest %q does not match manifest %q", got, want)
	}
	return nil
}
