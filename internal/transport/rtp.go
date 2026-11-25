// Package transport reorders, de-duplicates, and reassembles RTP packets.
package transport

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/pion/rtp"

	"mediad/internal/timeline"
)

const (
	// MaxPackets is the hard RTP reorder-cache bound.
	MaxPackets = 512
	// MaxWaitNS is the 250 ms reorder window.
	MaxWaitNS int64 = 250_000_000
)

// ErrBackpressure is returned when the bounded receiver cache is full.
var ErrBackpressure = errors.New("rtp receiver cache full")

// Config customizes the bounded receiver. Zero value uses defaults.
type Config struct {
	MaxPackets int
	MaxWaitNS  int64
}

func (c Config) withDefaults() Config {
	if c.MaxPackets <= 0 {
		c.MaxPackets = MaxPackets
	}
	if c.MaxWaitNS <= 0 {
		c.MaxWaitNS = MaxWaitNS
	}
	return c
}

// AU is a reassembled RTP access unit. Complete is false after a missing
// sequence or missing marker exceeds the reorder window.
type AU struct {
	RTPTimestamp   uint32
	Data           []byte
	Complete       bool
	MarkerReceived bool
	Sequences      []uint16
	Missing        []uint16
	FirstArrivalNS int64
}

type packetState struct {
	pkt    rtp.Packet
	extSeq int64
}

type auState struct {
	ts           uint32
	packets      map[int64]packetState
	firstArrival int64
	markerExt    *int64
}

// Receiver is a bounded, serialized-safe RTP jitter/reassembly buffer.
type Receiver struct {
	mu       sync.Mutex
	cfg      Config
	maxExt   *int64
	states   map[uint32]*auState
	seqToTS  map[int64]uint32
	buffered int

	Duplicates       int64
	BackpressureHits int64
}

func NewReceiver(cfg Config) *Receiver {
	return &Receiver{
		cfg:     cfg.withDefaults(),
		states:  make(map[uint32]*auState),
		seqToTS: make(map[int64]uint32),
	}
}

// Push inserts a packet. Duplicate packets return duplicate=true and no error.
// A full cache returns an error wrapping ErrBackpressure without dropping.
func (r *Receiver) Push(pkt *rtp.Packet, nowNS int64) (duplicate bool, err error) {
	if pkt == nil {
		return false, timeline.New(timeline.KindArgument, "transport.rtp", -1, -1, "nil packet")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	extSeq := r.unwrapSequence(pkt.SequenceNumber)
	if _, seen := r.seqToTS[extSeq]; seen {
		r.Duplicates++
		return true, nil
	}
	if r.buffered >= r.cfg.MaxPackets {
		r.BackpressureHits++
		return false, &timeline.Error{
			Kind:  timeline.KindInternal,
			Stage: "transport.rtp.backpressure",
			Index: -1, Offset: -1,
			Err: fmt.Errorf("%w: buffered=%d max=%d", ErrBackpressure, r.buffered, r.cfg.MaxPackets),
		}
	}

	st, ok := r.states[pkt.Timestamp]
	if !ok {
		st = &auState{ts: pkt.Timestamp, packets: make(map[int64]packetState), firstArrival: nowNS}
		r.states[pkt.Timestamp] = st
	}
	if nowNS < st.firstArrival || st.firstArrival == 0 {
		st.firstArrival = nowNS
	}
	st.packets[extSeq] = packetState{pkt: *pkt, extSeq: extSeq}
	r.seqToTS[extSeq] = pkt.Timestamp
	r.buffered++
	if pkt.Marker {
		seq := extSeq
		st.markerExt = &seq
	}
	return false, nil
}

func (r *Receiver) unwrapSequence(seq uint16) int64 {
	raw := int64(seq)
	if r.maxExt == nil {
		v := raw
		r.maxExt = &v
		return raw
	}
	refMod := uint16(uint64(*r.maxExt) & 0xffff)
	delta := int32(seq) - int32(refMod)
	if delta > 32767 {
		delta -= 65536
	} else if delta < -32768 {
		delta += 65536
	}
	ext := *r.maxExt + int64(delta)
	if ext > *r.maxExt {
		r.maxExt = &ext
	}
	return ext
}

// Poll returns complete AUs and expires AUs older than the reorder window.
// Complete marker-delimited AUs are returned immediately even before expiry.
func (r *Receiver) Poll(nowNS int64) []AU {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []AU
	for ts, st := range r.states {
		if au, ok := r.tryComplete(st); ok {
			out = append(out, au)
			r.deleteState(ts)
		}
	}

	// Expire oldest first when any state exceeds 250 ms.
	type oldState struct {
		ts  uint32
		age int64
	}
	var aged []oldState
	for ts, st := range r.states {
		if nowNS-st.firstArrival >= r.cfg.MaxWaitNS {
			aged = append(aged, oldState{ts, st.firstArrival})
		}
	}
	sort.Slice(aged, func(i, j int) bool { return aged[i].age < aged[j].age })
	for _, item := range aged {
		st := r.states[item.ts]
		if st == nil {
			continue
		}
		out = append(out, r.finalizeIncomplete(st))
		r.deleteState(item.ts)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].FirstArrivalNS < out[j].FirstArrivalNS })
	return out
}

