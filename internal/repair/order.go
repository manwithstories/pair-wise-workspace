// Package repair restores packet order and fixes timestamps.
package repair

import (
	"crypto/sha256"
	"fmt"

	"github.com/pion/rtp"
)

// EventKind classifies packet-order anomalies.
type EventKind int

const (
	EvLoss EventKind = iota
	EvReorder
	EvDuplicate
	EvConflict
	EvLate
)

func (k EventKind) String() string {
	switch k {
	case EvLoss:
		return "loss"
	case EvReorder:
		return "reorder"
	case EvDuplicate:
		return "duplicate"
	case EvConflict:
		return "conflict"
	case EvLate:
		return "late"
	}
	return "unknown"
}

// Event reports one anomaly located at ExtSeq on track SSRC.
type Event struct {
	Kind   EventKind
	SSRC   uint32
	ExtSeq uint64 // first affected extended sequence number
	Count  uint32
	Detail string
}

// Emitted is one packet released in sequence order.
type Emitted struct {
	ExtSeq  uint64
	Packet  *rtp.Packet
	Hash    [sha256.Size]byte
	RecIndex int
}

type bufEntry struct {
	pkt   *rtp.Packet
	hash  [sha256.Size]byte
	recIx int
}

type trackState struct {
	init     bool
	maxExt   uint64
	frontier uint64 // next extseq to emit
	buf      map[uint64]bufEntry
	recent   map[uint64][sha256.Size]byte // hashes of recently emitted
	recentQ  []uint64
}

// Reassembler restores per-track sequence order with a bounded window.
type Reassembler struct {
	window uint32
	tracks map[uint32]*trackState
}

func NewReassembler(window uint32) *Reassembler {
	if window < 8 {
		window = 8
	}
	return &Reassembler{window: window, tracks: map[uint32]*trackState{}}
}

func (r *Reassembler) track(ssrc uint32) *trackState {
	t, ok := r.tracks[ssrc]
	if !ok {
		t = &trackState{buf: map[uint64]bufEntry{}, recent: map[uint64][sha256.Size]byte{}}
		r.tracks[ssrc] = t
	}
	return t
}

// extend converts a 16-bit sequence number to the extended domain,
// tracking wraparound against the highest sequence seen so far.
func extend(t *trackState, seq uint16) uint64 {
	if !t.init {
		return uint64(seq)
	}
	max := t.maxExt
	cand := (max &^ 0xFFFF) | uint64(seq)
	if cand > max && cand-max > 0x8000 && cand >= 0x10000 {
		cand -= 0x10000 // late packet from the previous cycle
	} else if max > cand && max-cand > 0x8000 {
		cand += 0x10000 // forward wraparound
	}
	return cand
}

// Push feeds one packet; it returns packets released in order and
// any anomaly events. Conflicting duplicates never overwrite the
// packet already accepted.
func (r *Reassembler) Push(ssrc uint32, pkt *rtp.Packet, recIndex int) ([]Emitted, []Event) {
	t := r.track(ssrc)
	hash := sha256.Sum256(pkt.Payload)
	ext := extend(t, pkt.SequenceNumber)
	var events []Event

	if t.init {
		if ext < t.frontier {
			// Arrived after the window closed: duplicate of an
			// emitted packet, or a late packet already declared lost.
			if h, ok := t.recent[ext]; ok {
				if h == hash {
					events = append(events, Event{Kind: EvDuplicate, SSRC: ssrc, ExtSeq: ext, Count: 1})
				} else {
					events = append(events, Event{Kind: EvConflict, SSRC: ssrc, ExtSeq: ext, Count: 1,
						Detail: "conflicting duplicate dropped; first copy kept"})
				}
			} else {
				events = append(events, Event{Kind: EvLate, SSRC: ssrc, ExtSeq: ext, Count: 1,
					Detail: "arrived outside reorder window"})
			}
			return nil, events
		}
		if old, ok := t.buf[ext]; ok {
			if old.hash == hash {
				events = append(events, Event{Kind: EvDuplicate, SSRC: ssrc, ExtSeq: ext, Count: 1})
			} else {
				events = append(events, Event{Kind: EvConflict, SSRC: ssrc, ExtSeq: ext, Count: 1,
					Detail: "conflicting duplicate dropped; first copy kept"})
			}
			return nil, events
		}
		if ext < t.maxExt {
			events = append(events, Event{Kind: EvReorder, SSRC: ssrc, ExtSeq: ext, Count: 1})
		}
	} else {
		t.init = true
		t.frontier = ext
	}
	t.buf[ext] = bufEntry{pkt: pkt, hash: hash, recIx: recIndex}
	if !t.init || ext > t.maxExt {
		t.maxExt = ext
	}
	emitted := r.drain(t, ssrc)
	// Window overflow: declare the gap lost and jump the frontier.
	if uint32(len(t.buf)) > r.window {
		var minKey uint64 = ^uint64(0)
		for k := range t.buf {
			if k < minKey {
				minKey = k
			}
		}
		if minKey > t.frontier {
			events = append(events, Event{Kind: EvLoss, SSRC: ssrc, ExtSeq: t.frontier,
				Count: uint32(minKey - t.frontier),
				Detail: fmt.Sprintf("gap of %d packet(s) declared lost", minKey-t.frontier)})
			t.frontier = minKey
		}
		emitted = append(emitted, r.drain(t, ssrc)...)
	}
	return emitted, events
}

