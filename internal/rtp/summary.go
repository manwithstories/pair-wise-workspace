package rtpx

import (
	"fmt"
	"sort"
)

// Summary returns a snapshot. Loss is derived from the observed sequence
// range, so it describes the path rather than the sender; comparing it with
// the generator's send count attributes the difference to forwarding-stage
// backpressure.
func (a *Analyzer) Summary() (Summary, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.stream) == 0 {
		return Summary{}, ErrNoPackets
	}
	out := Summary{
		ClockRate:     a.cfg.ClockRate,
		ReorderWindow: a.cfg.ReorderWindow,
		Streams:       make([]StreamStats, 0, len(a.stream)),
	}
	var weightedJitter, weight float64
	for ssrc, st := range a.stream {
		rangeLen := int64(st.highest-st.firstSeq) + 1
		lost := rangeLen - int64(st.unique)
		if lost < 0 {
			lost = 0
		}
		avgMs := ticksToMs(st.jitter, a.cfg.ClockRate)
		maxMs := ticksToMs(st.jitterMax, a.cfg.ClockRate)
		out.Streams = append(out.Streams, StreamStats{
			SSRC:          ssrc,
			Received:      st.received,
			Unique:        st.unique,
			Expected:      rangeLen,
			Lost:          lost,
			Duplicates:    st.duplicates,
			Reordered:     st.reordered,
			LateOutside:   st.late,
			FirstSequence: uint16(st.firstSeq),
			LastSequence:  uint16(st.highest),
			WrapCount:     st.wraps,
			MarkerCount:   st.markers,
			PayloadBytes:  st.payload,
			JitterAvgMs:   round3(avgMs),
			JitterMaxMs:   round3(maxMs),
		})
		out.Received += st.received
		out.Unique += st.unique
		out.Lost += lost
		out.Duplicates += st.duplicates
		out.Reordered += st.reordered
		out.LateOutside += st.late
		weightedJitter += avgMs * float64(st.received)
		weight += float64(st.received)
		if maxMs > out.JitterMaxMs {
			out.JitterMaxMs = maxMs
		}
	}
	if weight > 0 {
		out.JitterAvgMs = weightedJitter / weight
	}
	sort.Slice(out.Streams, func(i, j int) bool { return out.Streams[i].SSRC < out.Streams[j].SSRC })
	out.JitterAvgMs = round3(out.JitterAvgMs)
	out.JitterMaxMs = round3(out.JitterMaxMs)
	return out, nil
}

// FormatError describes why a summary could not be produced.
func FormatError(err error) string { return fmt.Sprintf("rtp stats: %v", err) }

func ticksToMs(ticks float64, clockRate int) float64 {
	if clockRate <= 0 {
		return 0
	}
	return ticks / float64(clockRate) * 1000
}

func round3(v float64) float64 {
	if v < 0 {
		v = 0
	}
	return float64(int64(v*1000+0.5)) / 1000
}
