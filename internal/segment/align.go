package segment

import (
	"fmt"

	"presspack/internal/core"
)

// AlignmentReport summarizes cross-variant timeline agreement.
type AlignmentReport struct {
	MaxError    core.Time // largest start-time divergence between variants
	WithinFrame bool      // MaxError <= one frame
}

// SegmentStarts converts per-segment durations to cumulative start times.
func SegmentStarts(durations []core.Time) []core.Time {
	starts := make([]core.Time, len(durations))
	var acc core.Time
	for i, d := range durations {
		starts[i] = acc
		acc = acc.Add(d)
	}
	return starts
}

// CheckAlignment compares the segment start times of every variant pair.
// Variants with different segment counts produce a recoverable fault; the
// overlap region is still measured.
func CheckAlignment(variantStarts map[string][]core.Time, frame core.Time) (AlignmentReport, core.Faults) {
	var rep AlignmentReport
	var faults core.Faults
	names := make([]string, 0, len(variantStarts))
	for n := range variantStarts {
		names = append(names, n)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			a, b := variantStarts[names[i]], variantStarts[names[j]]
			n := min(len(a), len(b))
			if len(a) != len(b) {
				faults = append(faults, core.NewFault(core.FaultManifestMisalign, -1, true,
					fmt.Sprintf("variant %s has %d segments, %s has %d",
						names[i], len(a), names[j], len(b))))
			}
			for k := 0; k < n; k++ {
				d := a[k].Sub(b[k]).Ticks
				if d < 0 {
					d = -d
				}
				if d > rep.MaxError.Ticks {
					rep.MaxError = core.Time{Ticks: d}
				}
			}
		}
	}
	rep.WithinFrame = rep.MaxError.Ticks <= frame.Ticks
	return rep, faults
}

// CheckSubtitleFit verifies the subtitle timeline ends within one frame of
// the media duration.
func CheckSubtitleFit(lastCueEnd, mediaDuration, frame core.Time) (core.Time, bool) {
	d := lastCueEnd.Sub(mediaDuration).Ticks
	if d < 0 {
		d = -d
	}
	return core.Time{Ticks: d}, d <= frame.Ticks
}
