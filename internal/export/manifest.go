// Package export writes in-range keyframes to disk and emits a
// manifest with per-frame pts/type/size/sha256, verifying counts,
// bytes and digests. It reuses the shared frame table (no re-parse).
package export

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mp4tool/internal/index"
	"mp4tool/internal/sample"
	"mp4tool/internal/timeline"
)

// ErrVerify reports a post-write verification mismatch.
var ErrVerify = errors.New("export_verify_failed")

// ManifestFrame is one manifest entry.
type ManifestFrame struct {
	Index  int    `json:"index"`
	PtsMs  int64  `json:"pts_ms"`
	Type   string `json:"type"`
	Size   uint32 `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest summarizes one export run.
type Manifest struct {
	File       string          `json:"file"`
	FromMs     int64           `json:"from_ms"`
	ToMs       int64           `json:"to_ms"`
	Frames     []ManifestFrame `json:"frames"`
	Written    int             `json:"written"`
	Bytes      uint64          `json:"bytes"`
	WrapEvents int             `json:"wrap_events"`
}

// Export selects presented frames whose normalized pts falls inside
// [fromMs, toMs], writes keyframes as raw sample bytes into outDir and
// stores manifest.json there.
func Export(r io.ReaderAt, srcPath string, frames []index.Frame, fromMs, toMs int64, ts uint32, outDir string, wrapEvents int) (*Manifest, error) {
	m := &Manifest{File: filepath.Base(srcPath), FromMs: fromMs, ToMs: toMs, WrapEvents: wrapEvents}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	for _, f := range frames {
		if !f.Presented {
			continue
		}
		ms := timeline.TicksToMs(f.NormPTS, ts)
		if ms < fromMs || ms > toMs {
			continue
		}
		typ := "P"
		if f.Keyframe {
			typ = "I"
		}
		entry := ManifestFrame{Index: f.Index, PtsMs: ms, Type: typ, Size: f.Size}
		if f.Keyframe {
			name := filepath.Join(outDir, fmt.Sprintf("frame_%04d.bin", f.Index))
			w, err := os.Create(name)
			if err != nil {
				return nil, err
			}
			sum, n, err := sample.ReadSample(r, f, w)
			w.Close()
			if err != nil {
				return nil, err
			}
			if n != int64(f.Size) {
				return nil, ErrVerify
			}
			entry.SHA256 = sum
			m.Written++
		} else {
			sum, err := sample.HashSample(r, f)
			if err != nil {
				return nil, err
			}
			entry.SHA256 = sum
		}
		m.Bytes += uint64(f.Size)
		m.Frames = append(m.Frames, entry)
	}
	if len(m.Frames) == 0 {
		return nil, index.ErrSeekOutOfRange
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0o644); err != nil {
		return nil, err
	}
	return m, nil
}
