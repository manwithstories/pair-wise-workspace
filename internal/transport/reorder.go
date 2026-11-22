// Package transport receives a locally generated RTP packet sequence, removes
// duplicates and reorders it by extended sequence number.  It tolerates
// bounded jitter, marks burst loss as explicit gaps, and never fabricates
// frames to paper over missing data.
package transport

import (
	"fmt"
	"sort"

	"github.com/pion/rtp"

	"archivecheck/internal/fault"
)

// Packet is one received RTP packet plus its observed arrival ordinal.
type Packet struct {
	Seq       uint16
	ExtSeq    uint64
	Arrival   int
	Payload   []byte
	Marker    bool
	Timestamp uint32
	SSRC      uint32
}

// Gap is an explicit run of missing extended sequence numbers.
type Gap struct {
	Start uint64
	End   uint64 // inclusive
}

// Len returns the number of missing packets in the gap.
func (g Gap) Len() uint64 { return g.End - g.Start + 1 }

// Result is the outcome of deduplication and reordering.
type Result struct {
	Packets    []Packet // ordered by ExtSeq
	Gaps       []Gap
	Duplicates int
	Reordered  int
	Received   int
	FirstSeq   uint64
	LastSeq    uint64
	// Delivery lists each surviving packet's arrival ordinal, in resequenced
	// order, so callers can observe the permutation.
	Delivery []int
	// Lost counts the number of extended sequence numbers inside the recovered
	// extent that never arrived.
	Lost int
}

// Jitter is the number of out-of-order arrivals tolerated as normal jitter
// before reporting a reorder.
type Jitter int

// Reorderer deduplicates and resequences by extended sequence number.
type Reorderer struct {
	Jitter Jitter
}

// NewReorderer returns a reorderer tolerating up to jitter positions.
func NewReorderer(j Jitter) *Reorderer {
	if j < 0 {
		j = 0
	}
	return &Reorderer{Jitter: j}
}

// Process deduplicates and reorders the arrival stream.  Every gap is explicit
// in the result and reported to faults; nothing is fabricated.
func (r *Reorderer) Process(in []Packet, faults *fault.Set) Result {
	res := Result{Received: len(in)}
	if len(in) == 0 {
		return res
	}

	// Assign extended sequence numbers in arrival order.  A 16-bit difference
	// interpreted as int16 gives forward progress for normal and reordered
	// arrivals and naturally identifies duplicates at the same position.
	type arrival struct {
		ext   int64
		pack  Packet
		order int
	}
	seen := make(map[int64]int, len(in)) // ext -> first arrival ordinal
	arrivalOrder := make([]int64, 0, len(in))
	arrivals := make([]arrival, 0, len(in))
	maxExt := int64(in[0].Seq)
	maxSeq := in[0].Seq

	for idx, p := range in {
		var ext int64
		if idx == 0 {
			ext = int64(p.Seq)
		} else {
			diff := int32(int16(p.Seq - maxSeq))
			ext = maxExt + int64(diff)
			if diff > 0 {
				maxExt = ext
				maxSeq = p.Seq
			}
		}
		p.ExtSeq = uint64(ext)
		if _, dup := seen[ext]; dup {
			res.Duplicates++
			if faults != nil {
				faults.Add(fault.New(fault.ModuleTransport, fault.KindPacketDuplicate, fault.NoOffset(),
					fmt.Sprintf("sequence %d (ext %d) arrived twice; first at arrival %d", p.Seq, ext, seen[ext])))
			}
			continue
		}
		seen[ext] = idx
		arrivalOrder = append(arrivalOrder, ext)
		arrivals = append(arrivals, arrival{ext: ext, pack: p, order: idx})
	}

	if len(arrivals) == 0 {
		return res
	}
	sort.Slice(arrivals, func(i, j int) bool { return arrivals[i].ext < arrivals[j].ext })

	for _, a := range arrivals {
		res.Packets = append(res.Packets, a.pack)
		res.Delivery = append(res.Delivery, a.order)
	}
	res.FirstSeq = arrivals[0].pack.ExtSeq
	res.LastSeq = arrivals[len(arrivals)-1].pack.ExtSeq

	// Reorder count is measured on arrival order: a packet is late when a
	// later-sequenced packet has already arrived.
	maxSeen := int64(-1)
	for _, ext := range arrivalOrder {
		if ext < maxSeen {
			res.Reordered++
		} else {
			maxSeen = ext
		}
	}

	for i := 1; i < len(arrivals); i++ {
		prev, cur := arrivals[i-1].ext, arrivals[i].ext
		if cur != prev+1 {
			g := Gap{Start: uint64(prev + 1), End: uint64(cur - 1)}
			res.Gaps = append(res.Gaps, g)
			res.Lost += int(g.Len())
			if faults != nil {
				faults.Add(fault.New(fault.ModuleTransport, fault.KindPacketGap, fault.NoOffset(),
					fmt.Sprintf("burst loss of %d packet(s): extended sequence %d..%d never arrived", g.Len(), g.Start, g.End)))
			}
		}
	}
	if res.Reordered > 0 && faults != nil {
		faults.Add(fault.New(fault.ModuleTransport, fault.KindPacketReorder, fault.NoOffset(),
			fmt.Sprintf("%d packet(s) arrived out of order beyond tolerate=%d", res.Reordered, r.Jitter)))
	}
	return res
}

// LossRate returns lost / (received + lost), or zero for an empty stream.
func (r Result) LossRate() float64 {
	total := len(r.Packets) + r.Lost
	if total == 0 {
		return 0
	}
	return float64(r.Lost) / float64(total)
}

// PayloadBytes sums the payload of surviving packets in resequenced order.
func (r Result) PayloadBytes() int64 {
	var n int64
	for _, p := range r.Packets {
		n += int64(len(p.Payload))
	}
	return n
}

// Reassemble concatenates the surviving payloads in sequence order.
func (r Result) Reassemble() []byte {
	var out []byte
	for _, p := range r.Packets {
		out = append(out, p.Payload...)
	}
	return out
}

// PacketsFromRTP unmarshals raw RTP byte slices preserving arrival order.
func PacketsFromRTP(raw [][]byte) ([]Packet, error) {
	out := make([]Packet, 0, len(raw))
	for i, b := range raw {
		var p rtp.Packet
		if err := p.Unmarshal(b); err != nil {
			return nil, fmt.Errorf("packet %d: %w", i, err)
		}
		out = append(out, Packet{
			Seq:       p.SequenceNumber,
			Arrival:   i,
			Payload:   append([]byte(nil), p.Payload...),
			Marker:    p.Marker,
			Timestamp: p.Timestamp,
			SSRC:      p.SSRC,
		})
	}
	return out, nil
}

// ToRTP marshals a packet back to wire bytes.
func (p Packet) ToRTP() ([]byte, error) {
	h := rtp.Header{
		Version:        2,
		Marker:         p.Marker,
		SequenceNumber: p.Seq,
		Timestamp:      p.Timestamp,
		SSRC:           p.SSRC,
		PayloadType:    96,
	}
	pkt := &rtp.Packet{Header: h, Payload: p.Payload}
	buf, err := pkt.Marshal()
	if err != nil {
		return nil, err
	}
	return buf, nil
}
