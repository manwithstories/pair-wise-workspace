package fixture

import (
	"math/rand/v2"
	"sort"
)

// inject applies the requested fault injections to the ordered records.
func inject(recs []record, o Options, rng *rand.Rand) []record {
	rtpIdx := []int{}
	videoIdx := []int{}
	for i, r := range recs {
		if r.rtp && r.video {
			videoIdx = append(videoIdx, i)
		} else if r.rtp {
			rtpIdx = append(rtpIdx, i)
		}
	}

	if o.has("loss") {
		drop := map[int]bool{}
		for _, i := range rtpIdx {
			if rng.Float64() < 0.02 {
				drop[i] = true
			}
		}
		recs = keep(recs, func(i int) bool { return !drop[i] })
		rtpIdx = reindex(recs, false)
		videoIdx = reindex(recs, true)
	}

	if o.has("duplicate") {
		var out []record
		for _, r := range recs {
			out = append(out, r)
			if r.rtp && rng.Float64() < 0.03 {
				out = append(out, r) // re-delivered packet
			}
		}
		recs = out
		rtpIdx = reindex(recs, false)
		videoIdx = reindex(recs, true)
	}

	if o.has("reorder") {
		for k := 0; k < 5 && len(rtpIdx) > 2; k++ {
			i := rtpIdx[rng.IntN(len(rtpIdx)-1)]
			recs[i], recs[i+1] = recs[i+1], recs[i]
		}
	}

	if o.has("late-vfr") && len(videoIdx) > 4 {
		// Hold back three video packets with rising lateness:
		//   a1 ~1.5 s — inside the reorder window (reordered)
		//   a2 ~4.8 s — declared lost, then forwarded: timeline replay
		//   b  to end — beyond the rollback horizon: quarantine
		a1 := videoIdx[len(videoIdx)/5]
		a2 := videoIdx[2*len(videoIdx)/5]
		b := videoIdx[len(videoIdx)/3]
		r1, r2, rb := recs[a1], recs[a2], recs[b]
		// Remove from the stream, highest index first.
		idxs := []int{a1, a2, b}
		sort.Ints(idxs)
		for i := len(idxs) - 1; i >= 0; i-- {
			recs = append(recs[:idxs[i]], recs[idxs[i]+1:]...)
		}
		reinsert := func(r record, later int) {
			pos := later
			if pos >= len(recs) {
				pos = len(recs)
			}
			recs = append(recs[:pos], append([]record{r}, recs[pos:]...)...)
		}
		reinsert(r1, a1+120) // ~1.5 s of mixed traffic
		reinsert(r2, a2+385) // ~4.8 s of mixed traffic
		recs = append(recs, rb)
		rtpIdx = reindex(recs, false)
		videoIdx = reindex(recs, true)
	}

	if o.has("bad-frame") && len(videoIdx) > 0 {
		// Corrupt one byte inside a frame payload (CRC mismatch).
		i := videoIdx[len(videoIdx)/2]
		buf := append([]byte(nil), recs[i].buf...)
		buf[4+12+13+5] ^= 0xff // prefix + RTP header + frame header + 5
		recs[i].buf = buf
	}

	if o.has("truncated-extension") && len(rtpIdx) > 0 {
		i := rtpIdx[2*len(rtpIdx)/5]
		pkt := append([]byte(nil), recs[i].buf[4:]...)
		pkt[0] |= 0x10 // X bit
		out := make([]byte, 0, len(pkt)+8)
		out = append(out, pkt[:12]...)
		out = append(out, 0xbe, 0xde, 0x00, 0x02) // profile + 2 words
		out = append(out, 1, 2, 3, 4)             // only 1 word present
		recs[i].buf = prefix(out)
		rtpIdx = reindex(recs, false)
	}

	if o.has("short-header") && len(rtpIdx) > 0 {
		i := rtpIdx[len(rtpIdx)/2]
		recs[i].buf = prefix(recs[i].buf[4:12]) // 8 of 12 header bytes
	}
	return recs
}

func keep(recs []record, pred func(int) bool) []record {
	var out []record
	for i, r := range recs {
		if pred(i) {
			out = append(out, r)
		}
	}
	return out
}

// reindex returns the positions of RTP records (video if v is true,
// audio otherwise) after the record slice changed.
func reindex(recs []record, v bool) []int {
	var out []int
	for i, r := range recs {
		if r.rtp && r.video == v {
			out = append(out, i)
		}
	}
	return out
}
