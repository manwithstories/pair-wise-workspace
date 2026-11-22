// Package transport reassembles an ordered, deduplicated media stream
// from RTP packets. Duplicate deliveries never count towards bitrate,
// 16-bit sequence wraparound is unfolded, and gaps are reported as
// lost sequence ranges.
package transport

import (
	"sort"

	"github.com/pion/rtp"
)

// Stats summarizes one processed packet stream.
type Stats struct {
	Received    int
	Duplicates  int
	Lost        int
	UniqueBytes int64
	LostRanges  [][2]uint64 // inclusive extended-sequence ranges
}

// Result is the reassembled stream.
type Result struct {
	Packets []*rtp.Packet // ordered, deduplicated
	Stats   Stats
}

// extendSeq maps a 16-bit sequence number onto the 64-bit extended
// sequence space relative to the highest sequence seen so far
// (RFC 3711 style index estimation).
func extendSeq(highest uint64, seq uint16) uint64 {
	base := highest &^ 0xFFFF
	cand := base + uint64(seq)
	switch {
	case cand+32768 < highest:
		return cand + 65536 // forward wraparound
	case highest+32768 < cand:
		if cand >= 65536 {
			return cand - 65536 // late packet from the previous cycle
		}
	}
	return cand
}

// Process consumes packets in arrival order and returns the ordered,
// deduplicated stream plus loss/duplicate statistics.
func Process(pkts []*rtp.Packet) Result {
	type item struct {
		ext uint64
		p   *rtp.Packet
	}
	seen := make(map[uint64]bool)
	var items []item
	var st Stats
	var highest uint64
	init := false

	for _, p := range pkts {
		if p == nil {
			continue
		}
		st.Received++
		var ext uint64
		if !init {
			ext = uint64(p.SequenceNumber)
			init = true
		} else {
			ext = extendSeq(highest, p.SequenceNumber)
		}
		if seen[ext] {
			st.Duplicates++
			continue
		}
		seen[ext] = true
		if ext > highest || !init {
			highest = ext
		}
		st.UniqueBytes += int64(len(p.Payload))
		items = append(items, item{ext: ext, p: p})
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].ext < items[j].ext })

	res := Result{Stats: st}
	var prev uint64
	for i, it := range items {
		if i > 0 && it.ext > prev+1 {
			res.Stats.Lost += int(it.ext - prev - 1)
			res.Stats.LostRanges = append(res.Stats.LostRanges, [2]uint64{prev + 1, it.ext - 1})
		}
		prev = it.ext
		res.Packets = append(res.Packets, it.p)
	}
	return res
}

// PayloadBytes concatenates the ordered unique payloads.
func PayloadBytes(res Result) []byte {
	var n int64
	for _, p := range res.Packets {
		n += int64(len(p.Payload))
	}
	out := make([]byte, 0, n)
	for _, p := range res.Packets {
		out = append(out, p.Payload...)
	}
	return out
}
