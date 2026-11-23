// Package inspect derives human-meaningful structure from scanned samples:
// keyframe cadence (GOP intervals) and the damaged media ranges on the shared
// clock. It never repairs anything; it reports.
package inspect

import (
	"fmt"
	"sort"

	"mediaflow/internal/clock"
	"mediaflow/internal/cmaf"
)

// GOPStats summarizes keyframe cadence for one track of one representation.
// Interval values are in milliseconds on the unified clock.
type GOPStats struct {
	TrackID        uint32    `json:"track_id"`
	Keyframes      int       `json:"keyframes"`
	Intervals      []float64 `json:"intervals_ms"`
	MinMS          float64   `json:"gop_interval_min_ms"`
	MaxMS          float64   `json:"gop_interval_max_ms"`
	MeanMS         float64   `json:"gop_interval_mean_ms"`
	Uncertain      int       `json:"uncertain_count"`
	UncertainIndex []int     `json:"uncertain_keyframes,omitempty"`
	Missing        int       `json:"missing_keyframes"`
}

// GOPAccumulator collects keyframe observations incrementally so a 30-second,
// 30-fps stream does not need to be materialized as a slice of samples.
type GOPAccumulator struct {
	TrackID   uint32
	lastKeyUs int64
	haveLast  bool
	intervals []float64
	keyframes int
	uncertain []int
	missing   int
	nominalMS float64
}

// NewGOPAccumulator returns an accumulator for a track with an optional
// nominal keyframe interval used to flag missing markers.
func NewGOPAccumulator(trackID uint32, nominalMS float64) *GOPAccumulator {
	return &GOPAccumulator{TrackID: trackID, nominalMS: nominalMS}
}

// Observe adds one sample. keyframeKnown=false when the sync flag conflicts
// with a structural boundary (for example a flag on a zero-duration sample),
// in which case the keyframe is counted as uncertain and excluded from the
// cadence statistics. The caller decides; the accumulator never guesses.
func (g *GOPAccumulator) Observe(sampleUs int64, keyframe, keyframeKnown bool) {
	if keyframe && !keyframeKnown {
		g.uncertain = append(g.uncertain, g.keyframes)
		g.keyframes++
		return
	}
	if !keyframe {
		return
	}
	g.keyframes++
	if g.haveLast {
		delta := float64(sampleUs-g.lastKeyUs) / float64(clock.UsPerMS)
		if delta > 0 {
			g.intervals = append(g.intervals, delta)
		}
	}
	g.lastKeyUs = sampleUs
	g.haveLast = true
}

// AddMissing marks n keyframe markers whose absence was inferred from a gap
// that is an exact multiple of the nominal interval.
func (g *GOPAccumulator) AddMissing(n int) { g.missing += n }

// Result produces the statistics.
func (g *GOPAccumulator) Result() GOPStats {
	st := GOPStats{TrackID: g.TrackID, Keyframes: g.keyframes, Uncertain: len(g.uncertain), UncertainIndex: g.uncertain, Missing: g.missing}
	if len(g.intervals) > 0 {
		cp := make([]float64, len(g.intervals))
		copy(cp, g.intervals)
		sort.Float64s(cp)
		var sum float64
		for _, v := range cp {
			sum += v
		}
		st.Intervals = g.intervals
		st.MinMS = cp[0]
		st.MaxMS = cp[len(cp)-1]
		st.MeanMS = sum / float64(len(cp))
	}
	return st
}

// KeyframeIssue is one inconsistent keyframe observation, always reported
// rather than silently corrected.
type KeyframeIssue struct {
	TrackID uint32          `json:"track_id"`
	Sample  int             `json:"sample_index"`
	AtUs    int64           `json:"at_us"`
	Offset  uint64          `json:"offset"`
	Reason  string          `json:"reason"`
	Kind    clock.ErrorKind `json:"kind"`
}

// DetectKeyframeIssues inspects samples for structural contradictions: a sync
// sample with zero duration, a sample whose container range is truncated, or
// a per-sample sync flag that disagrees with the fragment's first-sample flag.
// Conflicting samples are returned; callers feed keyframeKnown=false to the
// accumulator so the cadence statistics are never silently repaired.
func DetectKeyframeIssues(trackID uint32, s cmaf.Sample) *KeyframeIssue {
	var reason string
	switch {
	case s.IsKeyframe && s.DurationTicks == 0:
		reason = "sync sample declares zero duration"
	case s.FlagsConflict:
		reason = "first-sample flag disagrees with per-sample sync flag"
	case s.Truncated:
		reason = "sample payload truncated"
	default:
		return nil
	}
	return &KeyframeIssue{
		TrackID: trackID, Sample: s.Index, AtUs: s.DecodeTimeUs, Offset: s.Offset,
		Reason: reason, Kind: clock.ErrKeyframeConflict,
	}
}

// ConflictFromFlags reports a disagreement between the fragment's
// first-sample sync flag and the per-sample sync flag for the same sample.
func ConflictFromFlags(trackID uint32, s cmaf.Sample, firstSampleSync, perSampleSync bool) *KeyframeIssue {
	if firstSampleSync == perSampleSync {
		return nil
	}
	return &KeyframeIssue{
		TrackID: trackID, Sample: s.Index, AtUs: s.DecodeTimeUs, Offset: s.Offset,
		Reason: fmt.Sprintf("first-sample sync=%v but per-sample sync=%v", firstSampleSync, perSampleSync),
		Kind:   clock.ErrKeyframeConflict,
	}
}
