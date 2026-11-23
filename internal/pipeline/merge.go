package pipeline

import (
	"sort"

	"mediascan/internal/fault"
)

// MergeFaults combines fault slices produced concurrently by independent
// subsystem scans into one deterministically ordered slice. Ordering is the
// canonical fault total order (ticks, track, offset, frame, kind), which is
// independent of worker scheduling, so repeated runs yield identical output.
func MergeFaults(partials ...[]fault.Fault) []fault.Fault {
	n := 0
	for _, p := range partials {
		n += len(p)
	}
	if n == 0 {
		return []fault.Fault{}
	}
	merged := make([]fault.Fault, 0, n)
	for _, p := range partials {
		merged = append(merged, p...)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return fault.OrderKey(merged[i], merged[j]) < 0
	})
	return merged
}

// OrderedResult is any item carrying the position needed for deterministic
// ordering on the unified timeline. Subsystem outputs adapt to it.
type OrderedResult interface {
	PositionTicks() int64
	PositionTrack() uint32
	PositionOffset() int64
}

// MergeResults orders concurrently produced results by (ticks, track,
// offset). It is generic over OrderedResult so callers keep their concrete
// types while sharing one stable, scheduling-independent ordering.
func MergeResults[T OrderedResult](partials ...[]T) []T {
	n := 0
	for _, p := range partials {
		n += len(p)
	}
	if n == 0 {
		return []T{}
	}
	merged := make([]T, 0, n)
	for _, p := range partials {
		merged = append(merged, p...)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		if a.PositionTicks() != b.PositionTicks() {
			return a.PositionTicks() < b.PositionTicks()
		}
		if a.PositionTrack() != b.PositionTrack() {
			return a.PositionTrack() < b.PositionTrack()
		}
		return a.PositionOffset() < b.PositionOffset()
	})
	return merged
}
