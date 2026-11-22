// Package timeline repairs track timestamps.  It detects regressions,
// duplicates and cross-track drift using exact rational time, applies a
// deterministic repair rule, and records the size of every correction.
package timeline

import (
	"fmt"
	"sort"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
)

// Frame is one timed unit on a track.
type Frame struct {
	Track uint64
	// Index is the frame's position within the track, after ordering.
	Index int
	// Original is the timestamp as stored in the container.
	Original clock.Rational
	// Repaired is the timestamp after the fix pass; it equals Original when
	// no repair was needed.
	Repaired clock.Rational
	// Offset is the source byte offset of the block, for diagnostics.
	Offset int64
	// Keyframe marks a random-access point.
	Keyframe bool
	// Bytes is the payload size, used by bitrate statistics.
	Bytes int64
	// Digest identifies the payload for integrity comparisons.
	Digest uint32
	// Missing is set when the frame belongs to an explicit transport gap.  A
	// missing frame keeps its slot but is marked, never silently re-timed.
	Missing bool
}

// TrackInfo describes one track's declared timing.
type TrackInfo struct {
	Number    uint64
	Kind      string
	DefaultNS uint64 // DefaultDuration in nanoseconds, 0 if absent
	Scale     clock.Rational
	// Frames are ordered by Original timestamp after Group.
	Frames []Frame
}

// Repair records one correction.
type Repair struct {
	Track      uint64 `json:"track"`
	Index      int    `json:"index"`
	Offset     int64  `json:"offset"`
	Kind       string `json:"kind"`
	BeforeUsec int64  `json:"before_us"`
	AfterUsec  int64  `json:"after_us"`
	DeltaUsec  int64  `json:"delta_us"`
	Before     string `json:"before"`
	After      string `json:"after"`
}

// Drift reports cross-track divergence at a common instant.
type Drift struct {
	TrackA   uint64 `json:"track_a"`
	TrackB   uint64 `json:"track_b"`
	AtUsec   int64  `json:"at_us"`
	SkewUsec int64  `json:"skew_us"`
	Beyond   bool   `json:"beyond_limit"`
}

// Policy configures repair behaviour.  Zero values pick safe defaults, and a
// zero MaxDrift disables drift flagging.
type Policy struct {
	// MaxDrift is the largest tolerated cross-track skew.
	MaxDrift clock.Rational
	// DuplicateRule selects how repeated timestamps are resolved:
	// "nudge" (default) advances by one tick, "drop" marks later copies
	// missing.
	DuplicateRule string
	// RegressRule selects how backwards timestamps are resolved:
	// "clamp" (default) holds the previous timestamp plus one tick,
	// "drop" marks the frame missing.
	RegressRule string
}

// Result is the repair outcome.
type Result struct {
	Tracks     map[uint64]*TrackInfo
	Repairs    []Repair
	Drifts     []Drift
	Regress    int
	Duplicates int
	// Tick is the smallest representable time step used by the nudge rule.
	Tick   clock.Rational
	Faults *fault.Set
}

// Timeline is the set of tracks under repair.
type Timeline struct {
	Tracks map[uint64]*TrackInfo
	Policy Policy
	// Tick is the least timestamp unit; for Matroska this is one tick of the
	// segment timescale.
	Tick clock.Rational
}

// New builds a timeline with the given tick size.
func New(tick clock.Rational, p Policy) *Timeline {
	if p.DuplicateRule == "" {
		p.DuplicateRule = "nudge"
	}
	if p.RegressRule == "" {
		p.RegressRule = "clamp"
	}
	return &Timeline{Tracks: make(map[uint64]*TrackInfo), Policy: p, Tick: tick}
}

// AddTrack registers a track and its frames.
func (tl *Timeline) AddTrack(info *TrackInfo) {
	tl.Tracks[info.Number] = info
}

