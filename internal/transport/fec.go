package transport

import (
	"avpipe/internal/media"
)

// FEC block geometry. Every eight media packets form one block and are
// protected by a single XOR parity packet.
const (
	BlockSize     = 8
	WrappedHdrLen = 17 // kind(1) + seq(4) + pts(8) + length(4)
)

// FecStats summarises recovery behaviour.
type FecStats struct {
	Blocks          int
	Recovered       int // packets reconstructed from parity
	BurstLossBlocks int // blocks with >=2 missing media packets
	GapSlots        []GapSlot
}

// GapSlot describes one unrecovered media packet position.
type GapSlot struct {
	BlockID uint32
	Slot    int
	Kind    media.PacketKind
	PTS     uint64 // inferred timeline position
}

// WrapPayload prepends the self-describing header that lets XOR recovery
// reconstruct the exact original length, kind and timestamp.
func WrapPayload(kind media.PacketKind, seq uint32, pts uint64, content []byte) []byte {
	out := make([]byte, WrappedHdrLen+len(content))
	out[0] = byte(kind)
	putU32(out[1:5], seq)
	putU64(out[5:13], pts)
	putU32(out[13:17], uint32(len(content)))
	copy(out[WrappedHdrLen:], content)
	return out
}

// UnwrapPayload parses a (possibly recovered) wrapped payload.
func UnwrapPayload(wrapped []byte) (kind media.PacketKind, seq uint32, pts uint64, content []byte, ok bool) {
	if len(wrapped) < WrappedHdrLen {
		return 0, 0, 0, nil, false
	}
	length := u32(wrapped[13:17])
	if uint32(len(wrapped)-WrappedHdrLen) < length {
		return 0, 0, 0, nil, false
	}
	return media.PacketKind(wrapped[0]), u32(wrapped[1:5]), u64(wrapped[5:13]),
		wrapped[WrappedHdrLen : WrappedHdrLen+length], true
}

// Parity computes the XOR parity packet payload for one block of wrapped
// media payloads, zero-extending shorter members to the block maximum.
func Parity(members [][]byte) []byte {
	n := media.ParitySize(members)
	out := make([]byte, n)
	for _, m := range members {
		for i := 0; i < len(m); i++ {
			out[i] ^= m[i]
		}
	}
	return out
}

type fecBlock struct {
	slots    [BlockSize][]byte // wrapped payload per slot, nil when missing
	pts      [BlockSize]uint64
	kind     [BlockSize]media.PacketKind
	parity   []byte
	observed int // media slots ever seen, including corrupt ones
	maxSlot  int // highest slot index that ever held a member
}

// blockKey identifies one FEC block: video and audio streams are numbered
// independently, so kind discriminates blocks sharing an ID.
func blockKey(kind media.PacketKind, blockID uint32) uint64 {
	return uint64(kind)<<32 | uint64(blockID)
}

// blockKind returns the media kind carried by a block from any surviving
// slot, falling back to video for fully empty blocks.
func (b *fecBlock) blockKind() media.PacketKind {
	for s := 0; s < BlockSize; s++ {
		if b.slots[s] != nil {
			return b.kind[s]
		}
	}
	return media.KindVideo
}

