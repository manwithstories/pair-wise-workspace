package fixture

import (
	"crypto/sha256"

	"avpipe/internal/media"
	"avpipe/internal/transport"
)

// FaultMode selects which transport faults the demo injects.
type FaultMode string

const (
	FaultNone  FaultMode = "none"
	FaultMixed FaultMode = "mixed"
)

// BuildPackets assembles the ordered transport stream: video media packets,
// audio media packets and one XOR parity packet per eight-member block.
func BuildPackets(frames []FrameSet, audio []AudioSet) []media.Packet {
	var out []media.Packet
	for i := range frames {
		out = append(out, WrapVideo(&frames[i].Frame))
	}
	for i := range audio {
		out = append(out, WrapAudio(audio[i]))
	}
	out = append(out, buildParity(out)...)
	return out
}

// buildParity emits one parity packet per (kind, blockID) found in media.
func buildParity(mediaPackets []media.Packet) []media.Packet {
	type key struct {
		kind media.PacketKind
		bid  uint32
	}
	groups := map[key][][]byte{}
	var order []key
	var pts map[key]uint64 = map[key]uint64{}
	for _, p := range mediaPackets {
		k := key{p.Kind, p.BlockID}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
			pts[k] = p.PTS
		}
		groups[k] = append(groups[k], p.Payload)
	}
	var parity []media.Packet
	for _, k := range order {
		members := groups[k]
		if len(members) != transport.BlockSize {
			continue // partial trailing block is unprotected
		}
		payload := transport.Parity(members)
		sum := sha256.Sum256(payload)
		// Parity packets use a distinct sequence space (high bit set) so a
		// video parity and an audio parity for block N never collide with
		// media packet sequence numbers or with each other.
		paritySeq := uint32(1)<<31 | uint32(k.kind)<<24 | k.bid
		parity = append(parity, media.Packet{
			Seq: paritySeq, BlockID: k.bid, Kind: media.KindParity,
			ParityOf: k.kind, PTS: pts[k], Payload: payload, SHA256: sum[:],
		})
	}
	return parity
}

// pktKey is the (kind, seq) identity used throughout the transport layer.
func pktKey(kind media.PacketKind, seq uint32) uint64 {
	return uint64(kind)<<32 | uint64(seq)
}

// InjectFaults applies the mixed fault profile deterministically and returns
// the delivery-order packet list plus a description of what was injected.
func InjectFaults(packets []media.Packet, frames uint32, rngSeed int64) ([]media.Packet, FaultReport) {
	rep := FaultReport{}
	index := map[uint64]int{} // pktKey -> position
	for i, p := range packets {
		index[pktKey(p.Kind, p.Seq)] = i
	}
	work := append([]media.Packet(nil), packets...)
	drop := map[uint64]bool{}
	corrupt := map[uint64]bool{}

	// 1. Single recoverable loss: video block 1, slot 3 (parity stays intact).
	single := pktKey(media.KindVideo, transport.BlockSize+3)
	if pos, ok := index[single]; ok {
		rep.SingleLoss = append(rep.SingleLoss, work[pos].Seq)
		drop[single] = true
	}

	// 2. Checksum corruption on one audio packet: digest is tampered so
	// ingest rejects it; FEC then reconstructs the payload from parity.
	corr := pktKey(media.KindAudio, 2)
	if pos, ok := index[corr]; ok {
		bad := append([]byte(nil), work[pos].SHA256...)
		bad[0] ^= 0xFF
		work[pos].SHA256 = bad
		corrupt[corr] = true
		rep.Corrupt = append(rep.Corrupt, work[pos].Seq)
	}

	// 3. Burst loss: two consecutive video packets in a later block, with
	// their parity also removed, forcing gap timeline marking.
	bid := uint32(4)
	for slot := uint32(2); slot <= 3; slot++ {
		k := pktKey(media.KindVideo, bid*transport.BlockSize+slot)
		if pos, ok := index[k]; ok {
			drop[k] = true
			rep.BurstLoss = append(rep.BurstLoss, work[pos].Seq)
		}
	}
	paritySeq4 := uint32(1)<<31 | uint32(media.KindVideo)<<24 | bid
	pk := pktKey(media.KindParity, paritySeq4)
	if _, ok := index[pk]; ok {
		drop[pk] = true
	}

	var delivered []media.Packet
	for _, p := range work {
		k := pktKey(p.Kind, p.Seq)
		if drop[k] {
			continue
		}
		delivered = append(delivered, p)
		// 4. Exact duplicate retransmission of one surviving video packet.
		if p.Kind == media.KindVideo && p.Seq == 10 {
			delivered = append(delivered, p)
			rep.ExactDup = append(rep.ExactDup, p.Seq)
		}
		// 5. Conflicting duplicate: same identity, different payload bytes.
		if p.Kind == media.KindAudio && p.Seq == 6 {
			conflict := p
			conflict.Payload = append([]byte(nil), p.Payload...)
			conflict.Payload[len(conflict.Payload)-1] ^= 0x0F
			sum := sha256.Sum256(conflict.Payload)
			conflict.SHA256 = sum[:]
			delivered = append(delivered, conflict)
			rep.ConflictDup = append(rep.ConflictDup, p.Seq)
		}
	}

	// 6. Reorder: swap two adjacent delivery positions in the first half.
	_ = frames
	_ = rngSeed
	if len(delivered) > 8 {
		delivered[3], delivered[4] = delivered[4], delivered[3]
		rep.ReorderedPairs++
	}
	rep.CorruptKeys = corrupt
	return delivered, rep
}

// FaultReport records every injected condition for the terminal summary.
type FaultReport struct {
	SingleLoss     []uint32
	Corrupt        []uint32
	BurstLoss      []uint32
	ExactDup       []uint32
	ConflictDup    []uint32
	ReorderedPairs int
	CorruptKeys    map[uint64]bool
}
