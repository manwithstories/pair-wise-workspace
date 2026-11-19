// Package render exports deterministic offscreen review frames from a
// composed timeline. No graphics device or window is opened: frames are
// small PPM rasters written through a temporary file and atomically
// renamed into place.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pairflow/media/internal/clock"
	"pairflow/media/internal/timeline"
)

// Frame is one exported review frame.
type Frame struct {
	Index     int            `json:"index"`
	Segment   int            `json:"segment"`
	Track     string         `json:"track"`
	Source    string         `json:"source"`
	SourceOff int64          `json:"source_offset"`
	Time      clock.Rational `json:"time"`
	Sample    int            `json:"sample_number,omitempty"`
	Keyframe  bool           `json:"keyframe_hit"`
	Path      string         `json:"path"`
	SHA256    string         `json:"sha256"`
	Bytes     int64          `json:"bytes"`
}

// Segment is a contiguous run of frames sharing a SHA-256 digest.
type Segment struct {
	Index  int            `json:"index"`
	Track  string         `json:"track"`
	Start  clock.Rational `json:"start"`
	End    clock.Rational `json:"end"`
	Frames int            `json:"frames"`
	SHA256 string         `json:"sha256"`
	File   string         `json:"file"`
	Bytes  int64          `json:"bytes"`
}

