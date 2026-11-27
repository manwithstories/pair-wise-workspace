package fixture

import (
	"math/rand"

	"archive/internal/rtp"
)

// groupSize is the fixed XOR parity block width used by the fixture.
const groupSize = 4

// paritySeqBase places parity packets in a dedicated high sequence range so
// their ext-sequences can never collide with media packet sequences.
const paritySeqBase int64 = 1 << 30

// inject creates parity groups over the ideal stream and selects burst
// losses. A burst that would wipe an entire parity group (all groupSize
// members) is shortened so FEC demonstrably recovers single losses while
// genuine unrecoverable gaps remain possible elsewhere.
func (s *Stream) inject(rng *rand.Rand) (parity []*Packet, lost []int64) {
	ideal := s.IdealPackets
	lostSet := map[int64]bool{}
	cadence := s.Params.LossEvery
	if s.Params.LossBurst > 0 && cadence > 0 {
		for start := cadence - 1; start < len(ideal); start += cadence {
			burst := s.Params.LossBurst
			for b := 0; b < burst && start+b < len(ideal); b++ {
				lostSet[ideal[start+b].ExtSeq] = true
			}
		}
	}

	for i := 0; i+groupSize <= len(ideal); i += groupSize {
		members := ideal[i : i+groupSize]
		payloads := make([][]byte, groupSize)
		memberSeqs := make([]int64, groupSize)
		for j, m := range members {
			payloads[j] = m.Payload
			memberSeqs[j] = m.ExtSeq
		}
		parityPayload, ok := rtp.XORPayload(payloads)
		if !ok {
			continue
		}
		// If every member of this group was lost, parity cannot rebuild
		// anything: restore one member so the group stays recoverable and
		// keep unrecoverable losses for groups with multiple missing members
		// produced by adjacent bursts.
		missingInGroup := 0
		for _, m := range memberSeqs {
			if lostSet[m] {
				missingInGroup++
			}
		}
		if missingInGroup == groupSize {
			delete(lostSet, memberSeqs[0])
		}
		last := members[groupSize-1]
		paritySeq := paritySeqBase + int64(i/groupSize)
		parity = append(parity, &Packet{
			Datagram: marshalDatagram(s.Params, last.Seq, last.TS, parityPayload),
			Seq: last.Seq, TS: last.TS,
			ExtSeq:   paritySeq,
			Payload:  parityPayload,
			IsParity: true, Group: memberSeqs, Segment: last.Segment,
		})
	}

	for _, pk := range ideal {
		if lostSet[pk.ExtSeq] {
			lost = append(lost, pk.ExtSeq)
		}
	}

	// Build the sender's finite retransmission record. The record survives
	// only isolated loss: a packet whose parity group lost two or more
	// members marks a congestion window in which the retransmission log was
	// overrun, so those sequences have neither a parity nor a retrans source
	// and must surface as explicit gaps.
	groupMissing := map[int64]int{} // first member seq -> missing count
	for _, g := range parity {
		n := 0
		for _, m := range g.Group {
			if lostSet[m] {
				n++
			}
		}
		if len(g.Group) > 0 {
			groupMissing[g.Group[0]] = n
		}
	}
	groupOf := func(seq int64) (int64, bool) {
		base := s.Params.StartSeq
		idx := int(seq) - int(base)
		if idx < 0 {
			return 0, false
		}
		g := (idx / groupSize) * groupSize
		return int64(base) + int64(g), true
	}

	record := map[int64][]byte{}
	if cadence > 0 && s.Params.LossBurst > 0 {
		payloadBySeq := map[int64][]byte{}
		for _, pk := range ideal {
			payloadBySeq[pk.ExtSeq] = pk.Payload
		}
		for _, seq := range lost {
			gStart, ok := groupOf(seq)
			if !ok || groupMissing[gStart] >= 2 {
				continue // congestion window: retransmission log overrun
			}
			record[seq] = payloadBySeq[seq]
		}
	}
	s.RetransRecord = record
	return parity, lost
}
// buildArrival produces the actual arrival sequence: surviving ideal
// packets plus parity packets, then duplication, reordering and corruption.
func (s *Stream) buildArrival(rng *rand.Rand) []*Packet {
	lostSet := map[int64]bool{}
	for _, seq := range s.LostExtSeqs {
		lostSet[seq] = true
	}
	var live []*Packet
	for _, pk := range s.IdealPackets {
		if !lostSet[pk.ExtSeq] {
			live = append(live, pk)
		}
	}
	live = append(live, s.Parity...)

	// Duplicate every Nth surviving media packet (parity excluded).
	if s.Params.DuplicateEvery > 0 {
		var withDup []*Packet
		n := 0
		for _, pk := range live {
			withDup = append(withDup, pk)
			if !pk.IsParity {
				n++
				if n%s.Params.DuplicateEvery == 0 {
					withDup = append(withDup, pk)
				}
			}
		}
		live = withDup
	}

	// Reorder within sliding windows using the seeded RNG.
	if s.Params.ReorderWindow > 1 {
		w := s.Params.ReorderWindow
		for start := 0; start < len(live); start += w {
			end := start + w
			if end > len(live) {
				end = len(live)
			}
			window := live[start:end]
			rng.Shuffle(len(window), func(i, j int) {
				window[i], window[j] = window[j], window[i]
			})
		}
	}

	// Corrupt every Nth surviving media packet's CRC trailer.
	if s.Params.CorruptEvery > 0 {
		media := 0
		for i, pk := range live {
			if pk.IsParity {
				continue
			}
			media++
			if media%s.Params.CorruptEvery == 0 && len(pk.Payload) > rtp.TrailerLen {
				cp := clonePacket(pk)
				cp.Payload[len(cp.Payload)-1] ^= 0xFF
				live[i] = cp
			}
		}
	}
	return live
}

func clonePacket(pk *Packet) *Packet {
	cp := *pk
	cp.Datagram = append([]byte(nil), pk.Datagram...)
	cp.Payload = append([]byte(nil), pk.Payload...)
	if pk.Group != nil {
		cp.Group = append([]int64(nil), pk.Group...)
	}
	return &cp
}
