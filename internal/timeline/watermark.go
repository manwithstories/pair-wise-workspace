// Package timeline merges subtitle and media events onto one nanosecond
// timeline, commits closed segments behind an event-time watermark, and
// deterministically replays a suffix of committed segments when a late event
// arrives.
package timeline

import (
	"sort"

	"mediascan/internal/media"
	"mediascan/internal/segment"
)

// Engine accumulates events and commits segments behind a watermark.
type Engine struct {
	cfg       Config
	events    []Event
	watermark int64
	segments  []Segment
	seen      map[uint32]bool
	classSet  map[string]bool
	nextSeq   int
	// lateEvents records events that arrived after the watermark and were
	// accepted for replay.
	lateEvents []Event
	issues     media.IssueSet
	// replays records each replay's statistics; lastReplay/replayCount mirror
	// the most recent one.
	replays     []ReplayStats
	lastReplay  ReplayStats
	replayCount int
}

// NewEngine builds an engine with normalized configuration.
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:      cfg.normalize(),
		seen:     map[uint32]bool{},
		classSet: map[string]bool{},
	}
}

// Issues returns the accumulated timeline issues.
func (e *Engine) Issues() media.IssueSet { return e.issues }

// AdvanceWatermark raises the watermark to ns if it is later than the current
// value and commits every segment fully behind it.
func (e *Engine) AdvanceWatermark(ns int64) {
	if ns < 0 {
		e.issues.Add(media.NewIssue(media.ClassTimelineWatermarkTime, media.Locator{TimeNs: ns},
			"negative watermark ignored"))
		return
	}
	if ns > e.watermark {
		e.watermark = ns
	}
	e.flush()
}

// Add ingests an event. If its start is before the current watermark the event
// is late: it is recorded, a late-event issue is emitted, and a deterministic
// replay of the affected suffix is scheduled.
func (e *Engine) Add(ev Event) {
	ev.Seq = e.nextSeq
	e.nextSeq++
	e.seen[ev.TrackID] = true
	for _, c := range ev.IssueClasses {
		e.classSet[c] = true
	}
	if ev.StartNs < e.watermark {
		ev.Late = true
		e.lateEvents = append(e.lateEvents, ev)
		e.issues.Add(media.NewIssue(media.ClassTimelineLateEvent, media.Locator{
			TrackID: ev.TrackID, TimeNs: ev.StartNs,
		}, "event at %d ns arrived after watermark %d ns; replaying suffix", ev.StartNs, e.watermark))
		// The late event is part of the event set now; replay recomputes from
		// the checkpoint preceding it. Because flush is a pure recompute, the
		// result equals the on-time ordering.
		e.events = append(e.events, ev)
		e.replayFrom(e.checkpointFor(ev.StartNs))
		return
	}
	e.events = append(e.events, ev)
	e.flush()
}

// Watermark returns the current watermark.
func (e *Engine) Watermark() int64 { return e.watermark }

// flush recomputes every segment that is wholly behind the watermark from the
// full event set. Recomputing (rather than mutating an accumulator) is what
// makes the engine order-independent: the committed output is a pure function
// of the events and the watermark, so a late event produces exactly the same
// segments as if it had arrived on time.
func (e *Engine) flush() {
	sorted := SortEvents(e.events)
	assigned := make([]Segment, 0, len(sorted)/4+1)
	byIndex := map[int]int{} // segment index -> position in assigned
	for _, ev := range sorted {
		if ev.StartNs >= e.watermark {
			// Events at or after the watermark are not yet closed. Sorting by
			// start time means everything after this is also unclosed.
			break
		}
		idx := int(ev.StartNs / e.cfg.SegmentNs)
		segStart := int64(idx) * e.cfg.SegmentNs
		segEnd := segStart + e.cfg.SegmentNs
		if segEnd > e.watermark {
			// Segment straddles the watermark; wait for it to close.
			continue
		}
		pos, ok := byIndex[idx]
		if !ok {
			pos = len(assigned)
			byIndex[idx] = pos
			assigned = append(assigned, Segment{Index: idx, StartNs: segStart, EndNs: segEnd})
		}
		assigned[pos].Events = append(assigned[pos].Events, ev)
	}
	for i := range assigned {
		seg := &assigned[i]
		finalizeSegment(seg, e.cfg)
		if (i+1)%e.cfg.CheckpointEvery == 0 {
			seg.Checkpoint = true
		}
		seg.Hash = segment.SHA256(seg.HashRecord())
	}
	e.segments = assigned
}

// finalizeSegment derives the hashed facts from the segment's events.
func finalizeSegment(s *Segment, cfg Config) {
	trackSet := map[uint32]bool{}
	for _, id := range cfg.TrackIDs {
		trackSet[id] = true
	}
	classSet := map[string]bool{}
	recovery := segment.Clean
	srcSeen := map[segment.Source]bool{}
	var sources []segment.Source
	for _, ev := range s.Events {
		trackSet[ev.TrackID] = true
		for _, c := range ev.IssueClasses {
			classSet[c] = true
		}
		if ev.Recovery == segment.Failed {
			recovery = segment.Failed
		} else if ev.Recovery == segment.Recovered && recovery != segment.Failed {
			recovery = segment.Recovered
		}
		if ev.Late {
			s.Late++
			if recovery != segment.Failed {
				recovery = segment.Recovered
			}
			classSet[string(media.ClassTimelineLateEvent)] = true
		}
		if !srcSeen[ev.Source] {
			srcSeen[ev.Source] = true
			sources = append(sources, ev.Source)
		}
	}
	s.TrackIDs = sortedTracks(trackSet)
	s.Classes = sortedStrings(classSet)
	s.Sources = sources
	s.Recovery = recovery
}

func sortedTracks(set map[uint32]bool) []uint32 {
	out := make([]uint32, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedStrings(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Segments returns the committed segments in index order.
func (e *Engine) Segments() []Segment {
	out := append([]Segment(nil), e.segments...)
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// LateEvents returns the events that arrived after the watermark.
func (e *Engine) LateEvents() []Event {
	return append([]Event(nil), e.lateEvents...)
}
