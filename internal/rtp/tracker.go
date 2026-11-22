package rtp

import (
	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

// streamState remembers the last accepted sequence number and the payload
// hashes seen in a bounded sliding window.
type streamState struct {
	ssrc      uint32
	lastSeq   uint16
	started   bool
	seen      map[uint16]uint64
	window    []uint16
	windowCap int
}

// Tracker performs per-stream duplicate/conflict classification and detects
// sequence gaps (loss). It is deterministic and allocation-bounded.
type Tracker struct {
	streams    map[uint32]*streamState
	log        *model.Log
	duplicates int
	conflicts  int
	gaps       []Gap
	maxWindow  int
}

// Gap describes a missing sequence interval on one stream.
type Gap struct {
	Stream uint32
	// From..To are the missing sequence numbers, inclusive, on 16-bit ring.
	From uint16
	To   uint16
	// At is the event time of the packet that revealed the gap.
	At clock.Ticks
}

// NewTracker builds a tracker. seenWindow bounds per-stream history entries so
// a long run cannot grow memory without limit.
func NewTracker(log *model.Log, seenWindow int) *Tracker {
	if seenWindow <= 0 {
		seenWindow = 4096
	}
	return &Tracker{
		streams:   make(map[uint32]*streamState),
		log:       log,
		maxWindow: seenWindow,
	}
}

func (t *Tracker) state(ssrc uint32) *streamState {
	s := t.streams[ssrc]
	if s == nil {
		s = &streamState{
			ssrc:      ssrc,
			seen:      make(map[uint16]uint64),
			windowCap: t.maxWindow,
		}
		t.streams[ssrc] = s
	}
	return s
}

// Observe classifies an event against the stream's recent history.
//
// Returns accept=false when the packet must not enter the jitter window
// (exact duplicate or same-sequence conflict). Conflicts are isolated rather
// than silently merged.
func (t *Tracker) Observe(ev *model.Event) (accept bool) {
	s := t.state(ev.Stream)
	if prior, ok := s.seen[ev.Seq]; ok {
		if prior == ev.PayloadHash {
			t.duplicates++
			t.log.Add(model.MediaError{
				Category: model.CatDuplicate,
				Stream:   ev.Stream,
				Seq:      ev.Seq,
				At:       ev.EventTime,
				Detail:   "identical payload retransmission",
				Decision: model.DecDeduplicated,
			})
			return false
		}
		t.conflicts++
		t.log.Add(model.MediaError{
			Category: model.CatConflict,
			Stream:   ev.Stream,
			Seq:      ev.Seq,
			At:       ev.EventTime,
			Detail:   "same sequence number carries a different payload",
			Decision: model.DecIsolated,
		})
		return false
	}
	if s.started {
		delta := seqDelta(s.lastSeq, ev.Seq)
		if delta > 1 {
			gap := Gap{
				Stream: ev.Stream,
				From:   s.lastSeq + 1,
				To:     ev.Seq - 1,
				At:     ev.EventTime,
			}
			t.gaps = append(t.gaps, gap)
			t.log.Add(model.MediaError{
				Category: model.CatLoss,
				Stream:   ev.Stream,
				Seq:      ev.Seq,
				At:       ev.EventTime,
				Detail:   "sequence gap indicates packet loss",
				Decision: model.DecBuffered,
			})
		}
	}
	s.seen[ev.Seq] = ev.PayloadHash
	s.window = append(s.window, ev.Seq)
	if len(s.window) > s.windowCap {
		drop := s.window[0]
		s.window = s.window[1:]
		delete(s.seen, drop)
	}
	s.lastSeq = ev.Seq
	s.started = true
	return true
}

// seqDelta returns b-a on the 16-bit ring, interpreting b as the newer value.
func seqDelta(a, b uint16) int {
	d := int(b) - int(a)
	if d < 0 {
		d += 1 << 16
	}
	return d
}

// Duplicates reports exact-duplicate packet count.
func (t *Tracker) Duplicates() int { return t.duplicates }

// Conflicts reports same-sequence/different-payload count.
func (t *Tracker) Conflicts() int { return t.conflicts }

// Gaps returns a copy of detected loss gaps.
func (t *Tracker) Gaps() []Gap {
	out := make([]Gap, len(t.gaps))
	copy(out, t.gaps)
	return out
}
