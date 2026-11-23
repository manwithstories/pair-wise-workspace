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
	Group        int
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

type trackState struct {
	next    uint64
	started bool
}

// Builder incrementally repairs decode times for one track of one init group,
// so callers can stream fragments without retaining the whole archive. Each
// init group is an independent program with its own decode clock.
type Builder struct {
	group     int
	track     uint32
	timescale uint32
	state     trackState
	repaired  int
	forward   int
}

// NewBuilder validates a track and returns a fresh builder for it.
func NewBuilder(group int, track *parse.Track) (*Builder, error) {
	if track == nil {
		return nil, fault.New(fault.KindTimeline, "", "trex", 0, "nil track")
	}
	if _, err := clock.New(track.Timescale); err != nil {
		return nil, fault.Wrap(err, fault.KindTimeline, "", "mdhd", 0,
			fmt.Sprintf("track %d has invalid timescale", track.ID))
	}
	return &Builder{group: group, track: track.ID, timescale: track.Timescale}, nil
}

// Seed restores the next decode time from a checkpoint.
func (b *Builder) Seed(next uint64, started bool) {
	b.state.next = next
	b.state.started = started
}

// Next returns the decode time after the last added sample and whether any
// fragment has been seen.
func (b *Builder) Next() (uint64, bool) { return b.state.next, b.state.started }

// Counts returns the number of backward and forward repairs observed.
func (b *Builder) Counts() (repaired, forward int) { return b.repaired, b.forward }

// Add repairs one parsed fragment for this builder's track and returns the
// timeline entries for that track.
func (b *Builder) Add(f *parse.Fragment) ([]FragmentTimeline, error) {
	if f == nil {
		return nil, fault.New(fault.KindTimeline, "", "moof", 0, "nil fragment")
	}
	var out []FragmentTimeline
	for _, traf := range f.Trafs {
		if traf.TrackID != b.track {
			continue
		}
		entry := FragmentTimeline{
			Group: f.Group, Segment: f.Segment, TrackID: traf.TrackID, Sequence: f.Sequence,
			OriginalBase: traf.BaseDecodeTime, RepairedBase: traf.BaseDecodeTime,
			Reason: ReasonNone, Timescale: b.timescale,
		}
		if b.state.started && traf.BaseDecodeTime < b.state.next {
			entry.RepairedBase = b.state.next
			entry.Reason = ReasonBackward
			b.repaired++
		} else if b.state.started && traf.BaseDecodeTime > b.state.next {
			entry.Reason = ReasonGap
			b.forward++
		} else if !b.state.started && traf.BaseDecodeTime != 0 {
			entry.Reason = ReasonGap
			b.forward++
		}
		cursor := entry.RepairedBase
		for i, s := range traf.Samples {
			if s.TrackID != traf.TrackID {
				return nil, fault.New(fault.KindTimeline, f.Segment, "trun", f.MoofOffset,
					fmt.Sprintf("sample track %d mismatches traf track %d", s.TrackID, traf.TrackID))
			}
			if uint64(s.Duration) > clock.MaxTicks-cursor {
				return nil, fault.New(fault.KindOverflow, f.Segment, "trun", f.MoofOffset,
					fmt.Sprintf("sample %d decode time overflows uint64", i))
			}
			stamp := SampleStamp{Sample: s, DTS: cursor, EndDTS: cursor + uint64(s.Duration)}
			if entry.Reason == ReasonBackward {
				stamp.OriginalDTS = traf.BaseDecodeTime + (cursor - entry.RepairedBase)
			} else {
				stamp.OriginalDTS = cursor
			}
			entry.Samples = append(entry.Samples, stamp)
			cursor = stamp.EndDTS
		}
		b.state.next = cursor
		b.state.started = true
		out = append(out, entry)
	}
	return out, nil
}

// Build repairs an entire already-ordered list of fragments for one track.
func Build(group int, frags []*parse.Fragment, track *parse.Track) (*Timeline, error) {
	b, err := NewBuilder(group, track)
	if err != nil {
		return nil, err
	}
	tl := &Timeline{}
	for _, f := range frags {
		entries, err := b.Add(f)
		if err != nil {
			return nil, err
		}
		tl.Fragments = append(tl.Fragments, entries...)
	}
	tl.Repaired, tl.Forward = b.Counts()
	return tl, nil
}
