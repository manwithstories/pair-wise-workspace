package pipeline

import (
	"mediasync/internal/audio"
	"mediasync/internal/rtprepair"
	"mediasync/internal/segment"
)

// gapsFromPackets derives timeline gaps from the repaired packet stream. Each
// emitted packet advances the timeline by its payload duration; a hole in the
// extended sequence numbers becomes a gap interval starting where the missing
// payload would have begun.
func gapsFromPackets(emitted []*rtprepair.Packet, seed int64, cam *camera) []segment.Gap {
	if len(emitted) == 0 || cam == nil {
		return nil
	}
	tb := cam.timebase
	var gaps []segment.Gap
	prev := int64(emitted[0].Seq)
	prevNS := int64(0)
	for i, p := range emitted {
		seq := int64(p.Seq)
		if i > 0 && seq != prev+1 {
			missing := seq - prev - 1
			gapNS := missing * tickSpan(emitted[i-1], emitted[i])
			gaps = append(gaps, segment.Gap{StartNS: prevNS, EndNS: prevNS + gapNS})
			prevNS += gapNS
		}
		_ = tb
		prev = seq
		prevNS += tickSpan(p, p)
	}
	return gaps
}

// tickSpan converts the RTP timestamp delta between two packets into
// nanoseconds on the shared timeline.
func tickSpan(a, b *rtprepair.Packet) int64 {
	const ticksPerSecond = 90000
	var delta uint32
	if b != nil && a != nil {
		delta = b.Header.Timestamp - a.Header.Timestamp
	}
	if delta == 0 {
		delta = 3000
	}
	return int64(delta) * 1_000_000_000 / ticksPerSecond
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
