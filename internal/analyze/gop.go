package analyze

import (
	"sort"

	"tscheck/internal/model"
)

// ContainsIDR reports whether an H.264 Annex-B elementary stream chunk
// contains an IDR access unit (NAL unit type 5).
func ContainsIDR(es []byte) bool {
	for i := 0; i+4 < len(es); i++ {
		if es[i] != 0x00 || es[i+1] != 0x00 {
			continue
		}
		var hdr int
		if es[i+2] == 0x01 {
			hdr = i + 3
		} else if es[i+2] == 0x00 && es[i+3] == 0x01 {
			hdr = i + 4
		} else {
			continue
		}
		if hdr < len(es) && es[hdr]&0x1F == 5 {
			return true
		}
		i = hdr
	}
	return false
}

// GOPStats summarizes keyframe cadence for one video track.
type GOPStats struct {
	Frames        int
	Keyframes     int
	KeyframeTicks []model.Ticks // unwrapped keyframe positions, 27 MHz
	FrameDeltas   []model.Ticks // raw VFR intervals between frames
	IntervalP50   model.Ticks
	IntervalP90   model.Ticks
	IntervalP99   model.Ticks
}

// GOPTracker collects frame and keyframe timestamps on the shared clock.
// Unanchored timestamps are skipped: they cannot be placed on the shared
// axis and must never be reported as keyframes.
type GOPTracker struct {
	frames    int
	keyframes []model.Ticks
	deltas    []model.Ticks
	last      model.Ticks
	hasLast   bool
}

// NewGOPTracker creates a GOPTracker.
func NewGOPTracker() *GOPTracker { return &GOPTracker{} }

// AddFrame records one frame; keyframe marks an IDR access unit.
func (g *GOPTracker) AddFrame(ts model.Timestamp, keyframe bool) {
	if !ts.Anchored {
		return
	}
	g.frames++
	if g.hasLast && ts.Ticks > g.last {
		g.deltas = append(g.deltas, ts.Ticks-g.last)
	}
	g.last = ts.Ticks
	g.hasLast = true
	if keyframe {
		g.keyframes = append(g.keyframes, ts.Ticks)
	}
}

// Stats returns the aggregated statistics with interval quantiles.
func (g *GOPTracker) Stats() GOPStats {
	st := GOPStats{
		Frames:        g.frames,
		Keyframes:     len(g.keyframes),
		KeyframeTicks: append([]model.Ticks(nil), g.keyframes...),
		FrameDeltas:   append([]model.Ticks(nil), g.deltas...),
	}
	if len(g.deltas) > 0 {
		sorted := append([]model.Ticks(nil), g.deltas...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		st.IntervalP50 = quantile(sorted, 0.50)
		st.IntervalP90 = quantile(sorted, 0.90)
		st.IntervalP99 = quantile(sorted, 0.99)
	}
	return st
}

// quantile returns the nearest-rank quantile of sorted values.
func quantile(sorted []model.Ticks, q float64) model.Ticks {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q*float64(len(sorted)) + 0.5)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
