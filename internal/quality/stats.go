// Package quality aggregates frame-level validity, RTP reachability and
// container defects into headline quality statistics on one time base.
package quality

import (
	"sort"

	"mediascan/internal/fault"
	"mediascan/internal/rtp"
)

// FrameOutcome records what happened to one frame index.
type FrameOutcome struct {
	Index      int
	ChecksumOK bool
	Reachable  bool // a keyframe is reachable and RTP delivered the frame
	Lost       bool // RTP burst loss removed the frame
}

// Stats is the headline quality result.
type Stats struct {
	TotalFrames   int
	ValidFrames   int
	ValidPctX100  int // valid / total * 100, fixed point *100
	ChecksumFault int
	LostFrames    int
	Unreachable   int
	// CorruptRanges are merged media-time intervals covering every
	// checksum failure and every RTP burst loss.
	CorruptRanges []fault.Interval
	Faults        []fault.Fault
}

// Build computes quality from per-frame outcomes, checksum faults and the
// RTP reorder result. All intervals are expressed on the shared tick axis
// and merged so adjacent/overlapping defects become one corrupt range.
func Build(outcomes []FrameOutcome, checksumFaults []fault.Fault, rr *rtp.Result, mergeGap int64) (*Stats, error) {
	s := &Stats{TotalFrames: len(outcomes)}
	var intervals []fault.Interval
	for _, f := range checksumFaults {
		if f.Cat != fault.CatBadChecksum {
			continue
		}
		s.ChecksumFault++
		s.Faults = append(s.Faults, f)
		if f.Span.End > f.Span.Start {
			intervals = append(intervals, f.Span)
		}
	}
	lost := map[uint64]bool{}
	if rr != nil {
		for _, b := range rr.Bursts {
			s.Faults = append(s.Faults, fault.Fault{
				Cat: fault.CatBurstLoss, Ref: faultRef(b),
				Span: fault.Interval{Start: b.StartTick, End: b.EndTick},
				Msg:  "burst loss mapped to quality corruption range",
			})
			intervals = append(intervals, fault.Interval{Start: b.StartTick, End: b.EndTick})
			for seq := b.FromSeq; seq <= b.ToSeq; seq++ {
				lost[seq] = true
			}
		}
	}
	for _, o := range outcomes {
		switch {
		case !o.ChecksumOK:
			// counted above; frame is invalid
		case o.Lost || !o.Reachable:
			if o.Lost {
				s.LostFrames++
			} else {
				s.Unreachable++
			}
		default:
			s.ValidFrames++
		}
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].Start != intervals[j].Start {
			return intervals[i].Start < intervals[j].Start
		}
		return intervals[i].End < intervals[j].End
	})
	merged, err := fault.Merge(intervals, mergeGap)
	if err != nil {
		return nil, err
	}
	s.CorruptRanges = merged
	if s.TotalFrames > 0 {
		s.ValidPctX100 = s.ValidFrames * 10000 / s.TotalFrames
	}
	return s, nil
}

func faultRef(b rtp.Burst) string {
	return "burst"
}

// ValidPct renders the fixed-point percentage as a human percentage.
func (s Stats) ValidPct() float64 { return float64(s.ValidPctX100) / 100 }