// Repair orders each track by source offset (container order), then applies
// the rules.  The ordering is stable across runs.
func (tl *Timeline) Repair() *Result {
	res := &Result{Tracks: make(map[uint64]*TrackInfo, len(tl.Tracks)), Tick: tl.Tick, Faults: &fault.Set{}}
	order := make([]uint64, 0, len(tl.Tracks))
	for n := range tl.Tracks {
		order = append(order, n)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	for _, n := range order {
		info := tl.Tracks[n]
		frames := append([]Frame(nil), info.Frames...)
		// Repair follows container order, identified by source offset.  Sorting
		// by timestamp instead would erase the regressions we must detect.
		sort.SliceStable(frames, func(i, j int) bool {
			if frames[i].Offset != frames[j].Offset {
				return frames[i].Offset < frames[j].Offset
			}
			return frames[i].Original.Cmp(frames[j].Original) < 0
		})
		res.Tracks[n] = &TrackInfo{Number: info.Number, Kind: info.Kind, DefaultNS: info.DefaultNS, Scale: info.Scale, Frames: frames}

		var prev clock.Rational
		havePrev := false
		for i := range frames {
			f := &frames[i]
			f.Index = i
			f.Repaired = f.Original
			if f.Missing {
				continue
			}
			switch {
			case havePrev && f.Original.Cmp(prev) < 0:
				res.Regress++
				kind := "timestamp-regression"
				tl.report(res, fault.KindTimestampRegress, n, f, fmt.Sprintf("timestamp %s precedes previous %s", f.Original, prev))
				if tl.Policy.RegressRule == "drop" {
					f.Missing = true
					res.Repairs = append(res.Repairs, tl.makeRepair(n, i, f, kind+"-drop", f.Original, f.Original))
					continue
				}
				f.Repaired = prev.Add(tl.Tick)
				res.Repairs = append(res.Repairs, tl.makeRepair(n, i, f, kind, f.Original, f.Repaired))
				prev = f.Repaired
			case havePrev && f.Original.Equal(prev):
				res.Duplicates++
				tl.report(res, fault.KindTimestampRepeat, n, f, fmt.Sprintf("timestamp %s repeats the previous frame", f.Original))
				if tl.Policy.DuplicateRule == "drop" {
					f.Missing = true
					res.Repairs = append(res.Repairs, tl.makeRepair(n, i, f, "duplicate-drop", f.Original, f.Original))
					continue
				}
				f.Repaired = prev.Add(tl.Tick)
				res.Repairs = append(res.Repairs, tl.makeRepair(n, i, f, "timestamp-duplicate", f.Original, f.Repaired))
				prev = f.Repaired
			default:
				prev = f.Repaired
			}
			havePrev = true
		}
	}
	if !tl.Policy.MaxDrift.IsZero() {
		res.Drifts = tl.detectDrift(order, res)
	}
	return res
}

func (tl *Timeline) report(res *Result, k fault.Kind, track uint64, f *Frame, msg string) {
	off := fault.NoOffset()
	if f.Offset >= 0 {
		off = fault.NewOffset(uint64(f.Offset))
	}
	res.Faults.Add(fault.New(fault.ModuleTimeline, k, off, msg).OnTrack(track))
}

func (tl *Timeline) makeRepair(track uint64, index int, f *Frame, kind string, before, after clock.Rational) Repair {
	return Repair{
		Track: track, Index: index, Offset: f.Offset, Kind: kind,
		Before: before.String(), After: after.String(),
		BeforeUsec: toMicros(before), AfterUsec: toMicros(after),
		DeltaUsec: toMicros(after.Sub(before)),
	}
}

func toMicros(r clock.Rational) int64 {
	return int64(r.Float64() * 1e6)
}

// detectDrift compares, at several checkpoints, the latest start time among
// all tracks so a late track is visible against the others.
func (tl *Timeline) detectDrift(order []uint64, res *Result) []Drift {
	var drifts []Drift
	for a := 0; a < len(order); a++ {
		for b := a + 1; b < len(order); b++ {
			ta, tb := res.Tracks[order[a]], res.Tracks[order[b]]
			start := clock.Max(trackStart(ta), trackStart(tb))
			skew := trackStart(ta).Sub(trackStart(tb))
			d := Drift{
				TrackA: ta.Number, TrackB: tb.Number,
				AtUsec: toMicros(start), SkewUsec: toMicros(skew),
				Beyond: skew.Abs().Cmp(tl.Policy.MaxDrift) > 0,
			}
			if d.Beyond {
				drifts = append(drifts, d)
				res.Faults.Add(fault.New(fault.ModuleTimeline, fault.KindDrift, fault.NoOffset(),
					fmt.Sprintf("tracks %d and %d start %.3f ms apart (limit %s)", ta.Number, tb.Number,
						skew.Abs().Millis(), tl.Policy.MaxDrift)).OnTrack(ta.Number))
			}
		}
	}
	return drifts
}

func trackStart(t *TrackInfo) clock.Rational {
	for _, f := range t.Frames {
		if !f.Missing {
			return f.Repaired
		}
	}
	return clock.Rational{}
}

// StartEnd returns the visible extent of a track.
func (t *TrackInfo) StartEnd() (clock.Rational, clock.Rational) {
	var start, end clock.Rational
	have := false
	for _, f := range t.Frames {
		if f.Missing {
			continue
		}
		if !have {
			start, end = f.Repaired, f.Repaired
			have = true
			continue
		}
		if f.Repaired.Cmp(start) < 0 {
			start = f.Repaired
		}
		if f.Repaired.Cmp(end) > 0 {
			end = f.Repaired
		}
	}
	return start, end
}

// FrameBytes sums payload bytes of visible frames.
func (t *TrackInfo) FrameBytes() int64 {
	var n int64
	for _, f := range t.Frames {
		if !f.Missing {
			n += f.Bytes
		}
	}
	return n
}

// Visible counts frames that were not marked missing.
func (t *TrackInfo) Visible() int {
	n := 0
	for _, f := range t.Frames {
		if !f.Missing {
			n++
		}
	}
	return n
}
