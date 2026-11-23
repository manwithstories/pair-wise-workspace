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

type trackState struct {
	next    uint64
	started bool
}

// Builder incrementally repairs decode times, so callers can stream fragments
// without retaining the whole archive.
type Builder struct {
	init     *parse.Init
	state    map[uint32]*trackState
	repaired int
	forward  int
}

// NewBuilder validates the init segment and returns a fresh builder.
func NewBuilder(init *parse.Init) (*Builder, error) {
	if init == nil {
		return nil, fault.New(fault.KindTimeline, "", "tfdt", 0, "nil init input")
	}
	if len(init.Tracks) == 0 {
		return nil, fault.New(fault.KindMissingBox, init.Segment, "trex", 0, "no tracks declared")
	}
	b := &Builder{init: init, state: map[uint32]*trackState{}}
	for _, tr := range init.Tracks {
		if _, err := clock.New(tr.Timescale); err != nil {
			return nil, fault.Wrap(err, fault.KindTimeline, init.Segment, "mdhd", 0,
				fmt.Sprintf("track %d has invalid timescale", tr.ID))
		}
		b.state[tr.ID] = &trackState{}
	}
	return b, nil
}

// Seed restores the per-track next decode time from a checkpoint.
func (b *Builder) Seed(next map[uint32]uint64) {
	for id, v := range next {
		if st := b.state[id]; st != nil {
			st.next = v
			st.started = true
		}
	}
}

// Next returns the per-track decode time after the last added sample.
func (b *Builder) Next() map[uint32]uint64 {
	out := make(map[uint32]uint64, len(b.state))
	for id, st := range b.state {
		out[id] = st.next
	}
	return out
}

// Counts returns the number of backward and forward repairs observed.
func (b *Builder) Counts() (repaired, forward int) { return b.repaired, b.forward }

// Add repairs one parsed fragment and returns one timeline entry per traf.
func (b *Builder) Add(f *parse.Fragment) ([]FragmentTimeline, error) {
	if f == nil {
		return nil, fault.New(fault.KindTimeline, "", "moof", 0, "nil fragment")
	}
	out := make([]FragmentTimeline, 0, len(f.Trafs))
	for _, traf := range f.Trafs {
		track, ok := b.init.Tracks[traf.TrackID]
		if !ok {
			return nil, fault.New(fault.KindMissingBox, f.Segment, "trex", f.MoofOffset,
				fmt.Sprintf("fragment references undeclared track %d", traf.TrackID))
		}
		st := b.state[traf.TrackID]
		if st == nil {
			return nil, fault.New(fault.KindMissingBox, f.Segment, "trex", f.MoofOffset,
				fmt.Sprintf("track %d has no timeline state", traf.TrackID))
		}
		entry := FragmentTimeline{
			Segment: f.Segment, TrackID: traf.TrackID, Sequence: f.Sequence,
			OriginalBase: traf.BaseDecodeTime, RepairedBase: traf.BaseDecodeTime,
			Reason: ReasonNone, Timescale: track.Timescale,
		}
		if st.started && traf.BaseDecodeTime < st.next {
			entry.RepairedBase = st.next
			entry.Reason = ReasonBackward
			b.repaired++
		} else if st.started && traf.BaseDecodeTime > st.next {
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
		st.next = cursor
		st.started = true
		out = append(out, entry)
	}
	return out, nil
}

// Build repairs an entire ordered archive at once.
func Build(ord *Ordered, init *parse.Init) (*Timeline, error) {
	if ord == nil {
		return nil, fault.New(fault.KindTimeline, "", "tfdt", 0, "nil ordering")
	}
	b, err := NewBuilder(init)
	if err != nil {
		return nil, err
	}
	tl := &Timeline{}
	for _, f := range ord.Fragments {
		entries, err := b.Add(f)
		if err != nil {
			return nil, err
		}
		tl.Fragments = append(tl.Fragments, entries...)
	}
	tl.Repaired, tl.Forward = b.Counts()
	return tl, nil
}
