// Package rtprecover reconstructs an ordered sample stream from RTP packets.
// Duplicates are counted once, sequence wrap is handled modulo 2^16, and gaps
// become explicit incomplete ranges rather than fabricated samples.
package rtprecover

import (
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/pion/rtp"

	"mediasync/internal/model"
)

// maxPacketBytes is the largest RTP packet accepted (well under 64 MiB).
const maxPacketBytes = 1 << 20

// Packet is a recovered packet in sequence order.
type Packet struct {
	Seq       uint16
	Timestamp uint32
	SSRC      uint32
	Payload   []byte
	Marker    bool
	// Duplicate is set when an identical (SSRC, seq, timestamp) packet was
	// already counted; duplicates contribute no sample.
	Duplicate bool
}

// Gap is an explicit incomplete range where packets never arrived.
type Gap struct {
	SSRC    uint32
	FromSeq uint16
	ToSeq   uint16
	// ExtFrom/ExtTo are the same range in the unwrapped sequence space, which
	// maps 1:1 to recovered-sample indices.
	ExtFrom  uint32
	ExtTo    uint32
	Missing  int
	FromTime uint32
	ToTime   uint32
}

// Result is the outcome of recovery for one or more SSRCs.
type Result struct {
	SSRC      uint32
	Packets   []Packet
	Gaps      []Gap
	Received  int
	Duplicate int
	// OutOfOrder counts packets whose sequence number was below the highest
	// already seen for their SSRC, in arrival order.
	OutOfOrder int
	// TimestampConflicts counts duplicate sequence numbers whose RTP timestamp
	// differed from the first copy.
	TimestampConflicts int
}

// DecodeStream reads the length-prefixed packet file the demo and tests use.
func DecodeStream(data []byte) ([][]byte, error) {
	var out [][]byte
	pos := 0
	for pos < len(data) {
		if len(data)-pos < 4 {
			return nil, model.NewError(model.ErrParamInvalid, 0, int64(pos), "RTP record header truncated")
		}
		n := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		pos += 4
		if n < 12 || n > maxPacketBytes {
			return nil, model.NewError(model.ErrSizeLimit, 0, int64(pos-4), "RTP packet length %d out of range", n)
		}
		if n > len(data)-pos {
			return nil, model.NewError(model.ErrParamInvalid, 0, int64(pos), "RTP record declares %d bytes, %d remain", n, len(data)-pos)
		}
		pkt := make([]byte, n)
		copy(pkt, data[pos:pos+n])
		out = append(out, pkt)
		pos += n
	}
	return out, nil
}

