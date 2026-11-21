// Package timeline composes normalized metadata tracks into a deterministic
// output timeline. It fixes the rules for overlap order, holes (no coverage),
// and boundary frames (a source sample that only partially covers an output
// frame), and it converts transport gaps into explicit frame blocking.
package timeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"mediad/internal/clock"
	"mediad/internal/metadata"
	"mediad/internal/model"
)

// Config controls frame-grid construction.
type Config struct {
	// Timebase is the timeline's rational tick duration. It is normally
	// 1/90000 so both 29.97 and 48 kHz sources convert exactly.
	Timebase clock.Timebase
	// FPSNum/FPSDen define the output frame cadence.
	FPSNum, FPSDen int64
	// DurationTicks bounds the timeline; zero derives it from the tracks.
	DurationTicks int64
	// HoleBlockThresholdMs blocks a frame when a coverage hole spans at least
	// this many milliseconds. Zero disables hole blocking.
	HoleBlockThresholdMs int64
	// Scale divides the canvas and layer rectangles for verification renders.
	// Geometry rules are unchanged; only the raster size is reduced.
	Scale int
}

// DefaultConfig is a 90 kHz timeline at 30 fps.
func DefaultConfig() Config {
	return Config{
		Timebase:             clock.Video90k,
		FPSNum:               30,
		FPSDen:               1,
		HoleBlockThresholdMs: 50,
		Scale:                1,
	}
}

// Layer is one source sample placed into an output frame.
type Layer struct {
	TrackKey    string         `json:"track_key"`
	SourceIndex int64          `json:"source_index"`
	SourceStart int64          `json:"source_start"` // timeline ticks
	SourceEnd   int64          `json:"source_end"`   // timeline ticks
	Rotation    int            `json:"rotation"`
	Crop        *metadata.Crop `json:"crop,omitempty"`
	Color       metadata.Color `json:"color"`
	X, Y, W, H  int            `json:"-"`
	Rect        [4]int         `json:"rect"`
	// CodedW/CodedH are the pre-crop source dimensions, needed to sample the
	// source independently of the forward compositor.
	CodedW int `json:"coded_w"`
	CodedH int `json:"coded_h"`
	// Boundary marks a source sample that covers less than the full output
	// frame; the deterministic rule keeps it because it covers the start.
	Boundary bool `json:"boundary"`
	// PartialTicks is how much of the output frame the source sample covers.
	PartialTicks int64 `json:"partial_ticks"`
}

// Frame is one planned output frame.
type Frame struct {
	Index int
	Start int64 // timeline ticks, half-open [Start,End)
	End   int64
	// Layers are ordered bottom to top by track key; later layers paint over
	// earlier ones.
	Layers []Layer
	// Overlaps counts additional video layers covering this frame.
	Overlaps int
	Hole     bool
	// HoleTicks is the uncovered duration inside this frame.
	HoleTicks int64
	Blocked   bool
	// BlockingDep is the first dependency that blocked this frame.
	BlockingDep string
	// GapIndices reference transport gaps that intersect this frame.
	GapIndices []int
	Color      metadata.Color
	Width      int
	Height     int
	// PlanHash commits to the deterministic layer plan.
	PlanHash string
	// FrameHash commits to the rendered pixels; filled by the render stage.
	FrameHash string
}

// Timeline is the composed output.
type Timeline struct {
	Timebase clock.Timebase
	Width    int
	Height   int
	Color    metadata.Color
	// Tracks are the normalized video tracks that contributed layers.
	Tracks []metadata.Track
	Frames []Frame
	// FailedTracks are metadata tracks with no usable geometry; their interval
	// blocks dependent frames. Dep names the first blocked dependency.
	FailedTracks []FailedTrack
	// BlockedDeps map frame index to the dependency that blocked it.
	Stats Stats
}

// Stats summarizes composition.
type Stats struct {
	FrameCount     int
	HoleFrames     int
	BoundaryFrames int
	OverlapFrames  int
	BlockedFrames  int
	FirstBlocked   string
}

