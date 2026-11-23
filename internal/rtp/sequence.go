// Package rtp validates recorded RTP packet streams: sequence wrap-around,
// duplicates, reordering and bursty loss. Missing sequence ranges are surfaced
// explicitly as gaps; they are never smoothed into a continuous stream.
package rtp

import (
	"sort"

	pionrtp "github.com/pion/rtp"

	"archive.local/archive/internal/model"
)

// Packet is one recorded RTP packet. Wire is the exact marshalled RTP packet;
// Seq, Timestamp and SSRC are its parsed header fields.
type Packet struct {
	Seq       uint16
	Timestamp uint32
	SSRC      uint32
	Payload   []byte
	// Wire holds the packet as it appeared on the link, encoded by pion/rtp.
	Wire []byte
}

// ToPion converts the record into a pion/rtp packet for marshalling.
func (p Packet) ToPion() *pionrtp.Packet {
	h := pionrtp.Packet{Header: pionrtp.Header{
		Version:        2,
		PayloadType:    96,
		SequenceNumber: p.Seq,
		Timestamp:      p.Timestamp,
		SSRC:           p.SSRC,
	}}
	h.Payload = p.Payload
	return &h
}

// Marshal encodes the record into wire bytes with pion/rtp.
func (p Packet) Marshal() ([]byte, error) {
	w, err := p.ToPion().Marshal()
	if err != nil {
		return nil, &model.Error{Code: model.CodeTransportDefect, Sample: -1, OffsetBytes: -1,
			Detail: "cannot marshal RTP packet", Err: err}
	}
	return w, nil
}

// ParseWire decodes an RTP packet with pion/rtp and fills the record.
func ParseWire(wire []byte) (Packet, error) {
	var p pionrtp.Packet
	if err := p.Unmarshal(wire); err != nil {
		return Packet{}, &model.Error{Code: model.CodeTransportDefect, Sample: -1, OffsetBytes: -1,
			Detail: "malformed RTP packet", Err: err}
	}
	if p.Version != 2 {
		return Packet{}, &model.Error{Code: model.CodeTransportDefect, Sample: -1, OffsetBytes: -1,
			Detail: "RTP packet version is not 2"}
	}
	cp := make([]byte, len(p.Payload))
	copy(cp, p.Payload)
	return Packet{Seq: p.SequenceNumber, Timestamp: p.Timestamp, SSRC: p.SSRC, Payload: cp, Wire: wire}, nil
}

// Gap is a contiguous run of extended sequence numbers that never arrived.
type Gap struct {
	Start int64
	Count int64
}

// End returns the exclusive end of the gap.
func (g Gap) End() int64 { return g.Start + g.Count }

// Arrival is one accepted packet after dedupe and reordering.
type Arrival struct {
	ExtSeq    int64
	Packet    Packet
	Reordered bool
	// ArrivalOrder is the 0-based index of this packet in the wire trace.
	ArrivalOrder int
}

// Result summarises a stream.
type Result struct {
	Arrivals  []Arrival // in extended-sequence order
	Gaps      []Gap     // every missing range
	Duplicate int
	Reordered int
	Lost      int64
	// ConfirmedHigh is the highest extended sequence reachable contiguously
	// from the first observed packet. A later burst never lowers it.
	ConfirmedHigh int64
	// MaxSeq is the largest extended sequence observed.
	MaxSeq int64
	// Wraps counts how many times the 16-bit wire sequence wrapped.
	Wraps int
}

// Track incrementally validates and orders one SSRC's packets.
type Track struct {
	ssrc  uint32
	seen  map[int64]Packet
	order map[int64]int // extended sequence -> position in the wire trace
	dups  int
	count int
	// unwrap state
	maxExt  int64
	haveMax bool
	maxWire uint16
	wraps   int
}

