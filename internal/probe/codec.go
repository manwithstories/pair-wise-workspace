package probe

import (
	"math"
	"sort"
)

// ComputeKeyframeStats derives key-frame positions and interval quantiles
// in milliseconds for one track. A video track without an explicit sync
// sample table is treated as all-sync (every sample is a key frame), which
// matches the ISOBMFF convention that an absent stss means all samples
// are sync samples.
func ComputeKeyframeStats(t *TrackMeta) KeyframeStats {
	var keyNanos []int64
	for _, s := range t.Samples {
		if s.IsKeyframe {
			keyNanos = append(keyNanos, s.DTSNanos)
		}
	}
	st := KeyframeStats{KeyframeCount: len(keyNanos)}
	if len(keyNanos) < 2 {
		return st
	}
	prev := keyNanos[0]
	for _, ts := range keyNanos[1:] {
		d := ts - prev
		if d < 0 {
			d = 0
		}
		st.IntervalsMS = append(st.IntervalsMS, nanosToMillis(d))
		prev = ts
	}
	sort.Slice(st.IntervalsMS, func(i, j int) bool {
		return st.IntervalsMS[i] < st.IntervalsMS[j]
	})
	st.P50MS = quantileSorted(st.IntervalsMS, 0.50)
	st.P90MS = quantileSorted(st.IntervalsMS, 0.90)
	st.P95MS = quantileSorted(st.IntervalsMS, 0.95)
	st.P99MS = quantileSorted(st.IntervalsMS, 0.99)
	st.MaxMS = st.IntervalsMS[len(st.IntervalsMS)-1]
	return st
}

func nanosToMillis(nanos int64) int64 {
	return (nanos + 500_000) / 1_000_000
}

// quantileSorted returns the nearest-rank quantile of an already sorted
// slice. The deterministic nearest-rank method keeps repeated runs
// byte-for-byte identical.
func quantileSorted(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(q * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// SampleRangeNanos returns the half-open nanosecond time range covered by
// samples [first, last] (0-based, inclusive). It is the bridge used by the
// transport layer to map packet gaps onto sample damage.
func (t *TrackMeta) SampleRangeNanos(first, last int64) (startNanos, endNanos int64) {
	if first < 0 || last < first || int(last) >= len(t.Samples) {
		return 0, 0
	}
	startNanos = t.Samples[first].DTSNanos
	end := t.Samples[last]
	return startNanos, end.DTSNanos + end.DurationNanos
}

// FindSampleAt maps a nanosecond timestamp to the index of the sample
// whose [DTS, DTS+duration) contains it, or the nearest boundary sample.
func (t *TrackMeta) FindSampleAt(nanos int64) int64 {
	idx := sort.Search(len(t.Samples), func(i int) bool {
		s := t.Samples[i]
		return s.DTSNanos+s.DurationNanos > nanos
	})
	if idx >= len(t.Samples) {
		return int64(len(t.Samples)) - 1
	}
	return int64(idx)
}
