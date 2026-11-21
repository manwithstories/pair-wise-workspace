// Package timeline mixes recovered per-track blocks onto one unified sample
// scale. Tracks are expected to be gap-free after recovery; any cross-track
// drift beyond one sample is reported as a classified sync error rather than
// corrected silently.
package timeline

import (
	"math"
	"sort"

	"avcheck/internal/clock"
)

// Track is one participant's recovered block stream plus its declared offset.
type Track struct {
	ID           uint32
	OffsetTicks  int64
	Channels     int
	Blocks       []*clock.Block
}

// Mixed is the aligned, clipped result of mixing all tracks.
type Mixed struct {
	Channels       int
	StartTick      int64
	Frames         int // sample frames after frame-boundary alignment
	Samples        []int32
	PerTrackFrames map[uint32]int
}

// Mix validates and sums the tracks. All tracks must share the channel count.
func Mix(tracks []Track) (*Mixed, error) {
	if len(tracks) == 0 {
		return nil, clock.NewError(clock.KindSync, "no tracks to mix")
	}
	built := make([]denseTrack, 0, len(tracks))
	channels := tracks[0].Channels
	for _, tr := range tracks {
		if tr.Channels != channels || channels < 1 {
			return nil, clock.NewError(clock.KindSync,
				"track %d channels %d != mix channels %d", tr.ID, tr.Channels, channels)
		}
		d, err := materialize(tr)
		if err != nil {
			return nil, err
		}
		built = append(built, d)
	}

	// Cross-track alignment: starts and lengths must agree within one sample.
	refStart := built[0].start
	refFrames := built[0].frames
	for _, d := range built[1:] {
		if diff := d.start - refStart; diff < -1 || diff > 1 {
			return nil, clock.NewError(clock.KindSync,
				"track %d starts %d ticks from reference, tolerance is 1",
				d.id, d.start-refStart)
		}
		if diff := d.frames - refFrames; diff < -1 || diff > 1 {
			return nil, clock.NewError(clock.KindSync,
				"track %d length differs by %d frames, tolerance is 1",
				d.id, d.frames-refFrames)
		}
	}

	// Common span: keep every track fully covered, then trim to a frame edge.
	commonFrames := refFrames
	for _, d := range built[1:] {
		if d.frames < commonFrames {
			commonFrames = d.frames
		}
	}
	aligned := int(clock.AlignToFrame(int64(commonFrames)))
	if aligned == 0 {
		return nil, clock.NewError(clock.KindSync, "mix shorter than one 30fps frame")
	}

	out := make([]int32, aligned*channels)
	for _, d := range built {
		off := int(d.start-refStart) * channels
		for f := 0; f < aligned; f++ {
			for c := 0; c < channels; c++ {
				idx := f*channels + c
				src := idx + off
				if src < 0 || src >= len(d.data) {
					continue // within the 1-tick tolerance; no silent fill beyond it
				}
				out[idx] = saturateAdd(out[idx], d.data[src])
			}
		}
	}
	perTrack := make(map[uint32]int, len(built))
	for _, d := range built {
		perTrack[d.id] = d.frames
	}
	return &Mixed{
		Channels:       channels,
		StartTick:      refStart,
		Frames:         aligned,
		Samples:        out,
		PerTrackFrames: perTrack,
	}, nil
}

// materialize expands a track's blocks into one dense, gap-checked buffer.
func materialize(tr Track) (denseTrack, error) {
	if len(tr.Blocks) == 0 {
		return denseTrack{}, clock.NewError(clock.KindSync, "track %d has no blocks", tr.ID)
	}
	blocks := append([]*clock.Block(nil), tr.Blocks...)
	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].StartTick < blocks[j].StartTick
	})
	start := blocks[0].StartTick + tr.OffsetTicks
	end := blocks[len(blocks)-1].EndTick() + tr.OffsetTicks
	frames := int(end - start)
	data := make([]int32, frames*tr.Channels)
	// Internal gap checking runs in the blocks' own scale; the offset is a
	// constant translation and must not be applied twice.
	var prevEnd int64 = blocks[0].StartTick
	for _, b := range blocks {
		if b.StartTick != prevEnd {
			return denseTrack{}, clock.NewError(clock.KindSync,
				"track %d gap/overlap at tick %d (block seq %d)", tr.ID, b.StartTick, b.Seq)
		}
		if b.Channels != tr.Channels {
			return denseTrack{}, clock.NewError(clock.KindSync,
				"track %d block seq %d channel mismatch", tr.ID, b.Seq)
		}
		copy(data[(b.StartTick-blocks[0].StartTick)*int64(tr.Channels):], b.Samples)
		prevEnd = b.StartTick + int64(b.Frames())
	}
	return denseTrack{id: tr.ID, start: start, frames: frames, data: data}, nil
}

type denseTrack struct {
	id     uint32
	start  int64
	frames int
	data   []int32
}

// saturateAdd sums two int32 samples with hard clipping.
func saturateAdd(a, b int32) int32 {
	if b > 0 && a > math.MaxInt32-b {
		return math.MaxInt32
	}
	if b < 0 && a < math.MinInt32-b {
		return math.MinInt32
	}
	return a + b
}