func (r *Reassembler) drain(t *trackState, ssrc uint32) []Emitted {
	var out []Emitted
	for {
		e, ok := t.buf[t.frontier]
		if !ok {
			break
		}
		delete(t.buf, t.frontier)
		out = append(out, Emitted{ExtSeq: t.frontier, Packet: e.pkt, Hash: e.hash, RecIndex: e.recIx})
		t.recent[t.frontier] = e.hash
		t.recentQ = append(t.recentQ, t.frontier)
		t.frontier++
	}
	// Bound the recent-emitted hash cache to the window.
	for len(t.recentQ) > int(r.window) {
		delete(t.recent, t.recentQ[0])
		t.recentQ = t.recentQ[1:]
	}
	return out
}

// Flush declares every remaining gap lost and emits what is left;
// called at end of stream. Returns events and emitted packets.
func (r *Reassembler) Flush(ssrc uint32) ([]Emitted, []Event) {
	t, ok := r.tracks[ssrc]
	if !ok || !t.init {
		return nil, nil
	}
	var events []Event
	var out []Emitted
	for len(t.buf) > 0 {
		var minKey uint64 = ^uint64(0)
		for k := range t.buf {
			if k < minKey {
				minKey = k
			}
		}
		if minKey > t.frontier {
			events = append(events, Event{Kind: EvLoss, SSRC: ssrc, ExtSeq: t.frontier,
				Count: uint32(minKey - t.frontier)})
			t.frontier = minKey
		}
		out = append(out, r.drain(t, ssrc)...)
	}
	return out, events
}

// State exports the reassembler for checkpointing.
func (r *Reassembler) State() map[uint32]TrackState {
	out := map[uint32]TrackState{}
	for ssrc, t := range r.tracks {
		st := TrackState{
			Init: t.init, MaxExt: t.maxExt, Frontier: t.frontier,
			Buf: map[uint64]BufEntry{}, Recent: map[uint64][32]byte{}, RecentQ: t.recentQ,
		}
		for k, v := range t.buf {
			raw, err := v.pkt.Marshal()
			if err == nil {
				st.Buf[k] = BufEntry{Raw: raw, Hash: v.hash, RecIndex: v.recIx}
			}
		}
		for k, v := range t.recent {
			st.Recent[k] = v
		}
		out[ssrc] = st
	}
	return out
}

// Restore loads a previously exported state.
func (r *Reassembler) Restore(states map[uint32]TrackState) error {
	for ssrc, st := range states {
		t := &trackState{
			init: st.Init, maxExt: st.MaxExt, frontier: st.Frontier,
			buf: map[uint64]bufEntry{}, recent: st.Recent, recentQ: st.RecentQ,
		}
		if t.recent == nil {
			t.recent = map[uint64][sha256.Size]byte{}
		}
		for k, v := range st.Buf {
			var p rtp.Packet
			if err := p.Unmarshal(v.Raw); err != nil {
				return fmt.Errorf("checkpoint buffer SSRC %d seq %d: %w", ssrc, k, err)
			}
			t.buf[k] = bufEntry{pkt: &p, hash: v.Hash, recIx: v.RecIndex}
		}
		r.tracks[ssrc] = t
	}
	return nil
}

// TrackState is the serializable per-track reassembler state.
type TrackState struct {
	Init     bool
	MaxExt   uint64
	Frontier uint64
	Buf      map[uint64]BufEntry
	Recent   map[uint64][32]byte
	RecentQ  []uint64
}

// BufEntry is a buffered packet in serialized form.
type BufEntry struct {
	Raw      []byte
	Hash     [32]byte
	RecIndex int
}