// NewTrack creates a tracker for a single SSRC.
func NewTrack(ssrc uint32) *Track {
	return &Track{ssrc: ssrc, seen: make(map[int64]Packet, 256), order: make(map[int64]int, 256)}
}

// Push feeds one packet. It returns false when the packet duplicates a
// sequence number already accepted and was dropped.
//
// Extended numbering uses the shortest forward distance from the current
// maximum. A step of more than half the sequence space is a wrap; smaller
// negative steps are reordering and map below the maximum without moving it.
func (t *Track) Push(p Packet) bool {
	ext := t.extend(p.Seq)
	if _, dup := t.seen[ext]; dup {
		t.dups++
		return false
	}
	t.seen[ext] = p
	t.order[ext] = t.count
	t.count++
	return true
}

func (t *Track) extend(seq uint16) int64 {
	if !t.haveMax {
		t.haveMax = true
		t.maxExt = int64(seq)
		t.maxWire = seq
		return t.maxExt
	}
	delta := int64(int16(seq - t.maxWire))
	// int16 conversion gives the signed shortest modular distance from the
	// highest sequence seen. A positive step that crosses 65535 is a wrap.
	if delta > 0 && seq < t.maxWire {
		t.wraps++
	}
	candidate := t.maxExt + delta
	if candidate > t.maxExt {
		t.maxExt = candidate
		t.maxWire = seq
	}
	return candidate
}

// Count returns the number of accepted packets.
func (t *Track) Count() int { return t.count }

// Close finalises ordering, gaps and confirmed progress.
func (t *Track) Close() Result {
	res := Result{Duplicate: t.dups}
	if len(t.seen) == 0 {
		return res
	}
	keys := make([]int64, 0, len(t.seen))
	for k := range t.seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	present := make(map[int64]struct{}, len(keys))
	// The stream is anchored at the first observed packet. Reordered packets
	// can extend below it; normalise so the lowest sequence is the origin.
	for _, k := range keys {
		present[k] = struct{}{}
	}
	origin := keys[0]

	// Reordering is measured in arrival order: a packet arrived out of order
	// when a higher extended sequence had already been accepted before it.
	byArrival := make(map[int]int64, len(keys))
	for _, k := range keys {
		byArrival[t.order[k]] = k
	}
	reordered := make(map[int64]bool, len(keys))
	var runningMax int64 = -1
	var seenAny bool
	for pos := 0; pos < t.count; pos++ {
		k := byArrival[pos]
		if seenAny && k < runningMax {
			reordered[k] = true
		}
		if k > runningMax {
			runningMax = k
		}
		seenAny = true
	}
	res.Arrivals = make([]Arrival, 0, len(keys))
	for _, k := range keys {
		res.Arrivals = append(res.Arrivals, Arrival{ExtSeq: k, Packet: t.seen[k],
			Reordered: reordered[k], ArrivalOrder: t.order[k]})
	}
	for _, v := range reordered {
		if v {
			res.Reordered++
		}
	}
	res.MaxSeq = keys[len(keys)-1]
	res.Wraps = t.wraps

	for i := 1; i < len(keys); i++ {
		if d := keys[i] - keys[i-1]; d > 1 {
			g := Gap{Start: keys[i-1] + 1, Count: d - 1}
			res.Gaps = append(res.Gaps, g)
			res.Lost += g.Count
		}
	}

	// Confirmed progress: walk forward from origin while sequence numbers are
	// contiguous. The first gap stops it, so a burst cannot be silently
	// counted as delivered.
	res.ConfirmedHigh = origin
	for e := origin; ; e++ {
		if _, ok := present[e]; !ok {
			break
		}
		res.ConfirmedHigh = e
	}
	return res
}

// ErrNoPackets reports a trace that contained no packets for this stream.
func ErrNoPackets(ssrc uint32) *model.Error {
	return &model.Error{Code: model.CodeTransportDefect, Track: ssrc, Sample: -1, OffsetBytes: -1,
		Detail: "RTP stream contains no packets"}
}
