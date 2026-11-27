// Package recovery rebuilds lost packets from the grouped XOR parity
// records embedded in a capture, after ordering and de-duplication.
package recovery

import (
	"fmt"
	"sort"

	"avrepair/internal/segment"
)

// GroupSize is the number of data packets protected by one parity record.
const GroupSize = 8

// Result summarizes one repaired capture.
type Result struct {
	Packets       []segment.Packet
	FirstSeq      uint16
	Duplicates    int
	Reordered     int
	Missing       int
	Recovered     int
	Unrecoverable []GroupLoss
	Warnings      []string
}

// GroupLoss describes a parity group that could not be fully rebuilt.
type GroupLoss struct {
	Base    uint16
	Missing []uint16
}

// Recover orders packets by sequence number, drops duplicates, then uses
// parity groups to rebuild single losses. Groups with two or more losses
// are marked unrecoverable and their surviving packets are still emitted.
func Recover(pkts []segment.Packet) Result {
	res := Result{}
	data := map[uint16][]byte{}
	parity := map[uint16][]byte{}
	var arrival []uint16
	for _, p := range pkts {
		if p.Parity {
			if _, dup := parity[p.Base]; dup {
				res.Duplicates++
				continue
			}
			parity[p.Base] = append([]byte(nil), p.Payload...)
			continue
		}
		if _, dup := data[p.Seq]; dup {
			res.Duplicates++
			continue
		}
		data[p.Seq] = append([]byte(nil), p.Payload...)
		arrival = append(arrival, p.Seq)
	}
	if len(data) == 0 {
		res.Warnings = append(res.Warnings, "capture contains no data packets")
		return res
	}
	for i := 1; i < len(arrival); i++ {
		if arrival[i] < arrival[i-1] {
			res.Reordered++
		}
	}
	bases := make([]int, 0, len(parity))
	for g := range parity {
		bases = append(bases, int(g))
	}
	sort.Ints(bases)
	for _, gi := range bases {
		g := uint16(gi)
		missing := make([]uint16, 0, GroupSize)
		var xor []byte
		for i := 0; i < GroupSize; i++ {
			s := g + uint16(i)
			p, ok := data[s]
			if !ok {
				missing = append(missing, s)
				continue
			}
			if xor == nil {
				xor = append([]byte(nil), p...)
				continue
			}
			xorInto(xor, p)
		}
		res.Missing += len(missing)
		switch {
		case len(missing) == 0:
			continue
		case len(missing) > 1 || xor == nil:
			res.Unrecoverable = append(res.Unrecoverable, GroupLoss{Base: g, Missing: missing})
			continue
		}
		rec := append([]byte(nil), parity[g]...)
		if len(rec) != segment.TSPacketLen {
			res.Warnings = append(res.Warnings, fmt.Sprintf("parity group %d has bad payload size", g))
			res.Unrecoverable = append(res.Unrecoverable, GroupLoss{Base: g, Missing: missing})
			continue
		}
		xorInto(rec, xor)
		data[missing[0]] = rec
		res.Recovered++
	}
	ordered := make([]int, 0, len(data))
	for s := range data {
		ordered = append(ordered, int(s))
	}
	sort.Ints(ordered)
	res.Packets = make([]segment.Packet, 0, len(ordered))
	for i, s := range ordered {
		if i == 0 {
			res.FirstSeq = uint16(s)
		}
		res.Packets = append(res.Packets, segment.Packet{Seq: uint16(s), Payload: data[uint16(s)]})
	}
	return res
}

func xorInto(dst, src []byte) {
	for i := 0; i < len(dst) && i < len(src); i++ {
		dst[i] ^= src[i]
	}
}

// BuildParity computes the XOR parity payload for a group of data packets.
// Short groups are zero-padded so every parity record is fixed size.
func BuildParity(base uint16, group [][]byte) []byte {
	out := make([]byte, segment.TSPacketLen)
	for _, p := range group {
		xorInto(out, p)
	}
	return out
}
