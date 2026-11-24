package timeline

import (
	"sort"
)

// KeyBoundary is one keyframe presentation time in the common basis.
type KeyBoundary struct {
	PTS90k     int64
	FragmentID string
	Sequence   uint32
}

// RepBoundaries collects keyframe boundaries for one representation.
type RepBoundaries struct {
	Representation string
	FrameDur90k    int64 // one frame tolerance in 90 kHz units
	Eligible       bool  // false when metadata policy marks it incompatible
	Boundaries     []KeyBoundary
}

// MemberHit records where one representation lands at a candidate point.
type MemberHit struct {
	Representation string
	FragmentID     string
	PTS90k         int64
	Deviation90k   int64
	Tolerance90k   int64
	WithinFrame    bool
}

// SwitchPoint is a reference keyframe boundary evaluated across all reps. The
// PTS is never moved: deviations are reported, and an over-one-frame drift
// marks the point non-switchable.
type SwitchPoint struct {
	PTS90k          int64
	Members         []MemberHit
	MaxDeviation90k int64
	Switchable      bool
	Reason          string
}

// AlignmentReport is the deterministic alignment result.
type AlignmentReport struct {
	Points        []SwitchPoint
	Aligned       int
	NotSwitchable int
}

// DefaultFrame90k is a nominal 30 fps frame at 90 kHz.
const DefaultFrame90k int64 = 3000

// nearestBoundary returns the boundary with minimal absolute PTS distance.
func nearestBoundary(bs []KeyBoundary, target int64) (KeyBoundary, int64) {
	best := bs[0]
	bestD := absDiff(bs[0].PTS90k, target)
	for _, b := range bs[1:] {
		d := absDiff(b.PTS90k, target)
		if d < bestD || (d == bestD && b.PTS90k < best.PTS90k) {
			best, bestD = b, d
		}
	}
	return best, bestD
}

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

// Align evaluates every boundary of a deterministic reference representation
// against the nearest keyframe in each other eligible representation.
func Align(reps []RepBoundaries) AlignmentReport {
	eligible := make([]RepBoundaries, 0, len(reps))
	for _, r := range reps {
		if r.Eligible && len(r.Boundaries) > 0 {
			b := append([]KeyBoundary(nil), r.Boundaries...)
			sort.SliceStable(b, func(i, j int) bool { return b[i].PTS90k < b[j].PTS90k })
			r.Boundaries = b
			eligible = append(eligible, r)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return eligible[i].Representation < eligible[j].Representation
	})

	report := AlignmentReport{}
	if len(eligible) == 0 {
		return report
	}
	ref := eligible[0]

	for _, rb := range ref.Boundaries {
		p := SwitchPoint{PTS90k: rb.PTS90k, Switchable: true}
		for _, r := range eligible {
			tol := r.FrameDur90k
			if tol <= 0 {
				tol = DefaultFrame90k
			}
			if len(r.Boundaries) == 0 {
				p.Switchable = false
				p.Reason = "representation has no keyframe boundary"
				continue
			}
			b, dev := nearestBoundary(r.Boundaries, rb.PTS90k)
			hit := MemberHit{
				Representation: r.Representation,
				FragmentID:     b.FragmentID,
				PTS90k:         b.PTS90k,
				Deviation90k:   dev,
				Tolerance90k:   tol,
				WithinFrame:    dev <= tol,
			}
			if r.Representation == ref.Representation {
				hit.WithinFrame = true
				hit.Deviation90k = 0
			}
			p.Members = append(p.Members, hit)
			if dev > p.MaxDeviation90k {
				p.MaxDeviation90k = dev
			}
			if !hit.WithinFrame {
				p.Switchable = false
				p.Reason = "keyframe deviation exceeds one frame; boundary not moved"
			}
		}
		sort.SliceStable(p.Members, func(i, j int) bool { return p.Members[i].Representation < p.Members[j].Representation })
		if p.Switchable {
			report.Aligned++
		} else {
			report.NotSwitchable++
		}
		report.Points = append(report.Points, p)
	}
	return report
}
