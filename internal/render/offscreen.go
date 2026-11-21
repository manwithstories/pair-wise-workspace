// Package render exports timeline slots as a reviewable, fully deterministic
// offscreen frame sequence plus a machine-readable JSON manifest. There is no
// GUI or image-codec dependency: each frame is a hashed text record.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediasync/internal/model"
	"mediasync/internal/timeline"
)

// FrameRecord is one exported frame entry in the manifest.
type FrameRecord struct {
	Index      int    `json:"index"`
	SharedTick int64  `json:"shared_tick"`
	TrackID    string `json:"track_id"`
	Rep        string `json:"rep"`
	Seq        int64  `json:"seq"`
	Incomplete bool   `json:"incomplete"`
	Recovered  bool   `json:"recovered"`
	FrameFile  string `json:"frame_file"`
	SHA256     string `json:"sha256"`
	Conclusion string `json:"conclusion"`
}

// Manifest is the machine-readable export summary.
type Manifest struct {
	Version     int                         `json:"version"`
	ClockRate   int64                       `json:"clock_rate"`
	FrameCount  int                         `json:"frame_count"`
	HashCount   int                         `json:"frame_hash_count"`
	Frames      []FrameRecord               `json:"frames"`
	Diagnostics []model.Diagnostic          `json:"diagnostics"`
	Mismatches  []timeline.BoundaryMismatch `json:"boundary_mismatches"`
	Incomplete  []string                    `json:"incomplete_tracks"`
	Checksum    string                      `json:"manifest_checksum"`
}

// Exporter writes frames and the manifest inside one validated directory.
type Exporter struct {
	root      string
	framesDir string
}

// NewExporter validates the output root and rejects path traversal: every path
// written later must remain inside the resolved root.
func NewExporter(root string) (*Exporter, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("output directory must not be empty")
	}
	// Lexical check before Clean collapses ".." away and hides the escape attempt.
	for _, part := range strings.FieldsFunc(filepath.ToSlash(root), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return nil, fmt.Errorf("output path %q contains a parent-directory segment", root)
		}
	}
	clean := filepath.Clean(root)
	abs, err := filepath.Abs(clean)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}
	frames := filepath.Join(abs, "frames")
	if err := os.MkdirAll(frames, 0o755); err != nil {
		return nil, err
	}
	return &Exporter{root: abs, framesDir: frames}, nil
}

// Root returns the resolved, absolute output directory.
func (e *Exporter) Root() string { return e.root }

func (e *Exporter) inside(path string) error {
	rel, err := filepath.Rel(e.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path %q escapes output root %q", path, e.root)
	}
	return nil
}

// Export renders every timeline slot. The same timeline always produces the
// same hashes: hashing covers canonical frame fields, never wall-clock data.
func (e *Exporter) Export(tl *timeline.Timeline, mismatches []timeline.BoundaryMismatch) (*Manifest, error) {
	manifest := &Manifest{Version: 1, ClockRate: tl.Clock.TickRate,
		Diagnostics: tl.Diag.All(), Mismatches: mismatches, Incomplete: tl.IncompleteTracks()}
	index := 0
	for _, slot := range tl.Slots {
		for _, af := range slot.Frames {
			rec, err := e.writeFrame(index, slot.Tick, af)
			if err != nil {
				return nil, err
			}
			manifest.Frames = append(manifest.Frames, rec)
			manifest.FrameCount++
			if rec.SHA256 != "" {
				manifest.HashCount++
			}
			index++
		}
	}
	manifest.Diagnostics = sortedDiagnostics(manifest.Diagnostics)
	if err := e.writeManifest(manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func (e *Exporter) writeFrame(index int, tick int64, af timeline.AlignedFrame) (FrameRecord, error) {
	name := fmt.Sprintf("frame_%06d_%s_%d.txt", index, sanitize(af.TrackID), af.Frame.Seq)
	path := filepath.Join(e.framesDir, name)
	if err := e.inside(path); err != nil {
		return FrameRecord{}, err
	}
	fr := af.Frame
	conclusion := "ok"
	switch {
	case fr.Incomplete:
		conclusion = "incomplete_gap_placeholder"
	case fr.Recovered:
		conclusion = "recovered_within_window"
	}
	// Canonical, stable record: no timestamps of the host clock, no map iteration.
	canonical := fmt.Sprintf("%d|%d|%s|%s|%d|%t|%t|%x",
		index, tick, af.TrackID, fr.Rep, fr.Seq, fr.Incomplete, fr.Recovered, fr.Payload)
	sum := sha256.Sum256([]byte(canonical))
	hash := hex.EncodeToString(sum[:])
	body := canonical + "\nsha256=" + hash + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return FrameRecord{}, err
	}
	return FrameRecord{Index: index, SharedTick: tick, TrackID: af.TrackID, Rep: fr.Rep,
		Seq: fr.Seq, Incomplete: fr.Incomplete, Recovered: fr.Recovered,
		FrameFile: filepath.ToSlash(filepath.Join("frames", name)), SHA256: hash,
		Conclusion: conclusion}, nil
}

func (e *Exporter) writeManifest(m *Manifest) error {
	// Stable JSON: frame list is already ordered; sort mismatch/diagnostic slices.
	sort.Slice(m.Mismatches, func(i, j int) bool {
		if m.Mismatches[i].AtTick != m.Mismatches[j].AtTick {
			return m.Mismatches[i].AtTick < m.Mismatches[j].AtTick
		}
		return m.Mismatches[i].TrackID < m.Mismatches[j].TrackID
	})
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	m.Checksum = hex.EncodeToString(sum[:])
	// Re-marshal with the checksum populated; checksum covers the body without it.
	data, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(e.root, "manifest.json")
	if err := e.inside(path); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func sanitize(name string) string {
	r := strings.NewReplacer("..", "_", string(os.PathSeparator), "_", "/", "_", "\\", "_")
	out := r.Replace(name)
	if out == "" || out == "." {
		return "track"
	}
	return out
}

func sortedDiagnostics(in []model.Diagnostic) []model.Diagnostic {
	out := append([]model.Diagnostic(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].AtTicks != out[j].AtTicks {
			return out[i].AtTicks < out[j].AtTicks
		}
		return out[i].Track < out[j].Track
	})
	return out
}