// DecodeFEC groups packets into blocks, recovers single losses via XOR and
// marks multi-packet burst losses as gap slots. Output packets keep their
// original transport sequence; recovered ones set Recovered.
func DecodeFEC(packets []*media.Packet) ([]*media.Packet, FecStats, []*media.Error) {
	blocks := map[uint64]*fecBlock{}
	var order []uint64
	var out []*media.Packet
	var errs []*media.Error
	stats := FecStats{}

	for _, p := range packets {
		bid := p.BlockID
		// Parity packets explicitly carry the protected media stream kind
		// in ParityOf, persisted by the container, so video and audio
		// blocks sharing an ID never merge.
		groupKind := p.Kind
		if p.Kind == media.KindParity {
			groupKind = p.ParityOf
		}
		k := blockKey(groupKind, bid)
		b, ok := blocks[k]
		if !ok {
			b = &fecBlock{}
			blocks[k] = b
			order = append(order, k)
		}
		if p.Kind == media.KindParity {
			b.parity = p.Payload
			continue
		}
		if p.Corrupt {
			// A corrupt member occupies its slot but contributes no bytes,
			// so parity can reconstruct it. It is not passed downstream.
			slot := int(p.Seq % BlockSize)
			b.pts[slot] = p.PTS
			b.kind[slot] = p.Kind
			b.observed++
			if slot > b.maxSlot {
				b.maxSlot = slot
			}
			continue
		}
		slot := int(p.Seq % BlockSize)
		b.slots[slot] = p.Payload
		b.pts[slot] = p.PTS
		b.kind[slot] = p.Kind
		b.observed++
		if slot > b.maxSlot {
			b.maxSlot = slot
		}
		out = append(out, p)
	}

	for _, k := range order {
		bid := uint32(k)
		b := blocks[k]
		stats.Blocks++
		missing := missingSlots(b)
		if len(missing) == 0 {
			continue
		}
		if len(missing) == 1 && len(b.parity) > 0 {
			slot := missing[0]
			members := make([][]byte, 0, BlockSize)
			for s := 0; s < BlockSize; s++ {
				if s != slot {
					members = append(members, b.slots[s])
				}
			}
			rec := xorWithParity(b.parity, members)
			kind, seq, pts, _, ok := UnwrapPayload(rec)
			if !ok {
				errs = append(errs, media.NewError(media.ClassLoss, "transport",
					"FEC reconstruction failed for block "+u64toa(k), 0, false))
				stats.BurstLossBlocks++
				stats.GapSlots = append(stats.GapSlots, GapSlot{bid, slot, b.blockKind(), inferPTS(b, slot)})
				continue
			}
			wantSeq := bid*BlockSize + uint32(slot)
			if seq != wantSeq {
				errs = append(errs, media.NewError(media.ClassLoss, "transport",
					"recovered sequence mismatch in block "+u64toa(k), pts, false))
				stats.BurstLossBlocks++
				stats.GapSlots = append(stats.GapSlots, GapSlot{bid, slot, kind, inferPTS(b, slot)})
				continue
			}
			out = append(out, &media.Packet{
				Seq: bid*BlockSize + uint32(slot), BlockID: bid, Kind: kind,
				PTS: pts, Payload: rec, Recovered: true,
			})
			stats.Recovered++
			continue
		}
		// Source-end padding: a trailing block that never had eight members.
		// Its empty slots are all above the highest slot that ever held a
		// member and form a contiguous tail; they are not transport loss,
		// carry no parity, and must not produce errors or gap statistics.
		if len(b.parity) == 0 && isTrailingPadding(b, missing) {
			continue
		}
		// Zero parity or multiple losses: emit a continuous gap timeline.
		stats.BurstLossBlocks++
		for _, slot := range missing {
			errs = append(errs, media.NewError(media.ClassLoss, "transport",
				"unrecoverable loss in block "+u64toa(k)+" slot "+itoa(slot),
				inferPTS(b, slot), true))
			stats.GapSlots = append(stats.GapSlots, GapSlot{bid, slot, b.blockKind(), inferPTS(b, slot)})
		}
	}
	return out, stats, errs
}

func missingSlots(b *fecBlock) []int {
	var miss []int
	for s := 0; s < BlockSize; s++ {
		if b.slots[s] == nil {
			miss = append(miss, s)
		}
	}
	return miss
}

// isTrailingPadding reports whether every missing slot lies above the highest
// slot that ever held a member, i.e. the block simply ended early at source.
func isTrailingPadding(b *fecBlock, missing []int) bool {
	for _, s := range missing {
		if s <= b.maxSlot {
			return false
		}
	}
	return len(missing) > 0
}

// xorWithParity recovers the missing member by XORing parity with every
// surviving member; shorter members contribute implicit zero bytes.
func xorWithParity(parity []byte, members [][]byte) []byte {
	out := make([]byte, len(parity))
	copy(out, parity)
	for _, m := range members {
		for i := 0; i < len(m) && i < len(out); i++ {
			out[i] ^= m[i]
		}
	}
	return out
}

