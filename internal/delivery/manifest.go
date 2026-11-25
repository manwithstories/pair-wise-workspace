// Package delivery turns an index into fMP4 segments and a manifest. Segments
// start and end only on random-access points: a damaged stretch is never split
// across a segment boundary, and the first keyframe after a gap defines where
// usable content resumes. Gaps are recorded in the manifest with the timeline
// position they occupy, so an editor sees the hole rather than a silently
// shortened cut.
package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"cinepipe/internal/clock"
	"cinepipe/internal/index"
)

// ManifestMedia is one produced init segment or media segment file.
type ManifestMedia struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	ByteLength int64  `json:"byte_length"`
	StartTicks int64  `json:"start_ticks"`
	EndTicks   int64  `json:"end_ticks,omitempty"`
	SHA256     string `json:"sha256"`
}

// ManifestGap records a timeline interval a segment does not cover because the
// source was damaged there. Times are on the shared 90 kHz axis and relative to
// the timeline origin.
type ManifestGap struct {
	StartTicks  int64  `json:"start_ticks"`
	EndTicks    int64  `json:"end_ticks"`
	Kind       string `json:"kind"`
	StartByte  int64  `json:"start_byte"`
	RecoveredAt int64 `json:"recovered_at"`
}

// ManifestTrack preserves one track's description, including the per-segment
// descriptions that were deliberately kept separate because their parameters
// (channel layout, sample rate, colour) differ.
type ManifestTrack struct {
	Kind        string                    `json:"kind"`
	PID         uint16                    `json:"pid"`
	Codec       string                    `json:"codec"`
	SampleRate  int                       `json:"sample_rate,omitempty"`
	Channels    int                       `json:"channels,omitempty"`
	Layout      string                    `json:"layout,omitempty"`
	Language    string                    `json:"language,omitempty"`
	Color       *index.ColorInfo          `json:"color,omitempty"`
	InitSegment string                    `json:"init_segment,omitempty"`
	Segments    []ManifestMedia           `json:"segments,omitempty"`
	Descriptions []index.TrackDescription `json:"descriptions,omitempty"`
}

// Manifest is the rootsummary an editor consumes.
type Manifest struct {
	Format        string          `json:"format"`
	Timescale     int64           `json:"timescale"`
	TargetKBPS    int             `json:"target_kbps,omitempty"`
	ActualKBPS    float64         `json:"actual_kbps"`
	DurationTicks int64           `json:"duration_ticks"`
	Source    index.Source        `json:"source"`
	Tracks        []ManifestTrack `json:"tracks"`
	Gaps          []ManifestGap   `json:"gaps,omitempty"`
	// AlignmentMaxMS is the largest difference between a video and audio
	// segment start that are meant to be co-timed; 0 means every aligned pair
	// matched.
	AlignmentMaxMS float64 `json:"alignment_max_ms"`
	// OutputSHA256 is the SHA-256 over every produced file in name order.
	OutputSHA256 string `json:"output_sha256"`
}

// Options controls manifest generation.
type Options struct {
	// TargetKBPS is the intended media bitrate. The deliverer reports the
	// achieved rate so the demo can check the deviation.
	TargetKBPS int
	// SegmentTicks is the target segment duration in 90 kHz ticks. Segments
	// extend to the next keyframe at or after the target.
	SegmentTicks int64
	// OutDir is where segment and manifest files are written.
	OutDir string
}

// segment is one planned media segment in tick space.
type segment struct {
	startTicks int64
	endTicks   int64
	startByte  int64
	endByte    int64
	keyframeIndex int64
	frames     int
	bytesEst   int64
}

// planSegments lays segment boundaries on random-access points. The first
// segment starts at the first keyframe on or after tick 0; each subsequent
// segment starts at the first keyframe at or after target. A damaged range
// forces the next segment to begin at its recovery keyframe, so the hole is not
// paved over.
func planSegments(idx *index.Index, segmentTicks int64) []segment {
	video, ok := videoTrack(idx)
	if !ok || len(video.Keyframes) == 0 {
		return nil
	}
	var keys []index.Keyframe
	for _, k := range video.Keyframes {
		if k.Keyframe {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Ticks < keys[j].Ticks })

	// Recovery keyframe offsets that must start a segment.
	recoveries := map[int64]bool{}
	for _, r := range idx.Ranges {
		if r.RecoveredAt >= 0 {
			recoveries[r.RecoveredAt] = true
		}
	}

	var out []segment
	cur := 0
	curSeg := &segment{startTicks: keys[0].Ticks, startByte: keys[0].ByteOffset, keyframeIndex: keys[0].Index}
	for i := 1; i < len(keys); i++ {
		k := keys[i]
		forceNew := recoveries[k.ByteOffset]
		target := curSeg.startTicks + segmentTicks
		// Cut at the first keyframe that reaches the target duration, or at any
		// recovery keyframe that ends a damaged range.
		if forceNew || k.Ticks >= target {
			curSeg.endTicks = k.Ticks
			curSeg.endByte = k.ByteOffset
			curSeg.frames = i - cur
			out = append(out, *curSeg)
			cur = i
			curSeg = &segment{startTicks: k.Ticks, startByte: k.ByteOffset, keyframeIndex: k.Index}
		}
	}
	curSeg.endTicks = clock.NoTime
	curSeg.endByte = idx.Source.Bytes
	curSeg.frames = len(keys) - cur
	out = append(out, *curSeg)
	return out
}

// videoTrack returns the first video track and whether one exists.
func videoTrack(idx *index.Index) (index.Track, bool) {
	for _, t := range idx.Tracks {
		if t.Kind == index.TrackVideo {
			return t, true
		}
	}
	return index.Track{}, false
}

// gapsFromRanges converts damaged ranges into manifest gaps that overlap the
// planned segments, excluding ranges that fall entirely outside the covered
// timeline.
func gapsFromRanges(idx *index.Index, segs []segment) []ManifestGap {
	if len(segs) == 0 {
		return nil
	}
	start := segs[0].startTicks
	end := segs[len(segs)-1].endTicks
	var out []ManifestGap
	for _, r := range idx.Ranges {
		g := ManifestGap{
			StartTicks:  r.StartTicks,
			EndTicks:    r.EndTicks,
			Kind:        string(r.Kind),
			StartByte:   r.StartByte,
			RecoveredAt: r.RecoveredAt,
		}
		if r.EndTicks == clock.NoTime {
			g.EndTicks = end
		}
		if g.EndTicks < start || g.StartTicks > end {
			continue
		}
		out = append(out, g)
	}
	return out
}

// hashFiles returns a combined SHA-256 over the named files in the given order.
func hashFiles(dir string, names []string) (string, error) {
	h := sha256.New()
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeManifest serialises the manifest deterministically.
func writeManifest(dir string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, "manifest.json"))
}
