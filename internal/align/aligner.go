// Package align computes a common presentation origin across renditions and
// snaps every rendition's cut points onto a shared boundary grid. All
// arithmetic is int64 in a common timescale; per-segment timescales are
// rescaled one segment at a time with overflow checks.
package align

import (
	"fmt"

	"dashpack/internal/ingest"
	"dashpack/internal/mpd"
)

// CommonTimescale is the grid clock (ticks per second) used inside the
// manifest and for cross-rendition comparison.
const CommonTimescale int64 = 90000

// AlignedRep is one rendition with its timeline snapped to the common grid.
type AlignedRep struct {
	Track ingest.Track
	// PTO is the presentationTimeOffset: the first boundary in common
	// timescale ticks.
	PTO int64
	// Cuts holds len(Segments)+1 boundary ticks in the common timescale.
	Cuts []int64
	// MaxSnapDev is the largest snap adjustment in common ticks.
	MaxSnapDev int64
}

// Result is the aligned ladder.
type Result struct {
	// Origin is the latest first-boundary across renditions (common ticks).
	Origin int64
	// DurationCommon is the longest presentation span (common ticks).
	DurationCommon int64
	Reps           []AlignedRep
}

// Align converts every probed segment boundary to the common timescale and
// snaps presentation-time boundaries to the nearest multiple of the segment
// duration. Disk-side timestamp regressions are rejected.
func Align(tracks []ingest.Track, segSec int64) (*Result, error) {
	if segSec <= 0 {
		return nil, fmt.Errorf("align: invalid segment duration %d", segSec)
	}
	grid := segSec * CommonTimescale
	res := &Result{Reps: make([]AlignedRep, len(tracks))}
	for i, tr := range tracks {
		ar, err := alignTrack(tr, grid)
		if err != nil {
			return nil, fmt.Errorf("align: %s: %w", tr.ID, err)
		}
		res.Reps[i] = ar
		if ar.PTO > res.Origin {
			res.Origin = ar.PTO
		}
		if d := ar.Cuts[len(ar.Cuts)-1] - ar.PTO; d > res.DurationCommon {
			res.DurationCommon = d
		}
	}
	return res, nil
}

func alignTrack(tr ingest.Track, grid int64) (AlignedRep, error) {
	ar := AlignedRep{Track: tr}
	n := len(tr.Segments)
	if n == 0 {
		return ar, fmt.Errorf("no segments")
	}
	var prevEnd int64
	cuts := make([]int64, n+1)
	for i, s := range tr.Segments {
		c, err := mpd.Rescale(s.PTS, s.Timescale, CommonTimescale)
		if err != nil {
			return ar, fmt.Errorf("segment %d pts: %w", i, err)
		}
		cuts[i] = c
		end, err := mpd.Rescale(s.PTS+s.Duration, s.Timescale, CommonTimescale)
		if err != nil {
			return ar, fmt.Errorf("segment %d end: %w", i, err)
		}
		if i > 0 && c < prevEnd {
			return ar, &TimestampRegression{Index: i}
		}
		prevEnd = end
	}
	last := tr.Segments[n-1]
	end, err := mpd.Rescale(last.PTS+last.Duration, last.Timescale, CommonTimescale)
	if err != nil {
		return ar, fmt.Errorf("last segment end: %w", err)
	}
	cuts[n] = end
	ar.PTO = cuts[0]
	for i := range cuts {
		pres := cuts[i] - ar.PTO
		snapped := snap(pres, grid)
		if d := abs64(snapped - pres); d > ar.MaxSnapDev {
			ar.MaxSnapDev = d
		}
		cuts[i] = snapped + ar.PTO
	}
	ar.Cuts = cuts
	return ar, nil
}

// TimestampRegression reports a segment whose start is not after the
// previous segment's start on disk.
type TimestampRegression struct{ Index int }

func (e *TimestampRegression) Error() string {
	return fmt.Sprintf("timestamp regression at segment %d", e.Index)
}

// snap rounds v to the nearest multiple of grid (half up, v >= 0).
func snap(v, grid int64) int64 {
	if v <= 0 {
		return 0
	}
	return (v + grid/2) / grid * grid
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