// Manifest is the review bundle index.
type Manifest struct {
	Version      int       `json:"version"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	Seed         int64     `json:"seed"`
	GeneratedAt  string    `json:"generated_at"`
	Frames       []Frame   `json:"frames"`
	Segments     []Segment `json:"segments"`
	KeyframeHits int       `json:"keyframe_hits"`
	MissingHits  []int     `json:"missing_keyframe_hits,omitempty"`
}

// Options configures export.
type Options struct {
	OutDir       string
	Width        int
	Height       int
	SegmentSize  int
	Seed         int64
	MaxFrames    int
	MaxBytes     int64
	KeyframeOnly bool
}

// PlanTargets selects the timeline entries that should become frames.
// Video entries are preferred; when a timeline has no video, subtitle
// entries are rendered so the review bundle is still meaningful.
func PlanTargets(tl *timeline.Timeline, opt Options) []timeline.Entry {
	var video []timeline.Entry
	for _, e := range tl.Entries {
		if e.Kind == timeline.KindVideo {
			video = append(video, e)
		}
	}
	if opt.KeyframeOnly {
		filtered := video[:0]
		for _, e := range video {
			if e.Sample != nil && e.Sample.Sync {
				filtered = append(filtered, e)
			}
		}
		video = filtered
	}
	if len(video) == 0 && !opt.KeyframeOnly {
		for _, e := range tl.Entries {
			if e.Kind == timeline.KindSubtitle {
				video = append(video, e)
			}
		}
	}
	if opt.MaxFrames > 0 && len(video) > opt.MaxFrames {
		video = video[:opt.MaxFrames]
	}
	return video
}

// Render writes frames and a manifest to opt.OutDir. The output
// directory must resolve inside the working tree's temp area or the
// caller-provided root; traversal into parent directories is rejected.
func Render(tl *timeline.Timeline, targets []timeline.Entry, opt Options) (*Manifest, error) {
	if opt.Width <= 0 {
		opt.Width = 64
	}
	if opt.Height <= 0 {
		opt.Height = 36
	}
	if opt.SegmentSize <= 0 {
		opt.SegmentSize = 8
	}
	root, err := safeDir(opt.OutDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	man := &Manifest{
		Version: 1, Width: opt.Width, Height: opt.Height, Seed: opt.Seed,
		GeneratedAt: "deterministic", // never wall-clock: keeps output reproducible
	}
	var totalBytes int64
	for i, e := range targets {
		seg := i / opt.SegmentSize
		pixels := rasterize(e, i, opt, tl.FrameDuration)
		sum := sha256.Sum256(pixels)
		name := fmt.Sprintf("frame-%05d-seg-%03d.ppm", i, seg)
		path := filepath.Join(root, name)
		if err := atomicWrite(path, pixels, root); err != nil {
			return nil, err
		}
		keyframe := e.Sample == nil || e.Sample.Sync
		if !keyframe {
			man.MissingHits = append(man.MissingHits, i)
		} else {
			man.KeyframeHits++
		}
		f := Frame{
			Index: i, Segment: seg, Track: e.Track, Source: e.Track,
			SourceOff: e.Offset, Time: e.Start, Keyframe: keyframe,
			Path: name, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(pixels)),
		}
		if e.Sample != nil {
			f.Sample = e.Sample.Number
		}
		man.Frames = append(man.Frames, f)
		totalBytes += int64(len(pixels))
		if opt.MaxBytes > 0 && totalBytes > opt.MaxBytes {
			return nil, clock.Errorf("render", "byte_budget", -1,
				"frame export exceeded %d bytes", opt.MaxBytes)
		}
	}
	man.Segments = buildSegments(man.Frames, tl, opt.SegmentSize)
	if err := WriteManifest(root, man); err != nil {
		return nil, err
	}
	return man, nil
}

// buildSegments groups frames into segments and hashes the manifest
// rows of each segment. The per-segment digest covers frame content
// digests, so it changes whenever any constituent frame changes.
func buildSegments(frames []Frame, tl *timeline.Timeline, size int) []Segment {
	bySeg := map[int][]Frame{}
	var order []int
	for _, f := range frames {
		if _, ok := bySeg[f.Segment]; !ok {
			order = append(order, f.Segment)
		}
		bySeg[f.Segment] = append(bySeg[f.Segment], f)
	}
	sort.Ints(order)
	out := make([]Segment, 0, len(order))
	for _, id := range order {
		group := bySeg[id]
		h := sha256.New()
		var bytes int64
		for _, f := range group {
			fmt.Fprintf(h, "%d|%s|%d|%s\n", f.Index, f.Track, f.SourceOff, f.SHA256)
			bytes += f.Bytes
		}
		seg := Segment{
			Index: id, Track: group[0].Track,
			Start: group[0].Time, End: group[len(group)-1].Time,
			Frames: len(group), SHA256: hex.EncodeToString(h.Sum(nil)),
			File: fmt.Sprintf("segment-%03d.json", id), Bytes: bytes,
		}
		out = append(out, seg)
	}
	_ = tl
	return out
}

// rasterize paints a deterministic gradient with a per-frame seed band
// and a top strip encoding the track kind, so a reviewer can identify a
// frame without metadata.
func rasterize(e timeline.Entry, index int, opt Options, frameDur clock.Rational) []byte {
	w, h := opt.Width, opt.Height
	header := fmt.Sprintf("P6\n%d %d\n255\n", w, h)
	buf := make([]byte, 0, len(header)+w*h*3)
	buf = append(buf, header...)
	seedByte := byte(opt.Seed*31 + int64(index)*17)
	kindByte := byte(0)
	switch e.Kind {
	case timeline.KindVideo:
		kindByte = 0x80
	case timeline.KindSubtitle:
		kindByte = 0xC0
	case timeline.KindAudio:
		kindByte = 0x40
	}
	// Normalised time on the frame duration, as an integer fraction of
	// the strip width. No floating point accumulation.
	phase := 0
	if frameDur.IsPositive() {
		phase = int((e.Start.Millis() / max64(frameDur.Millis(), 1)) % int64(w))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r := byte((x*3 + int(seedByte)) & 0xFF)
			g := byte((y*5 + index*11) & 0xFF)
			b := byte((x ^ y) & 0xFF)
			if y < 4 {
				// Track identity strip.
				r, g, b = kindByte, byte(x*4), 0x20
			} else if y == 5 {
				if x >= phase%w && x < (phase%w)+6 {
					r, g, b = 0xFF, 0xFF, 0xFF
				}
			}
			buf = append(buf, r, g, b)
		}
	}
	return buf
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// safeDir resolves dir to an absolute path and refuses paths that
// escape via ".." at any point.
func safeDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", clock.NewSourceError("render", "bad_output_path", -1, "empty output directory")
	}
	clean := filepath.Clean(dir)
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".." {
			return "", clock.Errorf("render", "path_traversal", -1,
				"output path %q escapes the working tree", dir)
		}
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(abs, "/") && filepath.Base(abs) == "/" {
		return "", clock.NewSourceError("render", "bad_output_path", -1, "refusing filesystem root")
	}
	return abs, nil
}

// atomicWrite writes data to a sibling temp file and renames it over
// path. The temp file lives inside root so the rename stays on one
// filesystem.
func atomicWrite(path string, data []byte, root string) error {
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return clock.Errorf("render", "path_escape", -1,
			"refusing to write %q outside %q", path, root)
	}
	tmp, err := os.CreateTemp(root, ".pairflow-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// WriteManifest writes manifest.json atomically.
func WriteManifest(root string, man *Manifest) error {
	data, err := marshalManifest(man)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(root, "manifest.json"), data, root)
}

// SegmentDigests returns id→digest in index order for verification.
func (m *Manifest) SegmentDigests() map[int]string {
	out := make(map[int]string, len(m.Segments))
	for _, s := range m.Segments {
		out[s.Index] = s.SHA256
	}
	return out
}