// FailedTrack is a failed track's interval and its blocking dependency.
type FailedTrack struct {
	Interval model.Interval
	Dep      string
}

// sourceFrame is a derived sample interval for one track.
type sourceFrame struct {
	index int64
	start int64
	end   int64
}

// trackFrames holds one track's converted interval and derived samples.
type trackFrames struct {
	track  metadata.Track
	start  int64
	end    int64
	srcDur int64
	frames []sourceFrame
}

// Compose builds the timeline from an asset, applying transport gaps.
//
// Rules, in order:
//  1. Frame grid: [k*T,(k+1)*T) for k in [0,n) at the configured cadence.
//  2. A source sample is a candidate when it covers the frame's start tick.
//     A sample covering less than 90% of the frame is a boundary sample and is
//     kept with Boundary=true (deterministic: the sample under the start wins).
//  3. Layers are ordered by track key; overlapping video is counted.
//  4. No candidate video sample => hole. A hole at or above the configured
//     threshold blocks the frame.
//  5. A blocked transport gap intersecting the frame blocks it; the first
//     blocking dependency is retained and never replaced.
func Compose(asset *metadata.Asset, gaps []Gap, cfg Config) (*Timeline, error) {
	if !cfg.Timebase.Valid() || cfg.FPSNum <= 0 || cfg.FPSDen <= 0 {
		return nil, model.New(model.CategoryTimeline, "config", "invalid timebase or cadence")
	}
	if len(asset.Tracks) == 0 {
		return nil, model.New(model.CategoryTimeline, "compose", "no usable tracks").
			WithBlockingDep("metadata")
	}
	frameTicks := (cfg.FPSDen * cfg.Timebase.Den) / (cfg.FPSNum * cfg.Timebase.Num)
	if frameTicks <= 0 {
		return nil, model.New(model.CategoryTimeline, "config", "frame duration rounds to zero")
	}

	videos := make([]metadata.Track, 0, len(asset.Tracks))
	for _, t := range asset.Tracks {
		if t.Kind == metadata.KindVideo {
			videos = append(videos, t)
		}
	}
	if len(videos) == 0 {
		return nil, model.New(model.CategoryTimeline, "compose", "no video tracks")
	}
	sort.SliceStable(videos, func(i, j int) bool { return videos[i].Key < videos[j].Key })

	tl := &Timeline{
		Timebase: cfg.Timebase,
		Color:    metadata.ResolveColor(videos),
	}
	tl.Width, tl.Height = canvasSize(videos)
	if cfg.Scale > 1 {
		tl.Width = maxInt(tl.Width/cfg.Scale, 1)
		tl.Height = maxInt(tl.Height/cfg.Scale, 1)
	}
	tl.Tracks = append([]metadata.Track(nil), videos...)

	// Convert each track's interval and cadence into timeline ticks.
	var tfs []trackFrames
	end := int64(0)
	for _, t := range videos {
		start, err := t.Timebase.Convert(t.Start, cfg.Timebase)
		if err != nil {
			return nil, model.Wrap(model.CategoryTimeline, "convert-start", err).WithTrack(t.Key)
		}
		dur, err := t.Timebase.Convert(t.Duration, cfg.Timebase)
		if err != nil {
			return nil, model.Wrap(model.CategoryTimeline, "convert-duration", err).WithTrack(t.Key)
		}
		if dur <= 0 {
			continue
		}
		srcDur := (cfg.FPSDen * t.Timebase.Den) / (cfg.FPSNum * t.Timebase.Num)
		if srcDur <= 0 {
			srcDur = frameTicks
		}
		tf := trackFrames{track: t, start: start, end: start + dur, srcDur: srcDur}
		for s := int64(0); s < dur; s += srcDur {
			e := s + srcDur
			if e > dur {
				e = dur
			}
			// Source sample k covers [start+s, start+e).
			tf.frames = append(tf.frames, sourceFrame{index: s / srcDur, start: start + s, end: start + e})
		}
		tfs = append(tfs, tf)
		if tf.end > end {
			end = tf.end
		}
	}
	// DurationTicks bounds the timeline. A requested duration longer than the
	// media yields hole frames (and therefore a documented, blockable tail)
	// and a shorter one truncates, so the frame grid is always exactly
	// DurationTicks/frameTicks long when a duration is requested.
	if cfg.DurationTicks > 0 {
		end = cfg.DurationTicks
	}
	if end <= 0 {
		return nil, model.New(model.CategoryTimeline, "compose", "timeline duration is zero")
	}

	for _, f := range asset.Failures {
		if t, ok := asset.Track(f.Key); ok {
			iv := t.Interval()
			start, _ := iv.Timebase.Convert(iv.Start, cfg.Timebase)
			stop, _ := iv.Timebase.Convert(iv.End, cfg.Timebase)
			tl.FailedTracks = append(tl.FailedTracks, FailedTrack{
				Interval: model.Interval{Start: start, End: stop, Timebase: cfg.Timebase},
				Dep:      "metadata:" + f.Key,
			})
		}
	}

	for k := int64(0); ; k++ {
		start := k * frameTicks
		if start >= end {
			break
		}
		stop := start + frameTicks
		if stop > end {
			stop = end
		}
		frame := Frame{Index: int(k), Start: start, End: stop, Color: tl.Color,
			Width: tl.Width, Height: tl.Height}
		covered := int64(0)
		for _, tf := range tfs {
			sf, ok := sampleAt(tf.frames, start)
			if !ok {
				continue
			}
			partial := sf.end - start
			if sf.end > stop {
				partial = stop - start
			}
			if partial > covered {
				covered = partial
			}
			layer := Layer{
				TrackKey: tf.track.Key, SourceIndex: sf.index,
				SourceStart: sf.start, SourceEnd: sf.end,
				Rotation: tf.track.Rotation,
				Color:    tf.track.Color,
				W:        tf.track.OutputWidth(), H: tf.track.OutputHeight(),
				CodedW: tf.track.CodedWidth, CodedH: tf.track.CodedHeight,
			}
			// The layer carries the model's full-resolution crop and coded
			// size. Scaling is expressed only through W/H (the output raster),
			// so verification renders remain geometrically faithful.
			if tf.track.Crop != nil {
				c := *tf.track.Crop
				layer.Crop = &c
			}
			if cfg.Scale > 1 {
				layer.W = maxInt(layer.W/cfg.Scale, 1)
				layer.H = maxInt(layer.H/cfg.Scale, 1)

			}
			layer.X = (tl.Width - layer.W) / 2
			layer.Y = (tl.Height - layer.H) / 2
			layer.PartialTicks = partial
			// A sample shorter than a nominal frame (the timeline tail) is a
			// boundary sample. Compare against the frame cadence rather than
			// the possibly-truncated tail frame.
			layer.Boundary = partial < frameTicks*9/10
			frame.Layers = append(frame.Layers, layer)
		}
		if len(frame.Layers) > 1 {
			frame.Overlaps = len(frame.Layers) - 1
		}
		frame.HoleTicks = stop - start - covered
		if len(frame.Layers) == 0 || covered == 0 {
			frame.Hole = true
			// The hole's real length extends to the next covered tick. For a
			// leading hole that is the start of the first layer; for an
			// interior hole it is the current frame. This avoids measuring a
			// multi-second leading hole as a single frame.
			holeTicks := frame.HoleTicks
			if len(frame.Layers) == 0 {
				if next, ok := nextCovered(tfs, start); ok && next > start {
					holeTicks = next - start
				}
			}
			if cfg.HoleBlockThresholdMs > 0 {
				holeMs := cfg.Timebase.ToMillis(holeTicks)
				if holeMs >= cfg.HoleBlockThresholdMs {
					frame.Blocked = true
					frame.BlockingDep = "coverage:hole"
				}
			}
		}
		for i := range gaps {
			g := gaps[i]
			if g.Start <= start && g.End > start {
				frame.GapIndices = append(frame.GapIndices, i)
				if g.Blocked {
					frame.Blocked = true
					if frame.BlockingDep == "" {
						frame.BlockingDep = g.Dep
					}
				}
			}
		}
		for _, ft := range tl.FailedTracks {
			if ft.Interval.Start < stop && ft.Interval.End > start && frame.BlockingDep == "" {
				frame.Blocked = true
				frame.BlockingDep = ft.Dep
			}
		}
		frame.PlanHash = hashFramePlan(frame)
		tl.Frames = append(tl.Frames, frame)
	}

	for _, f := range tl.Frames {
		if f.Hole {
			tl.Stats.HoleFrames++
		}
		if f.Overlaps > 0 {
			tl.Stats.OverlapFrames++
		}
		for _, l := range f.Layers {
			if l.Boundary {
				tl.Stats.BoundaryFrames++
				break
			}
		}
		if f.Blocked {
			tl.Stats.BlockedFrames++
			if tl.Stats.FirstBlocked == "" {
				tl.Stats.FirstBlocked = f.BlockingDep
			}
		}
	}
	tl.Stats.FrameCount = len(tl.Frames)
	if tl.Stats.BlockedFrames > 0 && tl.Stats.FirstBlocked == "" {
		tl.Stats.FirstBlocked = "timeline:unknown"
	}
	return tl, nil
}

