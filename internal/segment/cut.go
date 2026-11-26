// Package segment cuts recovered block streams into timeline segments.
// Boundaries only ever land on complete sample frames; unrecovered gaps
// split segments rather than being filled, and every segment records the
// source blocks it came from for end-to-end provenance.
package segment

import (
	"fmt"

	"audiorestore/internal/model"
	"audiorestore/internal/wav"
)

// Config controls windowed cutting. All positions are integer frame
// counts on the owning track's sample clock.
type Config struct {
	// WindowFrames is the segment length in sample frames.
	WindowFrames int64
	// KeepPartial keeps a final shorter segment inside a contiguous run;
	// when false the trailing partial window is dropped.
	KeepPartial bool
}

// Segment is one cut fragment with exact PTS bounds and provenance.
type Segment struct {
	ID           string
	Track        string
	StartFrame   int64
	EndFrame     int64 // half-open
	StartPTS     model.Rational
	EndPTS       model.Rational
	SourceBlocks []int
	// StorageID is assigned by the dedupe stage; equal content shares it.
	StorageID string
}

type run struct {
	start  int64
	end    int64
	blocks []int
}

// Cut partitions the block stream into segments. Bad blocks (missing,
// corrupt, truncated, parity) never contribute audio: they delimit
// contiguous runs, so no segment spans an unrecovered gap.
func Cut(trackID string, rate model.SampleRate, blocks []wav.FrameBlock, cfg Config) ([]Segment, error) {
	if cfg.WindowFrames <= 0 {
		return nil, model.TrackedError{Kind: model.ErrInvalidParam, Track: trackID,
			Msg: fmt.Sprintf("window frames must be positive, got %d", cfg.WindowFrames)}
	}
	runs := collectRuns(blocks)
	var segs []Segment
	for _, rn := range runs {
		for s := rn.start; s < rn.end; s += cfg.WindowFrames {
			e := s + cfg.WindowFrames
			if e > rn.end {
				e = rn.end
			}
			if e-s < cfg.WindowFrames && !cfg.KeepPartial {
				break
			}
			segs = append(segs, Segment{
				ID:           fmt.Sprintf("%s/seg%04d", trackID, len(segs)),
				Track:        trackID,
				StartFrame:   s,
				EndFrame:     e,
				StartPTS:     model.SampleToPTS(s, rate),
				EndPTS:       model.SampleToPTS(e, rate),
				SourceBlocks: coveringBlocks(rn.blocks, blocks, s, e),
			})
		}
	}

	// Enforce monotonic PTS as a cut invariant.
	var prev model.Rational
	for _, s := range segs {
		if !prev.IsZero() && s.StartPTS.Cmp(prev) < 0 {
			return nil, model.TrackedError{Kind: model.ErrPTSNotMonotonic, Track: trackID,
				Sample: s.StartFrame, Msg: "segment PTS went backwards"}
		}
		prev = s.EndPTS
	}
	return segs, nil
}

// collectRuns groups intact audio blocks into contiguous frame ranges.
// A non-audio block, a gap, or a frame-range discontinuity closes a run.
func collectRuns(blocks []wav.FrameBlock) []run {
	var runs []run
	var cur *run
	var prevEnd int64
	for i := range blocks {
		b := &blocks[i]
		if b.Parity || b.Missing || b.Corrupt || b.Truncated {
			cur = nil
			continue
		}
		end := b.StartFrame + int64(b.Frames)
		if cur == nil || b.StartFrame != prevEnd {
			runs = append(runs, run{start: b.StartFrame, end: end})
			cur = &runs[len(runs)-1]
		} else {
			cur.end = end
		}
		cur.blocks = append(cur.blocks, b.Index)
		prevEnd = end
	}
	return runs
}

// coveringBlocks returns the distinct intact block indices whose frame
// ranges intersect [start,end), preserving encounter order.
func coveringBlocks(src []int, blocks []wav.FrameBlock, start, end int64) []int {
	out := make([]int, 0, len(src))
	for _, idx := range src {
		b := blocks[idx]
		bEnd := b.StartFrame + int64(b.Frames)
		if bEnd <= start || b.StartFrame >= end {
			continue
		}
		out = append(out, idx)
	}
	return out
}
