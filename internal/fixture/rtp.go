package fixture

import (
	rtpwire "tsqc/internal/rtp"
	"tsqc/internal/ts"
)

// toRTP fragments the TS into whole-packet payloads, protects every group
// of four with XOR parity, then applies sequence faults in a stable order:
// a protected single loss, a three-packet burst, a duplicate and pairwise
// reordering. Keyframe-bearing packets are never dropped so segment anchors
// survive every fault set.
func (b *builder) toRTP(raw []byte, payloadSize int, faults map[string]bool) ([]RTPPacket, []FECGroup) {
	var pkts []RTPPacket
	var groups []FECGroup
	seq := uint16(1000)
	groupSize := 4
	var group []RTPPacket
	for off := 0; off < len(raw); off += payloadSize {
		end := off + payloadSize
		if end > len(raw) {
			end = len(raw)
		}
		key := false
		base := off / ts.PacketSize
		for i := base; i*ts.PacketSize < end && i < len(b.pktKey); i++ {
			if b.pktKey[i] {
				key = true
			}
		}
		p := RTPPacket{Seq: seq, Payload: append([]byte(nil), raw[off:end]...), Key: key,
			Marker: end == len(raw)}
		wire, err := rtpwire.EncodeWire(rtpwire.Packet{Seq: p.Seq, Payload: p.Payload, Marker: p.Marker})
		if err == nil {
			p.Raw = wire
		}
		seq++
		pkts = append(pkts, p)
		group = append(group, p)
		if len(group) == groupSize {
			lengths := make([]int, len(group))
			payloads := make([][]byte, len(group))
			for i := range group {
				lengths[i] = len(group[i].Payload)
				payloads[i] = group[i].Payload
			}
			parity, _ := xorParity(payloads)
			groups = append(groups, FECGroup{Base: group[0].Seq, Count: len(group), Lengths: lengths, Parity: parity})
			group = group[:0]
		}
	}

	drop := map[uint16]bool{}
	keyAt := map[uint16]bool{}
	for _, p := range pkts {
		keyAt[p.Seq] = p.Key
	}
	// A three-packet burst gives one unrecoverable interval and is placed on
	// non-key payloads so every IDR still anchors a segment.
	if faults[FaultBurst] && len(pkts) > 24 {
		for i := len(pkts) / 3; i+2 < len(pkts); i++ {
			a, b2, c := pkts[i], pkts[i+1], pkts[i+2]
			if keyAt[a.Seq] || keyAt[b2.Seq] || keyAt[c.Seq] {
				continue
			}
			drop[a.Seq], drop[b2.Seq], drop[c.Seq] = true, true, true
			break
		}
	}
	// One protected single-packet loss proves XOR repair. It must sit in a
	// parity group whose other three members all survive.
	if len(groups) > 0 {
		for gi := len(groups) - 1; gi >= 0; gi-- {
			g := groups[gi]
			ok := true
			for j := 0; j < g.Count; j++ {
				sq := g.Base + uint16(j)
				if keyAt[sq] || drop[sq] {
					ok = false
					break
				}
			}
			if ok {
				drop[g.Base+2] = true
				break
			}
		}
	}
	var kept []RTPPacket
	for _, p := range pkts {
		if drop[p.Seq] {
			continue
		}
		kept = append(kept, p)
	}
	if faults[FaultDuplicate] && len(kept) > 10 {
		at := len(kept) / 4
		dup := kept[at]
		kept = append(kept[:at+1], append([]RTPPacket{dup}, kept[at+1:]...)...)
	}
	if faults[FaultReorder] {
		// Swap pairs, skipping any pair where the second is missing due to a
		// drop: the swap is between packets that are both still present.
		for i := 0; i+1 < len(kept); i += 9 {
			if kept[i].Key || kept[i+1].Key {
				continue
			}
			kept[i], kept[i+1] = kept[i+1], kept[i]
		}
	}
	return kept, groups
}

func xorParity(payloads [][]byte) ([]byte, int) {
	max := 0
	for _, p := range payloads {
		if len(p) > max {
			max = len(p)
		}
	}
	out := make([]byte, max)
	for _, p := range payloads {
		for i, v := range p {
			out[i] ^= v
		}
	}
	return out, max
}