func (r *Receiver) tryComplete(st *auState) (AU, bool) {
	if st.markerExt == nil || len(st.packets) == 0 {
		return AU{}, false
	}
	start, end := r.knownRange(st)
	if end != *st.markerExt {
		return AU{}, false
	}
	for seq := start; seq <= end; seq++ {
		if _, ok := st.packets[seq]; !ok {
			return AU{}, false
		}
	}
	return r.assemble(st, start, end, nil, true), true
}

func (r *Receiver) knownRange(st *auState) (int64, int64) {
	first := true
	var min, max int64
	for seq := range st.packets {
		if first || seq < min {
			min = seq
		}
		if first || seq > max {
			max = seq
		}
		first = false
	}
	if st.markerExt != nil && *st.markerExt > max {
		max = *st.markerExt
	}
	return min, max
}

func (r *Receiver) finalizeIncomplete(st *auState) AU {
	start, end := r.knownRange(st)
	var missing []uint16
	present := make(map[int64]bool, len(st.packets))
	for seq := range st.packets {
		present[seq] = true
	}
	// Only enumerate gaps through the marker when known. Without a marker,
	// gaps inside the received span prove corruption; tail loss is recorded
	// by MarkerReceived=false.
	scanEnd := end
	if st.markerExt == nil {
		maxPresent := start
		for seq := range st.packets {
			if seq > maxPresent {
				maxPresent = seq
			}
		}
		scanEnd = maxPresent
	}
	for seq := start; seq <= scanEnd; seq++ {
		if !present[seq] {
			missing = append(missing, uint16(seq))
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return r.assemble(st, start, scanEnd, missing, false)
}

func (r *Receiver) assemble(st *auState, start, end int64, missing []uint16, complete bool) AU {
	seqs := make([]int64, 0, end-start+1)
	for seq := start; seq <= end; seq++ {
		if _, ok := st.packets[seq]; ok {
			seqs = append(seqs, seq)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	data := make([]byte, 0)
	wireSeqs := make([]uint16, 0, len(seqs))
	for _, extSeq := range seqs {
		ps := st.packets[extSeq]
		wireSeqs = append(wireSeqs, ps.pkt.SequenceNumber)
		data = append(data, ps.pkt.Payload...)
	}
	marker := st.markerExt != nil
	return AU{
		RTPTimestamp:   st.ts,
		Data:           data,
		Complete:       complete && marker,
		MarkerReceived: marker,
		Sequences:      wireSeqs,
		Missing:        missing,
		FirstArrivalNS: st.firstArrival,
	}
}

func (r *Receiver) deleteState(ts uint32) {
	st := r.states[ts]
	if st == nil {
		return
	}
	for seq := range st.packets {
		delete(r.seqToTS, seq)
		r.buffered--
	}
	delete(r.states, ts)
}
