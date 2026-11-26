package clock

import (
	"sort"

	"archivectl/internal/core"
)

// DriftModel is a linear mapping from unwrapped device ticks to frames.
// TicksPerFrame is an exact rational, so callers can map every point from the
// same origin instead of accumulating per-block rounding errors.
type DriftModel struct {
	TicksPerFrame core.Rational `json:"ticks_per_frame"`
	OriginTick    int64         `json:"origin_tick"`
	OriginFrame   int64         `json:"origin_frame"`
}

// FrameForTick maps an absolute unwrapped tick to an absolute sample frame.
func (m DriftModel) FrameForTick(unwrappedTick int64) int64 {
	return (core.Timebase{}).AbsoluteFrame(
		unwrappedTick, m.OriginTick, m.OriginFrame, m.TicksPerFrame)
}

// EstimateDrift unwraps anchor ticks and fits the drift slope using the two
// anchors with the widest frame separation. The fixture clock is linear; this
// also keeps the estimate deterministic and independent of cue order.
func EstimateDrift(anchors []Anchor) (DriftModel, []NormalizedAnchor, error) {
	const op = "clock.EstimateDrift"
	anchors = dedupeAndSortAnchors(anchors)
	if len(anchors) < 2 {
		return DriftModel{}, nil, core.Newf(core.KindClock, op,
			"need at least two anchors, got %d", len(anchors))
	}
	raw := make([]uint32, len(anchors))
	for i := range anchors {
		raw[i] = anchors[i].Tick
	}
	ticks, err := UnwrapTicks(raw)
	if err != nil {
		return DriftModel{}, nil, err
	}

	lo, hi := 0, 0
	for i := range anchors {
		if anchors[i].Frame < anchors[lo].Frame {
			lo = i
		}
		if anchors[i].Frame > anchors[hi].Frame {
			hi = i
		}
	}
	if anchors[lo].Frame == anchors[hi].Frame {
		return DriftModel{}, nil, core.New(core.KindClock, op, "anchors do not span distinct frames")
	}
	frameDelta := anchors[hi].Frame - anchors[lo].Frame
	tickDelta := ticks[hi] - ticks[lo]
	rate, err := core.Rat(tickDelta, frameDelta)
	if err != nil {
		return DriftModel{}, nil, core.Wrap(core.KindClock, op, "", err)
	}
	if rate.N <= 0 {
		return DriftModel{}, nil, core.Newf(core.KindClock, op,
			"non-positive tick rate %d/%d", rate.N, rate.D)
	}
	model := DriftModel{
		TicksPerFrame: rate,
		OriginTick:    ticks[lo],
		OriginFrame:   anchors[lo].Frame,
	}

	normalized := make([]NormalizedAnchor, len(anchors))
	for i, a := range anchors {
		mapped := model.FrameForTick(ticks[i])
		normalized[i] = NormalizedAnchor{
			CueID:         a.CueID,
			RawTick:       a.Tick,
			UnwrappedTick: ticks[i],
			WAVFrame:      a.Frame,
			MappedFrame:   mapped,
			FrameError:    mapped - a.Frame,
		}
	}
	if err := core.CheckMonotonic(anchorMappedFrames(normalized)); err != nil {
		return DriftModel{}, nil, err
	}
	return model, normalized, nil
}

// NormalizeBlockRecords unwraps block ticks and attaches exact sample ranges.
// Sample ranges come from each record's FrameCount; no fixed block length is
// assumed.
func NormalizeBlockRecords(records []BlockRecord, model DriftModel) ([]NormalizedBlock, error) {
	ticks, err := UnwrapRecords(records)
	if err != nil {
		return nil, err
	}
	counts := make([]int, len(records))
	raw := make([]uint32, len(records))
	for i := range records {
		counts[i] = records[i].FrameCount
		raw[i] = records[i].Tick
	}
	blocks, err := CumulativeRanges(counts)
	if err != nil {
		return nil, err
	}
	for i := range blocks {
		blocks[i].RawTick = raw[i]
		blocks[i].UnwrappedTick = ticks[i]
		blocks[i].MappedFrame = model.FrameForTick(ticks[i])
	}
	return blocks, nil
}

// SortedUniqueFrames returns sorted unique WAV frame positions. Duplicate cue
// points collapse to one keyframe hit.
func SortedUniqueFrames(frames []int64) []int64 {
	if len(frames) == 0 {
		return nil
	}
	cp := append([]int64(nil), frames...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	out := cp[:0]
	var last int64
	for i, v := range cp {
		if i == 0 || v != last {
			out = append(out, v)
		}
		last = v
	}
	return out
}

func anchorMappedFrames(a []NormalizedAnchor) []int64 {
	out := make([]int64, len(a))
	for i := range a {
		out[i] = a[i].MappedFrame
	}
	return out
}

func dedupeAndSortAnchors(in []Anchor) []Anchor {
	byFrame := make(map[int64]Anchor, len(in))
	for _, a := range in {
		// A duplicated cue describes the same keyframe; the first copy is
		// sufficient for drift estimation.
		if _, ok := byFrame[a.Frame]; !ok {
			byFrame[a.Frame] = a
		}
	}
	out := make([]Anchor, 0, len(byFrame))
	for _, a := range byFrame {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Frame < out[j].Frame })
	return out
}
