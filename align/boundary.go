// Package align computes per-representation cut points from scan indexes and
// aligns them across bitrate ladders on the unified 90 kHz clock.
package align

import (
	"sort"

	"mediaslice/media"
	"mediaslice/scan"
)

// Interval is a media-time range in 90 kHz ticks.
type Interval struct {
	From media.Ticks `json:"from"`
	To   media.Ticks `json:"to"`
	// Unsliceable is true when no keyframe existed inside the wait window.
	Unsliceable bool `json:"unsliceable,omitempty"`
}

// CutPoint is one chosen segment boundary for one representation.
type CutPoint struct {
	Tick        media.Ticks `json:"tick"`
	SampleIndex int         `json:"sample_index"`
	Keyframe    bool        `json:"keyframe"`
	// WaitTicks is how far behind the ideal grid the cut had to wait.
	WaitTicks media.Ticks `json:"wait_ticks"`
}

// TrackBoundary holds the cutting plan for one representation.
type TrackBoundary struct {
	RepID        string          `json:"rep_id"`
	SegmentTicks media.Ticks     `json:"segment_ticks"`
	Cuts         []CutPoint      `json:"cuts"`
	Unsliceable  []Interval      `json:"unsliceable,omitempty"`
	TrackErrors  media.ErrorList `json:"track_errors,omitempty"`
}

// BoundaryConfig controls cut point generation.
type BoundaryConfig struct {
	SegmentTicks    media.Ticks
	KeyframeWaitMax media.Ticks
}

// ComputeTrackBoundary walks the actual VFR sample timings of one track and
// snaps each ideal segment boundary to the next keyframe sample, never
// exceeding KeyframeWaitMax. Regions without an eligible keyframe are
// recorded explicitly as unsliceable instead of guessed.
func ComputeTrackBoundary(repID string, idx *scan.FileIndex, cfg BoundaryConfig) *TrackBoundary {
	out := &TrackBoundary{RepID: repID, SegmentTicks: cfg.SegmentTicks}
	video := idx.VideoTrack()
	if video == nil {
		out.TrackErrors = append(out.TrackErrors,
			media.NewError(media.KindMissingTrack, idx.SourcePath, "no video track"))
		return out
	}
	n := video.SampleCount()
	if n == 0 {
		out.TrackErrors = append(out.TrackErrors,
			media.NewError(media.KindBadSampleTable, idx.SourcePath, "empty video sample table"))
		return out
	}
	endTick := video.StartTick[n-1] + video.Duration[n-1]

	// searchStart advances only to previously consumed keyframes; searches
	// themselves never mutate it, so an unsliceable gap cannot skip the next
	// real keyframe.
	searchStart := 0
	// nearestKey returns the keyframe closest to ideal whose wait stays
	// within the configured window; a keyframe one tick early (VFR jitter)
	// is a valid snap target, so the scan looks one sample back as well.
	nearestKey := func(ideal media.Ticks) (int, CutPoint, bool) {
		lo := searchStart
		if lo > 0 {
			lo-- // permit a keyframe placed slightly before the ideal grid
		}
		best := -1
		var bestDist media.Ticks = 1 << 62
		for i := lo; i < n; i++ {
			smp := video.Sample(i)
			if !smp.Keyframe {
				continue
			}
			wait := smp.StartTick - ideal
			dist := wait
			if dist < 0 {
				dist = -dist
			}
			if dist < bestDist {
				best, bestDist = i, dist
			}
			if smp.StartTick > ideal+cfg.KeyframeWaitMax {
				break
			}
		}
		if best < 0 || bestDist > cfg.KeyframeWaitMax {
			return 0, CutPoint{}, false
		}
		smp := video.Sample(best)
		return best, CutPoint{
			Tick:        smp.StartTick,
			SampleIndex: smp.Index,
			Keyframe:    true,
			WaitTicks:   smp.StartTick - ideal,
		}, true
	}

	grid := cfg.SegmentTicks
	for ideal := media.Ticks(0); ideal < endTick; ideal += grid {
		// The first cut at tick 0 is always the first sample (stream start).
		if ideal == 0 {
			out.Cuts = append(out.Cuts, CutPoint{Tick: 0, SampleIndex: 0, Keyframe: video.Keyframe[0]})
			searchStart = 1
			continue
		}
		i, cp, ok := nearestKey(ideal)
		if !ok {
			out.Unsliceable = append(out.Unsliceable, Interval{
				From:        ideal,
				To:          minTick(ideal+grid, endTick),
				Unsliceable: true,
			})
			continue
		}
		out.Cuts = append(out.Cuts, cp)
		searchStart = i + 1
	}
	// Terminal cut at the actual stream end so the final tail segment is
	// emitted rather than dropped. It is the last sample's end tick, not an
	// averaged duration, preserving VFR correctness.
	lastCut := out.Cuts[len(out.Cuts)-1].Tick
	if endTick > lastCut {
		out.Cuts = append(out.Cuts, CutPoint{
			Tick: endTick, SampleIndex: n, Keyframe: false,
			WaitTicks: 0,
		})
	}
	return out
}