// sampleAt returns the sample covering t, if any. Samples are sorted and
// half-open, so the search is deterministic.
func sampleAt(frames []sourceFrame, t int64) (sourceFrame, bool) {
	i := sort.Search(len(frames), func(i int) bool { return frames[i].end > t })
	if i < len(frames) && frames[i].start <= t && t < frames[i].end {
		return frames[i], true
	}
	return sourceFrame{}, false
}

// nextCovered returns the earliest tick >= t at which any track has a sample.
func nextCovered(tfs []trackFrames, t int64) (int64, bool) {
	best := int64(-1)
	for _, tf := range tfs {
		i := sort.Search(len(tf.frames), func(i int) bool { return tf.frames[i].end > t })
		if i >= len(tf.frames) {
			continue
		}
		if best < 0 || tf.frames[i].start < best {
			best = tf.frames[i].start
		}
	}
	return best, best >= 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func canvasSize(tracks []metadata.Track) (int, int) {
	w, h := 0, 0
	for _, t := range tracks {
		if t.OutputWidth() > w {
			w = t.OutputWidth()
		}
		if t.OutputHeight() > h {
			h = t.OutputHeight()
		}
	}
	return w, h
}

// hashFramePlan canonicalizes the layer plan and hashes it.
func hashFramePlan(f Frame) string {
	h := sha256.New()
	fmt.Fprintf(h, "frame=%d;%d;%d;%dx%d;color=%s;hole=%t;overlap=%d;blocked=%t;dep=%s;",
		f.Index, f.Start, f.End, f.Width, f.Height, f.Color.Key(), f.Hole, f.Overlaps, f.Blocked, f.BlockingDep)
	for _, l := range f.Layers {
		crop := "-"
		if l.Crop != nil {
			crop = fmt.Sprintf("%d,%d,%d,%d", l.Crop.X, l.Crop.Y, l.Crop.W, l.Crop.H)
		}
		fmt.Fprintf(h, "layer=%s@%d[%d,%d);rot=%d;crop=%s;rect=%d,%d,%d,%d;color=%s;boundary=%t;gap=",
			l.TrackKey, l.SourceIndex, l.SourceStart, l.SourceEnd, l.Rotation, crop,
			l.X, l.Y, l.W, l.H, l.Color.Key(), l.Boundary)
	}
	for _, gi := range f.GapIndices {
		fmt.Fprintf(h, "%d,", gi)
	}
	return hex.EncodeToString(h.Sum(nil))
}
