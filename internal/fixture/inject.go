package fixture

import (
	"encoding/binary"
	"math/rand/v2"
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
		// Hold back two video packets: one ~1.5 s (inside the rollback
		// horizon), one to the very end (beyond it -> quarantine).
		a := videoIdx[len(videoIdx)/3]
		b := videoIdx[2*len(videoIdx)/3]
		ra, rb := recs[a], recs[b]
		recs = append(recs[:b], recs[b+1:]...)
		recs = append(recs[:a], recs[a+1:]...)
		insertAt := len(recs)/3 + 120 // ~1.5 s of mixed traffic later
		if insertAt >= len(recs) {
			insertAt = len(recs) - 1
		}
		recs = append(recs[:insertAt], append([]record{ra}, recs[insertAt:]...)...)
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

// seqOf reads the RTP sequence number from a prefixed record.
func seqOf(buf []byte) uint16 {
	return binary.BigEndian.Uint16(buf[4+2 : 4+4])
}