// CommonBoundary is one aligned boundary shared by every representation.
type CommonBoundary struct {
	Index    int                 `json:"index"`
	GridTick media.Ticks         `json:"grid_tick"`
	PerRep   map[string]CutPoint `json:"per_rep"`
	// DeltaTicks is max-min of the per-representation boundary ticks; the
	// acceptance contract requires this to stay within one clock tick.
	DeltaTicks media.Ticks `json:"delta_ticks"`
}

// AlignResult is the full ladder alignment.
type AlignResult struct {
	SegmentTicks media.Ticks           `json:"segment_ticks"`
	Boundaries   []CommonBoundary      `json:"boundaries"`
	MaxDelta     media.Ticks           `json:"max_delta_ticks"`
	PerTrack     []*TrackBoundary      `json:"per_track"`
	Unsliceable  map[string][]Interval `json:"unsliceable,omitempty"`
}

// AlignLadder aligns independently computed per-representation boundaries onto
// the common ideal grid, matching cuts by grid position rather than raw tick
// equality so VFR jitter stays bounded.
func AlignLadder(bounds []*TrackBoundary, cfg BoundaryConfig) *AlignResult {
	res := &AlignResult{
		SegmentTicks: cfg.SegmentTicks,
		Unsliceable:  map[string][]Interval{},
	}
	for _, b := range bounds {
		res.PerTrack = append(res.PerTrack, b)
		if len(b.Unsliceable) > 0 {
			res.Unsliceable[b.RepID] = b.Unsliceable
		}
	}
	if len(bounds) == 0 {
		return res
	}
	// Index cuts per rep by their ideal grid ordinal (cut tick / segment,
	// rounded to nearest), tolerating +/-1 tick VFR jitter. The terminal
	// end-of-stream cut shares one explicit ordinal across every rep.
	termOrdinal := int((bounds[0].Cuts[len(bounds[0].Cuts)-1].Tick + cfg.SegmentTicks - 1) /
		cfg.SegmentTicks)
	cutMaps := make([]map[int]CutPoint, len(bounds))
	maxOrdinal := 0
	for i, b := range bounds {
		m := map[int]CutPoint{}
		for ci, c := range b.Cuts {
			ord := int((c.Tick + cfg.SegmentTicks/2) / cfg.SegmentTicks)
			if ci == len(b.Cuts)-1 {
				ord = termOrdinal // terminal cut: shared end ordinal
			}
			m[ord] = c
			if ord > maxOrdinal {
				maxOrdinal = ord
			}
		}
		cutMaps[i] = m
	}
	for ord := 0; ord <= maxOrdinal; ord++ {
		cb := CommonBoundary{Index: ord, GridTick: media.Ticks(ord) * cfg.SegmentTicks, PerRep: map[string]CutPoint{}}
		var lo, hi media.Ticks
		first := true
		for i, b := range bounds {
			c, ok := cutMaps[i][ord]
			if !ok {
				// Missing cut for this ordinal: this boundary cannot be
				// common; skip it but keep earlier validated boundaries.
				cb = CommonBoundary{}
				break
			}
			cb.PerRep[b.RepID] = c
			if first || c.Tick < lo {
				lo = c.Tick
			}
			if first || c.Tick > hi {
				hi = c.Tick
			}
			first = false
		}
		if len(cb.PerRep) == 0 || len(cb.PerRep) != len(bounds) {
			continue
		}
		cb.DeltaTicks = hi - lo
		if cb.DeltaTicks > res.MaxDelta {
			res.MaxDelta = cb.DeltaTicks
		}
		res.Boundaries = append(res.Boundaries, cb)
	}
	sort.SliceStable(res.Boundaries, func(i, j int) bool {
		return res.Boundaries[i].GridTick < res.Boundaries[j].GridTick
	})
	return res
}

func minTick(a, b media.Ticks) media.Ticks {
	if a < b {
		return a
	}
	return b
}
