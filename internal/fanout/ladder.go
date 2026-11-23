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

// FanOut transmits the source packets over one independent simulated
// channel per rung and recovers each rung on its own. The loss burst is
// injected on rung 0; other rungs see smaller deterministic disturbances.
func FanOut(pkts []rtp.Packet, rungs []Rung, lossBurst int) []LayerResult {
	results := make([]LayerResult, len(rungs))
	for i, rung := range rungs {
		burstStart, burstLen := -1, 0
		dupEvery, reorderEvery := 0, 0
		switch i {
		case 0:
			burstStart, burstLen = len(pkts)/3, lossBurst
			reorderEvery = 11
		case 1:
			burstStart, burstLen = len(pkts)/2, lossBurst/2
			dupEvery = 13
		default:
			burstStart, burstLen = len(pkts)/(2+i), lossBurst/(2*i)
			reorderEvery = 17 + i
		}
		wire := mrtp.Simulate(pkts, burstStart, burstLen, dupEvery, reorderEvery)
		rec := mrtp.NewRecoverer(pkts, 3, 13.3, 50)
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
