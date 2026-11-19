package pipeline

import (
	"sort"

	"mediasync/internal/audio"
	"mediasync/internal/rtprepair"
	"mediasync/internal/segment"
	"mediasync/internal/timeline"
)

// rtpClockHz is the RTP timestamp rate used by the generated stream.
const rtpClockHz int64 = 90000

// gapsFromPackets derives timeline gaps from the repaired packet stream. The
// position of every packet comes from its (wrap-expanded) RTP timestamp, so a
// hole in the sequence numbers becomes the exact interval where the missing
// payloads would have been. An empty result means the stream is contiguous.
func gapsFromPackets(emitted []*rtprepair.Packet) []segment.Gap {
	if len(emitted) < 2 {
		return nil
	}
	unwrap := timelineTickUnwrapper()
	positions := make([]int64, len(emitted))
	seqs := make([]int64, len(emitted))
	base := int64(0)
	for i, p := range emitted {
		ticks := unwrap(p.Header.Timestamp)
		if i == 0 {
			base = ticks
		}
		positions[i] = (ticks - base) * 1_000_000_000 / rtpClockHz
		seqs[i] = int64(p.Seq)
	}
	step := nominalStep(positions, seqs)
	if step <= 0 {
		return nil
	}
	type interval struct{ start, end int64 }
	var raw []interval
	for i := 1; i < len(emitted); i++ {
		missing := seqs[i] - seqs[i-1] - 1
		if missing <= 0 {
			continue
		}
		// Trust the timestamp delta, but never let clock jitter shrink a
		// known hole below the missing packet count.
		span := positions[i] - positions[i-1] - step
		if span < missing*step {
			span = missing * step
		}
		raw = append(raw, interval{positions[i-1] + step, positions[i-1] + step + span})
	}
	if len(raw) == 0 {
		return nil
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].start < raw[j].start })
	merged := make([]segment.Gap, 0, len(raw))
	for _, in := range raw {
		if in.end <= in.start {
			continue
		}
		if n := len(merged); n > 0 && in.start <= merged[n-1].EndNS {
			if in.end > merged[n-1].EndNS {
				merged[n-1].EndNS = in.end
			}
			continue
		}
		merged = append(merged, segment.Gap{StartNS: in.start, EndNS: in.end})
	}
	return merged
}

// nominalStep returns the most common per-packet advance in nanoseconds.
func nominalStep(positions, seqs []int64) int64 {
	counts := map[int64]int{}
	for i := 1; i < len(positions); i++ {
		if seqs[i] != seqs[i-1]+1 {
			continue
		}
		if d := positions[i] - positions[i-1]; d > 0 {
			counts[d]++
		}
	}
	if len(counts) == 0 {
		return 0
	}
	best, bestN := int64(0), 0
	for step, n := range counts {
		if n > bestN || (n == bestN && step < best) {
			best, bestN = step, n
		}
	}
	return best
}

// timelineTickUnwrapper expands a 32-bit RTP timestamp into a monotonically
// increasing tick counter.
func timelineTickUnwrapper() func(uint32) int64 {
	u := timeline.NewTickUnwrapper(0)
	return func(v uint32) int64 { return u.Expand(uint64(v)) }
}

// silenceRunsBetween returns silence runs overlapping the window.
func (c *camera) silenceRunsBetween(startNS, endNS int64) []audio.Run {
	if c.silence == nil {
		return nil
	}
	var out []audio.Run
	for _, run := range c.silence.Runs {
		if run.EndNS > startNS && run.StartNS < endNS {
			out = append(out, run)
		}
	}
	return out
}
