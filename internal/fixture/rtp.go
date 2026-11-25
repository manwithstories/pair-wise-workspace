package fixture

import (
	"math/rand/v2"

	"github.com/pion/rtp"
)

// RTPFrame is one access unit to packetise.
type RTPFrame struct {
	TrackID   uint32
	Timestamp uint32
	Payload   []byte
}

// RTPFaults describes the deliberate impairments of a generated stream.
type RTPFaults struct {
	// Duplicates is how many packets are sent twice.
	Duplicates int
	// Reorders is how many adjacent pairs are swapped on arrival.
	Reorders int
	// Drops is how many packets are removed entirely.
	Drops int
	// RepeatPackets, when positive, sends this many packets a second time far
	// after their original, so they cannot be mistaken for local reordering.
	RepeatPackets int
}

// RTPStream is a generated packet sequence plus the ground truth used by the
// selfcheck to assert recovery quality.
type RTPStream struct {
	Packets []rtp.Packet
	// SenderTicks are the 90 kHz sender timestamps, in send order.
	SenderTicks []uint32
	Total       int // packets before impairment
	Duplicated  int
	Reordered   int
	Dropped     int
}

// Packetize turns frames into RTP packets with pion/rtp so the wire encoding
// matches what a real receiver would parse. Each frame becomes one MJPEG
// packet marked as an access-unit boundary.
func Packetize(frames []RTPFrame, ssrc uint32, faults RTPFaults, seed uint64) RTPStream {
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	var out RTPStream
	var seq uint16 = 1000
	for _, f := range frames {
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Marker:         true,
				PayloadType:    26, // JPEG static payload type
				SequenceNumber: seq,
				Timestamp:      f.Timestamp,
				SSRC:           ssrc,
			},
			Payload: f.Payload,
		}
		out.Packets = append(out.Packets, pkt)
		out.SenderTicks = append(out.SenderTicks, f.Timestamp)
		seq++
		out.Total++
	}
	// Apply impairments deterministically.
	if faults.Drops > 0 {
		n := len(out.Packets)
		for i := 0; i < faults.Drops && n > 1; i++ {
			at := rng.IntN(n - 1)
			out.Packets = append(out.Packets[:at], out.Packets[at+1:]...)
			n--
			out.Dropped++
		}
	}
	if faults.Reorders > 0 {
		// A swap is only a useful reorder if the two packets are not adjacent
		// in arrival order; a plain adjacent swap with a window of two would
		// be indistinguishable from arrival jitter. Delay one packet by a
		// few positions so the receiver genuinely sees it out of order.
		for i := 0; i < faults.Reorders; i++ {
			dist := 3 + rng.IntN(3)
			at := rng.IntN(len(out.Packets) - dist - 1)
			pkt := out.Packets[at]
			out.Packets = append(out.Packets[:at], out.Packets[at+1:]...)
			to := at + dist
			if to > len(out.Packets) {
				to = len(out.Packets)
			}
			out.Packets = append(out.Packets, rtp.Packet{})
			copy(out.Packets[to+1:], out.Packets[to:])
			out.Packets[to] = pkt
			out.Reordered++
		}
	}
	if faults.Duplicates > 0 {
		for i := 0; i < faults.Duplicates; i++ {
			at := rng.IntN(len(out.Packets))
			dup := out.Packets[at]
			out.Packets = append(out.Packets, dup)
			out.Duplicated++
		}
	}
	if faults.RepeatPackets > 0 {
		take := faults.RepeatPackets
		if take > len(out.Packets) {
			take = len(out.Packets)
		}
		// Repeat packets appended at the end arrive after everything else.
		out.Packets = append(out.Packets, out.Packets[:take]...)
		out.Duplicated += take
	}
	return out
}

// Marshal serialises packets using pion/rtp and returns the wire buffers.
func Marshal(pkts []rtp.Packet) ([][]byte, error) {
	out := make([][]byte, len(pkts))
	for i, p := range pkts {
		buf, err := p.Marshal()
		if err != nil {
			return nil, err
		}
		out[i] = buf
	}
	return out, nil
}

// Unmarshal parses wire buffers back into packets, proving the generator and
// the recovery stage agree on the wire format.
func Unmarshal(bufs [][]byte) ([]rtp.Packet, error) {
	out := make([]rtp.Packet, 0, len(bufs))
	for _, b := range bufs {
		var p rtp.Packet
		if err := p.Unmarshal(b); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