// Recover parses and orders packets. It returns one Result per SSRC, sorted by
// SSRC for determinism.
func Recover(rawPackets [][]byte) ([]Result, error) {
	bySSRC := map[uint32][]*rtp.Packet{}
	for i, raw := range rawPackets {
		if len(raw) > maxPacketBytes {
			return nil, model.NewError(model.ErrSizeLimit, 0, int64(i), "RTP packet %d is %d bytes", i, len(raw))
		}
		var p rtp.Packet
		if err := p.Unmarshal(raw); err != nil {
			return nil, model.NewError(model.ErrParamInvalid, 0, int64(i), "RTP packet %d: %v", i, err)
		}
		cp := p
		cp.Payload = append([]byte(nil), p.Payload...)
		bySSRC[p.SSRC] = append(bySSRC[p.SSRC], &cp)
	}
	ssrcs := make([]uint32, 0, len(bySSRC))
	for s := range bySSRC {
		ssrcs = append(ssrcs, s)
	}
	sort.Slice(ssrcs, func(i, j int) bool { return ssrcs[i] < ssrcs[j] })

	results := make([]Result, 0, len(ssrcs))
	for _, s := range ssrcs {
		res, err := recoverOne(s, bySSRC[s])
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

// recoverOne orders one SSRC's packets, unwrapping 16-bit sequence numbers into
// an extended sequence space. The reference is the circular minimum sequence,
// which is robust to reordering within half the sequence space.
func recoverOne(ssrc uint32, pkts []*rtp.Packet) (Result, error) {
	res := Result{SSRC: ssrc, Received: len(pkts)}
	if len(pkts) == 0 {
		return res, nil
	}
	base := findBase(pkts)
	type ext struct {
		extSeq uint32
		pkt    *rtp.Packet
		orig   int
	}
	ordered := make([]ext, 0, len(pkts))
	for i, p := range pkts {
		d := int16(p.SequenceNumber - base)
		ordered = append(ordered, ext{extSeq: uint32(uint16(d)), pkt: p, orig: i})
	}
	// Count out-of-order arrivals against the highest extended sequence seen so
	// far in wire order, before sorting.
	seenArrival := make(map[uint32]bool, len(ordered))
	highArrival := uint32(0)
	haveArrival := false
	for _, e := range ordered {
		if haveArrival && e.extSeq != highArrival && seenArrival[e.extSeq] {
			continue
		}
		if haveArrival && e.extSeq < highArrival && !seenArrival[e.extSeq] {
			res.OutOfOrder++
		}
		if !haveArrival || e.extSeq > highArrival {
			highArrival = e.extSeq
			haveArrival = true
		}
		seenArrival[e.extSeq] = true
	}

	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].extSeq < ordered[j].extSeq })

	seen := make(map[uint32]*rtp.Packet, len(ordered))
	first := true
	var minExt, maxExt uint32
	for _, e := range ordered {
		if prev, ok := seen[e.extSeq]; ok {
			res.Duplicate++
			// A repeated sequence number with a different timestamp is a
			// conflict, not a clean duplicate: the sender's clock disagrees
			// with its sequencing. Count it once but flag it rather than
			// silently trusting either copy.
			if prev.Timestamp != e.pkt.Timestamp {
				res.TimestampConflicts++
			}
			res.Packets = append(res.Packets, Packet{
				Seq: e.pkt.SequenceNumber, Timestamp: e.pkt.Timestamp, SSRC: ssrc,
				Marker: e.pkt.Marker, Duplicate: true,
			})
			continue
		}
		seen[e.extSeq] = e.pkt
		if first {
			minExt = e.extSeq
			first = false
		}
		maxExt = e.extSeq
		res.Packets = append(res.Packets, Packet{
			Seq: e.pkt.SequenceNumber, Timestamp: e.pkt.Timestamp, SSRC: ssrc,
			Marker: e.pkt.Marker, Payload: e.pkt.Payload,
		})
	}

	// Gaps: contiguous runs of missing extended sequences between the first and
	// last packet seen. The range names the missing sequences themselves.
	for extSeq := minExt + 1; extSeq <= maxExt; extSeq++ {
		if _, ok := seen[extSeq]; ok {
			continue
		}
		start := extSeq
		for {
			if _, ok := seen[extSeq+1]; ok || extSeq+1 > maxExt {
				break
			}
			extSeq++
		}
		gap := Gap{
			SSRC: ssrc, FromSeq: seqAt(base, start), ToSeq: seqAt(base, extSeq),
			ExtFrom: start, ExtTo: extSeq,
			Missing: int(extSeq-start) + 1,
		}
		if start > minExt {
			gap.FromTime = timestampBefore(res.Packets, seqAt(base, start-1))
		}
		if extSeq < maxExt {
			gap.ToTime = timestampAfter(res.Packets, seqAt(base, extSeq+1))
		}
		res.Gaps = append(res.Gaps, gap)
	}
	return res, nil
}

// findBase returns the circular minimum sequence number: the candidate for
// which every packet's 16-bit delta is non-negative. This is the start of the
// stream for any window smaller than half the sequence space.
func findBase(pkts []*rtp.Packet) uint16 {
	seen := map[uint16]bool{}
	for _, p := range pkts {
		seen[p.SequenceNumber] = true
	}
	for _, cand := range pkts {
		c := cand.SequenceNumber
		ok := true
		for seq := range seen {
			if int16(seq-c) < 0 {
				ok = false
				break
			}
		}
		if ok {
			return c
		}
	}
	return pkts[0].SequenceNumber
}

// seqAt maps an extended sequence number back to 16 bits.
func seqAt(base uint16, ext uint32) uint16 { return base + uint16(ext) }

func timestampBefore(pkts []Packet, seq uint16) uint32 {
	for i := len(pkts) - 1; i >= 0; i-- {
		if !pkts[i].Duplicate && pkts[i].Seq == seq {
			return pkts[i].Timestamp
		}
	}
	return 0
}

func timestampAfter(pkts []Packet, seq uint16) uint32 {
	for _, p := range pkts {
		if !p.Duplicate && p.Seq == seq {
			return p.Timestamp
		}
	}
	return 0
}

// Blocks splits recovered packets into fixed-sample-count blocks suitable for
// checkpointing. Incomplete gaps are reported alongside, never filled.
func Blocks(res Result, blockSamples int) (blocks [][]Packet, incomplete []Gap) {
	if blockSamples <= 0 {
		blockSamples = 1
	}
	var cur []Packet
	for _, p := range res.Packets {
		if p.Duplicate {
			continue
		}
		cur = append(cur, p)
		if len(cur) == blockSamples {
			blocks = append(blocks, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		blocks = append(blocks, cur)
	}
	return blocks, res.Gaps
}

// Summary renders a short human-readable line for terminal output.
func (r Result) Summary() string {
	return fmt.Sprintf("ssrc=%08x received=%d duplicate=%d out_of_order=%d gaps=%d",
		r.SSRC, r.Received, r.Duplicate, r.OutOfOrder, len(r.Gaps))
}
