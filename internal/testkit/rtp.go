package testkit

import (
	"math/rand/v2"

	"github.com/pion/rtp"
)

// RTPStream builds a deterministic packet stream in arrival order with
// reorder jitter, duplicates, and periodic burst loss. Of each dropped
// burst, most packets are retransmitted inside the reorder window
// (recoverable), a few arrive only after it (late), and the rest never
// arrive (unrecoverable). Retried duplicates of delivered packets are
// included and must not be counted twice.
func RTPStream(rng *rand.Rand, n, dropBurst int, ssrc uint32) [][]byte {
	if dropBurst <= 0 {
		dropBurst = 1
	}
	dropped := map[int]bool{}
	retransAt := map[int][]int{} // arrival tick -> packet indices
	for i := 50; i+dropBurst < n; i += 100 {
		for j := i; j < i+dropBurst; j++ {
			dropped[j] = true
			switch r := rng.Float64(); {
			case r < 0.80: // retransmitted soon, inside the window
				at := i + dropBurst + 20 + rng.IntN(20)
				retransAt[at] = append(retransAt[at], j)
			case r < 0.85: // retransmitted only after the window expired
				at := i + 600
				retransAt[at] = append(retransAt[at], j)
			default: // never retransmitted
			}
		}
	}
	dupAt := map[int][]int{}
	for i := 0; i < n; i += 97 { // ~1% retried duplicates
		dupAt[i+3] = append(dupAt[i+3], i)
	}
	var order []int
	for t := 0; t < n+700; t++ {
		if t < n && !dropped[t] {
			order = append(order, t)
		}
		if extra, ok := retransAt[t]; ok {
			order = append(order, extra...)
		}
		if dups, ok := dupAt[t]; ok {
			order = append(order, dups...)
		}
	}
	// Reorder jitter: swap a few adjacent arrivals (stays inside window).
	for k := 0; k+1 < len(order); k += 41 {
		order[k], order[k+1] = order[k+1], order[k]
	}
	out := make([][]byte, 0, len(order))
	for _, idx := range order {
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				SequenceNumber: uint16(idx),
				Timestamp:      uint32(idx * 1800), // 20 ms at 90 kHz
				SSRC:           ssrc,
			},
			Payload: []byte{byte(idx), byte(idx >> 8), 0x4d, 0x4d},
		}
		b, err := pkt.Marshal()
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	return out
}
