// Package recovery rebuilds lost packets from per-group XOR parity.
package recovery

import (
	"archive/internal/ingest"
	"archive/internal/media"
)

// GroupSize is the number of packets covered by one parity packet.
const GroupSize = 4

// XorPayloads computes the parity payload for a group: the XOR of all
// payloads, each zero-padded to the longest payload in the group.
func XorPayloads(payloads [][]byte) []byte {
	max := 0
	for _, p := range payloads {
		if len(p) > max {
			max = len(p)
		}
	}
	out := make([]byte, max)
	for _, p := range payloads {
		for i, b := range p {
			out[i] ^= b
		}
	}
	return out
}

// RecoverAll folds parity information into the packet set. present holds
// the packets that survived; missing sequence numbers become Lost
// placeholders. A group with exactly one loss is rebuilt in place
// (Recovered=true); groups with multiple losses keep their gaps and no
// later packet is shifted. Returns the full sequence plus the number of
// recovered and still-lost packets.
func RecoverAll(track int, present []*media.Packet, pars []ingest.ParityInfo) (full []*media.Packet, recovered, lost int) {
	maxSeq := int64(-1)
	for _, p := range present {
		if p.Seq > maxSeq {
			maxSeq = p.Seq
		}
	}
	for _, pi := range pars {
		if end := pi.BaseSeq + int64(pi.Count) - 1; end > maxSeq {
			maxSeq = end
		}
	}
	if maxSeq < 0 {
		return nil, 0, 0
	}
	slots := make([]*media.Packet, maxSeq+1)
	for _, p := range present {
		slots[p.Seq] = p
	}
	for i := range slots {
		if slots[i] == nil {
			slots[i] = &media.Packet{Track: track, Seq: int64(i), Kind: media.KindAudio, Lost: true}
		}
	}
	for _, pi := range pars {
		rec, l := rebuildGroup(track, slots[pi.BaseSeq:pi.BaseSeq+int64(pi.Count)], &pi)
		recovered += rec
		lost += l
	}
	// Losses outside any parity group remain gaps.
	for _, p := range slots {
		if p.Lost && !covered(p.Seq, pars) {
			lost++
		}
	}
	return slots, recovered, lost
}

func covered(seq int64, pars []ingest.ParityInfo) bool {
	for _, pi := range pars {
		if seq >= pi.BaseSeq && seq < pi.BaseSeq+int64(pi.Count) {
			return true
		}
	}
	return false
}

func rebuildGroup(track int, group []*media.Packet, pi *ingest.ParityInfo) (recovered, lost int) {
	lostIdx := -1
	for i, p := range group {
		if p.Lost {
			if lostIdx >= 0 {
				// Multiple losses in one group: keep the gap.
				for _, q := range group {
					if q.Lost {
						lost++
					}
				}
				return 0, lost
			}
			lostIdx = i
		}
	}
	if lostIdx < 0 {
		return 0, 0
	}
	buf := make([]byte, len(pi.XOR))
	copy(buf, pi.XOR)
	for i, p := range group {
		if i == lostIdx || p.Lost {
			continue
		}
		for j, b := range p.Data {
			buf[j] ^= b
		}
	}
	n := pi.Lengths[lostIdx]
	if n > len(buf) {
		n = len(buf)
	}
	group[lostIdx] = &media.Packet{
		Track: track, Seq: pi.BaseSeq + int64(lostIdx), Kind: media.KindAudio,
		PTS: pi.PTS[lostIdx], Duration: media.TicksToNS(int64(pi.Samples[lostIdx]), media.AudioRate),
		Data: append([]byte(nil), buf[:n]...), Recovered: true,
	}
	return 1, 0
}
