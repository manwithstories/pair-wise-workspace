// Package rtp recovers synthetic RTP sequences sent over a lossy, out
// of order or duplicating channel. Recovery uses a sequence-number
// window: a packet that arrives after a later-numbered one is slotted
// back into place, duplicates are dropped, and packets that never
// arrive are reported as unrecoverable.
package rtp

import (
	"sort"

	"github.com/pion/rtp"

	"archive/internal/fault"
)

// Packet is one delivery of a packet, in channel order.
type Packet struct {
	Seq    uint16
	Arrive int // monotonically increasing delivery order
	RTP    *rtp.Packet
}

// Result summarises recovery over one injected sequence.
type Result struct {
	Sent          int     // packets in the original sequence
	Received      int     // deliveries observed, duplicates included
	Recovered     int     // late arrivals the window put back in order
	Duplicates    int     // duplicate deliveries dropped
	Unrecoverable int     // sequence numbers never delivered
	Rate          float64 // recovered / (recovered + unrecoverable), percent
	Order         []*rtp.Packet
}

// Inject describes the channel faults. The channel retransmits dropped
// packets once, after all first-pass deliveries, modelling a repair
// stream; whether those retransmissions are inside the window decides
// whether they are recoverable.
type Inject struct {
	DropEvery    int // drop one packet out of every DropEvery (0 = none)
	ReorderEvery int // swap every pair at this stride (0 = none)
	DuplicateAt  int // deliver the packet at this sequence number twice (-1 = none)
}

// Build synthesises a deterministic stream of n packets and returns the
// deliveries in channel order.
func Build(n int, inj Inject) []Packet {
	if n < 0 {
		n = 0
	}
	if n > 1<<16 {
		n = 1 << 16
	}
	var first, retrans []Packet
	arrive := 0
	var lost []uint16
	for i := 0; i < n; i++ {
		seq := uint16(i)
		if inj.DropEvery > 0 && i%inj.DropEvery == inj.DropEvery-1 {
			lost = append(lost, seq) // lost on the first pass
			continue
		}
		first = append(first, Packet{Seq: seq, Arrive: arrive, RTP: newPacket(seq)})
		arrive++
		if seq == uint16(inj.DuplicateAt) {
			first = append(first, Packet{Seq: seq, Arrive: arrive, RTP: newPacket(seq)})
			arrive++
		}
	}
	// The repair stream retransmits each lost packet exactly once, after
	// the first pass, so they arrive late and out of order.
	for _, seq := range lost {
		retrans = append(retrans, Packet{Seq: seq, Arrive: arrive, RTP: newPacket(seq)})
		arrive++
	}
	out := append(first, retrans...)
	if inj.ReorderEvery > 1 {
		for i := inj.ReorderEvery; i+1 < len(out); i += inj.ReorderEvery {
			out[i].Arrive, out[i+1].Arrive = out[i+1].Arrive, out[i].Arrive
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Arrive < out[j].Arrive })
	return out
}

func newPacket(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 3600,
			SSRC:           0x1234,
		},
		Payload: []byte{0xde, 0xad, byte(seq >> 8), byte(seq)},
	}
}

// Recover rebuilds the ordered stream from observed deliveries.
func Recover(sent int, observed []Packet) (Result, error) {
	if sent < 0 || sent > 1<<16 {
		return Result{}, fault.Newf(fault.Fatal, "rtp", "", "sent count %d out of range", sent)
	}
	res := Result{Sent: sent, Received: len(observed)}
	best := map[uint16]*rtp.Packet{}
	firstSeen := map[uint16]int{}
	for _, p := range observed {
		if int(p.Seq) >= sent {
			return Result{}, fault.Newf(fault.Fatal, "rtp", "", "sequence %d beyond sent count %d", p.Seq, sent)
		}
		if p.RTP == nil {
			return Result{}, fault.Newf(fault.Fatal, "rtp", "", "nil packet for seq %d", p.Seq)
		}
		if _, dup := best[p.Seq]; dup {
			res.Duplicates++
			continue
		}
		best[p.Seq] = p.RTP
		firstSeen[p.Seq] = p.Arrive
	}

	// A delivery is late when some later sequence number had already
	// arrived. Walk backwards tracking the earliest arrival seen above;
	// that is the exact set a sequence window restores.
	minAbove := int(^uint(0) >> 1)
	for seq := sent - 1; seq >= 0; seq-- {
		a, ok := firstSeen[uint16(seq)]
		if !ok {
			res.Unrecoverable++
			continue
		}
		if a > minAbove {
			res.Recovered++
		}
		if a < minAbove {
			minAbove = a
		}
	}
	for seq := 0; seq < sent; seq++ {
		if p, ok := best[uint16(seq)]; ok {
			res.Order = append(res.Order, p)
		}
	}
	denom := res.Recovered + res.Unrecoverable
	if denom > 0 {
		res.Rate = float64(res.Recovered) * 100 / float64(denom)
	}
	if len(res.Order) != sent-res.Unrecoverable {
		return Result{}, fault.Newf(fault.Fatal, "rtp", "", "internal: ordered %d of %d",
			len(res.Order), sent-res.Unrecoverable)
	}
	return res, nil
}
