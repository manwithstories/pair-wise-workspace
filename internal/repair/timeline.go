package repair

import (
	"fmt"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
	"archivecheck/internal/parse"
)

// Reason explains why a decode time was rewritten.
type Reason string

const (
	ReasonNone     Reason = "none"
	ReasonBackward Reason = "tfdt_backward"
	ReasonGap      Reason = "tfdt_forward_gap"
)

// SampleStamp is a sample annotated with its repaired decode timestamp.
type SampleStamp struct {
	parse.Sample
	DTS    uint64
	EndDTS uint64
	// OriginalDTS keeps the value implied by the untouched tfdt.
	OriginalDTS uint64
}

// FragmentTimeline is the repaired timeline for one fragment/track pair.
type FragmentTimeline struct {
	Segment      string
	TrackID      uint32
	Sequence     uint32
	OriginalBase uint64
	RepairedBase uint64
	Reason       Reason
	Timescale    uint32
	Samples      []SampleStamp
}

// Timeline is the repaired timeline for a whole archive.
type Timeline struct {
	Fragments []FragmentTimeline
	Repaired  int
	Forward   int
}

// Build repairs tfdt regressions per track using integer tick arithmetic in
// each track's own time base. Original values and the repair reason are kept.
func Build(ord *Ordered, init *parse.Init) (*Timeline, error) {
	if ord == nil || init == nil {
		return nil, fault.New(fault.KindTimeline, "", "tfdt", 0, "nil ordering or init input")
	}
	if len(init.Tracks) == 0 {
		return nil, fault.New(fault.KindMissingBox, init.Segment, "trex", 0, "no tracks declared")
	}
	tl := &Timeline{}
	type trackState struct {
		next    uint64
		started bool
	}
	states := map[uint32]*trackState{}
	for _, tr := range init.Tracks {
		if _, err := clock.New(tr.Timescale); err != nil {
			return nil, fault.Wrap(err, fault.KindTimeline, init.Segment, "mdhd", 0,
				fmt.Sprintf("track %d has invalid timescale", tr.ID))
		}
		states[tr.ID] = &trackState{}
	}
	for _, f := range ord.Fragments {
		for _, traf := range f.Trafs {
			track, ok := init.Tracks[traf.TrackID]
			if !ok {
				return nil, fault.New(fault.KindMissingBox, f.Segment, "trex", f.MoofOffset,
					fmt.Sprintf("fragment references undeclared track %d", traf.TrackID))
			}
			st := states[traf.TrackID]
			entry := FragmentTimeline{
				Segment: f.Segment, TrackID: traf.TrackID, Sequence: f.Sequence,
				OriginalBase: traf.BaseDecodeTime, RepairedBase: traf.BaseDecodeTime,
				Reason: ReasonNone, Timescale: track.Timescale,
			}
			if st.started && traf.BaseDecodeTime < st.next {
				entry.RepairedBase = st.next
				entry.Reason = ReasonBackward
				tl.Repaired++
			} else if st.started && traf.BaseDecodeTime > st.next {
				entry.Reason = ReasonGap
				tl.Forward++
			}
			cursor := entry.RepairedBase
			orig := traf.BaseDecodeTime
			if cursor < orig {
				orig = cursor
			}
			for _, s := range traf.Samples {
				if s.TrackID != traf.TrackID {
					return nil, fault.New(fault.KindTimeline, f.Segment, "trun", f.MoofOffset,
						fmt.Sprintf("sample track %d mismatches traf track %d", s.TrackID, traf.TrackID))
				}
				if cursor < entry.RepairedBase {
					return nil, fault.New(fault.KindOverflow, f.Segment, "trun", f.MoofOffset,
						"samples exceed uint64 tick range")
				}
				if cursor > clock.MaxTicks-uint64(s.Duration) {
					return nil, fault.New(fault.KindOverflow, f.Segment, "trun", f.MoofOffset,
						"sample decode time overflows uint64")
				}
				stamp := SampleStamp{Sample: s, DTS: cursor, EndDTS: cursor + uint64(s.Duration)}
				if entry.Reason == ReasonBackward {
					stamp.OriginalDTS = orig + (cursor - entry.RepairedBase)
				} else {
					stamp.OriginalDTS = cursor
				}
				entry.Samples = append(entry.Samples, stamp)
				cursor = stamp.EndDTS
			}
			st.next = cursor
			st.started = true
			tl.Fragments = append(tl.Fragments, entry)
		}
	}
	return tl, nil
}
