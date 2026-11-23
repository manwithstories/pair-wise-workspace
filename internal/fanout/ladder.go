// Package fanout splits one input stream into a multi-bitrate ladder of
// representations, each recovered independently over its own channel.
package fanout

import (
	"fmt"

	"github.com/pion/rtp"

	mrtp "mediad/internal/rtp"
)

// Rung is one bitrate representation layer.
type Rung struct {
	Name    string
	Bitrate int // bps
	Scale   int // size denominator relative to the source (1 = full)
}

// Ladder builds n rungs from a source bitrate, halving each step.
// n is clamped to [3, 8].
func Ladder(srcBitrate, n int) []Rung {
	if n < 3 {
		n = 3
	}
	if n > 8 {
		n = 8
	}
	rungs := make([]Rung, n)
	for i := range rungs {
		scale := 1 << i
		rungs[i] = Rung{
			Name:    fmt.Sprintf("r%d", i),
			Bitrate: srcBitrate / scale,
			Scale:   scale,
		}
	}
	return rungs
}

// LayerResult holds one rung's recovered packets and recovery stats.
type LayerResult struct {
	Rung     Rung
	Packets  []rtp.Packet
	Stats    mrtp.Stats
	LostSeqs map[uint16]bool // sequence numbers dropped on the wire
}

// BurstWindow returns the deterministic burst-loss window (start index and
// length) applied to rung rungIdx for a stream of total packets.
func BurstWindow(total, rungIdx, lossBurst int) (start, length int) {
	switch rungIdx {
	case 0:
		return total / 3, lossBurst
	case 1:
		return total / 2, lossBurst / 2
	default:
		return total / (2 + rungIdx), lossBurst / (2 * rungIdx)
	}
}

// channelParams returns the deterministic per-rung channel disturbance.
func channelParams(total, i, lossBurst int) (start, blen, dupEvery, reorderEvery int) {
	start, blen = BurstWindow(total, i, lossBurst)
	switch i {
	case 0:
		reorderEvery = 11
	case 1:
		dupEvery = 13
	default:
		reorderEvery = 17 + i
	}
	if blen == 0 {
		start = -1
	}
	return start, blen, dupEvery, reorderEvery
}

// FanOut transmits the source packets over one independent simulated
// channel per rung and recovers each rung on its own. recoverySrc is the
// sender buffer used to answer retransmission requests; packets absent from
// it are unrecoverable.
func FanOut(pkts []rtp.Packet, rungs []Rung, lossBurst int, recoverySrc []rtp.Packet) []LayerResult {
	results := make([]LayerResult, len(rungs))
	for i, rung := range rungs {
		start, blen, dupEvery, reorderEvery := channelParams(len(pkts), i, lossBurst)
		wire := mrtp.Simulate(pkts, start, blen, dupEvery, reorderEvery)
		rec := mrtp.NewRecoverer(recoverySrc, 3, 13.3, 50)
		var out []rtp.Packet
		for _, p := range wire.Packets {
			rec.Push(p)
			out = append(out, rec.Drain()...)
		}
		out = append(out, rec.Finish()...)
		results[i] = LayerResult{Rung: rung, Packets: out, Stats: rec.Stats, LostSeqs: wire.Lost}
	}
	return results
}
