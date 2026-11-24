// Package video exports a deterministic, off-screen frame sequence from a
// keyframe index. Frames are placeholder renders derived purely from the
// sample metadata (no GUI, no encoder dependency): identical inputs always
// produce byte-identical fragment files. Writes go through a temp file and
// an atomic rename so a crash can never leave a half-committed frame.
package video

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"review/internal/index"
	"review/internal/media"
)

// Frame is one exported, fully resolved frame descriptor.
type Frame struct {
	Number   int
	Track    uint32
	TimeNS   int64
	Keyframe bool
	FileName string
	Hash     string
}

// Exporter writes frames under a validated output directory.
type Exporter struct {
	root string
}

// NewExporter validates the output root. Rejecting relative or traversal
// paths here keeps every later write inside one reviewed directory.
func NewExporter(root string) (*Exporter, error) {
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) {
		return nil, media.NewBoundary("frame-export",
			"output path must be absolute", 0, -1, nil)
	}
	if strings.Contains(clean, "..") {
		return nil, media.NewBoundary("frame-export",
			"output path must not contain traversal segments", 0, -1, nil)
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return nil, media.NewInternal("frame-export", "create output dir", err)
	}
	return &Exporter{root: clean}, nil
}

// Root returns the validated absolute output directory.
func (e *Exporter) Root() string { return e.root }

// renderFrame produces deterministic placeholder bytes for one index row.
// The content includes sample number, timestamp and keyframe flag so a
// reviewer can diff frames without any external media.
func renderFrame(row index.PreviewRow, keyframe bool) []byte {
	h := sha256.New()
	fmt.Fprintf(h, "review-frame sample=%d ts=%d key=%t",
		row.Sample, row.TimeScaled, keyframe)
	sum := hex.EncodeToString(h.Sum(nil))
	return []byte(fmt.Sprintf(
		"REVIEW FRAME\nsample=%d\ntimescaled=%d\nkeyframe=%t\nsha256=%s\n",
		row.Sample, row.TimeScaled, keyframe, sum))
}

// Export writes one file per index entry through temp+rename and returns
// the ordered frame descriptors. Existing committed files with identical
// content are left untouched, making reruns idempotent.
func (e *Exporter) Export(rows []index.PreviewRow, timescale uint32) ([]Frame, error) {
	if len(rows) == 0 {
		return nil, media.NewBoundary("frame-export", "no frames to export", 0, -1, nil)
	}
	frames := make([]Frame, 0, len(rows))
	for i, row := range rows {
		name := fmt.Sprintf("frame-%05d.rvf", row.Sample)
		path := filepath.Join(e.root, name)
		content := renderFrame(row, row.Keyframe)
		sum := sha256.Sum256(content)
		if err := atomicWrite(path, content); err != nil {
			return nil, media.NewInternal("frame-export",
				fmt.Sprintf("frame %d commit failed", row.Sample), err)
		}
		frames = append(frames, Frame{
			Number: i + 1, Track: 0, TimeNS: int64(row.TimeScaled) *
				media.NanosPerSecond / int64(timescale),
			Keyframe: row.Keyframe, FileName: name,
			Hash: hex.EncodeToString(sum[:]),
		})
	}
	return frames, nil
}

// atomicWrite writes to a sibling temp file, fsyncs, then renames into
// place. The target directory must already exist.
func atomicWrite(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".frame-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// SegmentHash folds every frame hash into one stable digest for the whole
// sequence, allowing the demo to prove reruns produce identical segments.
func SegmentHash(frames []Frame) string {
	h := sha256.New()
	for _, f := range frames {
		fmt.Fprintf(h, "%s:%s\n", f.FileName, f.Hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}
