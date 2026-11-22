package fixture

import "github.com/pion/rtp"

// RTPOptions controls impairment injection in Packetize.
type RTPOptions struct {
	Duplicates bool  // duplicate every 7th packet
	Reorder    bool  // swap selected adjacent pairs
	WrapStart  bool  // start sequence near the 16-bit wrap point
	Loss       []int // chunk indices to drop entirely
}

// Packetize chunks data into RTP packets and applies the requested
// impairments in arrival order.
func Packetize(data []byte, opts RTPOptions) []*rtp.Packet {
	const mtu = 1200
	n := (len(data) + mtu - 1) / mtu
	var seq uint16 = 1000
	if opts.WrapStart {
		seq = 65534
	}

	// Deterministic arrival order: swap pairs (i, i+1) for i%5==2.
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	if opts.Reorder {
		for i := 2; i+1 < n; i += 5 {
			order[i], order[i+1] = order[i+1], order[i]
		}
	}

	lost := make(map[int]bool, len(opts.Loss))
	for _, l := range opts.Loss {
		lost[l] = true
	}

	var out []*rtp.Packet
	for _, idx := range order {
		if lost[idx] {
			continue
		}
		end := (idx + 1) * mtu
		if end > len(data) {
			end = len(data)
		}
		p := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				SequenceNumber: seq + uint16(idx),
				Timestamp:      uint32(idx * 3000),
				SSRC:           0x1F43B675,
			},
			Payload: data[idx*mtu : end],
		}
		out = append(out, p)
		if opts.Duplicates && idx%7 == 3 {
			out = append(out, p)
		}
	}
	return out
}
