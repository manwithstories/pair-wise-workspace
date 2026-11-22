package merge

import (
	"medpipe/internal/clock"
	"medpipe/internal/fault"
	rtpstream "medpipe/internal/rtp"
)

// Event is one decoded, deduplicated packet placed on the timeline.
type Event struct {
	Track        uint32
	Seq          uint16 // raw RTP sequence number
	ExtSeq       uint64 // extended sequence number
	RTPTimestamp uint32
	Payload      []byte
	Hash         [32]byte // SHA-256 of the payload
	Offset       int64    // byte offset of the source record
	Shared       clock.Rat
	// MissingBefore counts packets of this track known to be missing
	// between the previous emitted event and this one.
	MissingBefore uint32
}

// Result is the output of decoding one job. Exactly one of Ev/SR/Err
// is set. Index echoes Job.Index so the driver can restore stream
// order before touching the merger.
type Result struct {
	Index uint64
	Ev    *Event
	SR    *SRUpdate
	Err   error
}

// SRUpdate carries a parsed sender report through the pool unchanged.
type SRUpdate struct {
	Track  uint32
	SR     *rtpstream.SenderReport
	Offset int64
}

// MaxReorderWindow bounds the per-track reorder window.
const MaxReorderWindow = 4096

// trackBuffer reorders one track's packets inside a finite ring window
// and deduplicates by (track, extended sequence, payload hash).
type trackBuffer struct {
	track   uint32
	window  uint64
	slots   []*Event // ring of size window, indexed by ExtSeq % window
	next    uint64   // next extended sequence to emit
	started bool
	ext     rtpstream.SeqExtender
	// recent remembers hashes of already-emitted sequences so late
	// duplicates are recognized; bounded to 2*window entries.
	recent      map[uint64][32]byte
	recentOrder []uint64
	// pendingGaps counts missing sequences skipped since the last
	// emitted event; attached to the next real event.
	pendingGaps uint32
	// primed becomes true once the window has filled for the first
	// time; until then nothing is emitted, so packets reordered at
	// stream start are not mistaken for late arrivals.
	primed bool

	Dups    int
	TooLate int
}

func newTrackBuffer(track uint32, window int) *trackBuffer {
	if window < 1 {
		window = 1
	}
	return &trackBuffer{
		track:  track,
		window: uint64(window),
		slots:  make([]*Event, window),
		recent: map[uint64][32]byte{},
	}
}

// push inserts one event and returns any events that became emit-ready
// in sequence order. Located faults are appended to fl.
func (b *trackBuffer) push(ev *Event, fl *fault.List) []*Event {
	ev.ExtSeq = b.ext.Extend(ev.Seq)
	if !b.started {
		b.started = true
		b.next = ev.ExtSeq
	}
	if ev.ExtSeq < b.next {
		if !b.primed {
			// Still priming: nothing has been emitted, so the sequence
			// base can move backwards to cover startup reordering.
			b.next = ev.ExtSeq
			return b.insert(ev, fl)
		}
		// Late arrival: exact duplicate or out-of-window packet.
		if h, ok := b.recent[ev.ExtSeq]; ok && h == ev.Hash {
			b.Dups++
			return nil
		}
		b.TooLate++
		fl.Add(fault.New(fault.KindLost, int64(b.track), int64(ev.ExtSeq),
			ev.Offset, true, "packet arrived outside reorder window"))
		return nil
	}
	var out []*Event
	if ev.ExtSeq >= b.next+b.window {
		// The window is full: sequences the new packet jumped past are
		// declared missing, keeping memory bounded.
		out = b.advanceTo(ev.ExtSeq-b.window+1, fl)
	}
	return append(out, b.insert(ev, fl)...)
}

// insert places ev in its ring slot and emits any contiguous prefix.
func (b *trackBuffer) insert(ev *Event, fl *fault.List) []*Event {
	slot := ev.ExtSeq % b.window
	if cur := b.slots[slot]; cur != nil && cur.ExtSeq == ev.ExtSeq {
		if cur.Hash == ev.Hash {
			b.Dups++
			return nil
		}
		fl.Add(fault.New(fault.KindCorrupt, int64(b.track), int64(ev.ExtSeq),
			ev.Offset, true, "conflicting payloads for one sequence number"))
		return nil
	}
	b.slots[slot] = ev
	if !b.primed {
		max, _ := b.ext.MaxExtended()
		if max-b.next+1 < b.window {
			return nil
		}
		b.primed = true
	}
	return b.drain()
}

// advanceTo resolves every sequence below target, emitting present
// events and counting the absent ones as missing.
func (b *trackBuffer) advanceTo(target uint64, fl *fault.List) []*Event {
	var out []*Event
	for b.next < target {
		if cur := b.slotAt(b.next); cur != nil {
			out = append(out, b.emit(cur))
		} else {
			fl.Add(fault.New(fault.KindLost, int64(b.track), int64(b.next),
				fault.Unknown, true, "packet missing beyond reorder window"))
			b.pendingGaps++
		}
		b.next++
	}
	return out
}

// drain emits the contiguous prefix starting at b.next.
func (b *trackBuffer) drain() []*Event {
	var out []*Event
	for {
		cur := b.slotAt(b.next)
		if cur == nil {
			break
		}
		out = append(out, b.emit(cur))
		b.next++
	}
	return out
}

// flush emits everything left at end of stream. Gaps up to the highest
// seen sequence are counted; the caller reads TailGaps afterwards.
func (b *trackBuffer) flush() []*Event {
	if !b.started {
		return nil
	}
	var out []*Event
	max, ok := b.ext.MaxExtended()
	if !ok {
		return nil
	}
	for b.next <= max {
		if cur := b.slotAt(b.next); cur != nil {
			out = append(out, b.emit(cur))
		} else {
			b.pendingGaps++
		}
		b.next++
	}
	return out
}

// slotAt returns the event waiting for extSeq, or nil.
func (b *trackBuffer) slotAt(extSeq uint64) *Event {
	cur := b.slots[extSeq%b.window]
	if cur != nil && cur.ExtSeq == extSeq {
		return cur
	}
	return nil
}

// emit removes cur from its slot, attaches gap accounting and records
func (b *trackBuffer) emit(cur *Event) *Event {
	b.slots[cur.ExtSeq%b.window] = nil
	cur.MissingBefore = b.pendingGaps
	b.pendingGaps = 0
	b.recent[cur.ExtSeq] = cur.Hash
	b.recentOrder = append(b.recentOrder, cur.ExtSeq)
	if limit := 2 * int(b.window); len(b.recentOrder) > limit {
		delete(b.recent, b.recentOrder[0])
		b.recentOrder = b.recentOrder[1:]
	}
	return cur
}

// TailGaps returns missing sequences past the last emitted event
// (packets lost at the tail inside the observed sequence range).
func (b *trackBuffer) TailGaps() uint32 { return b.pendingGaps }
