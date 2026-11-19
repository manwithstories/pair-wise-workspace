package demux

import (
	"mediapack/internal/clock"
	"mediapack/internal/fault"
)

// tableInput carries the raw sample-table boxes of one track.
type tableInput struct {
	stts []sttsEntry
	ctts []cttsEntry // version 1 (signed) composition offsets; may be nil
	stsc []stscEntry
	stsz []uint32 // per-sample sizes
	stco []uint64 // chunk offsets (from stco or co64)
	stss map[uint32]bool
}

type sttsEntry struct{ Count, Delta uint32 }
type cttsEntry struct {
	Count  uint32
	Offset int32
}
type stscEntry struct{ FirstChunk, SamplesPerChunk, DescIndex uint32 }

// buildSamples expands the sample table into a per-sample list, validating
// monotonicity and payload availability against the actual file size. All
// anomalies are recorded on fl with both file offset and media time; the
// skip policy never hides the gap: unavailable samples stay in the list
// with Available=false so the pipeline can report the missing interval.
func buildSamples(in tableInput, fl *fault.List, trackID, timescale uint32, fileSize int64) []Sample {
	n := len(in.stsz)
	if n == 0 {
		return nil
	}
	// Expand decoding timestamps from stts.
	deltas := make([]uint32, 0, n)
	for _, e := range in.stts {
		for i := uint32(0); i < e.Count && len(deltas) < n; i++ {
			deltas = append(deltas, e.Delta)
		}
	}
	for len(deltas) < n { // stts short: repeat last delta, note it
		if len(deltas) == 0 {
			deltas = append(deltas, 0)
		} else {
			deltas = append(deltas, deltas[len(deltas)-1])
		}
	}
	// Expand composition offsets from ctts (signed, version 1 semantics).
	offsets := make([]int32, n)
	idx := 0
	for _, e := range in.ctts {
		for i := uint32(0); i < e.Count && idx < n; i++ {
			offsets[idx] = e.Offset
			idx++
		}
	}
	// Expand chunk layout: sample -> chunk, then byte offsets.
	chunkOf := make([]uint32, n)
	s := 0
	for ci := 0; ci < len(in.stco) && s < n; ci++ {
		spc := samplesPerChunk(in.stsc, uint32(ci+1))
		for j := uint32(0); j < spc && s < n; j++ {
			chunkOf[s] = uint32(ci)
			s++
		}
	}
	samples := make([]Sample, n)
	var dts, prevPTS int64
	first := true
	chunkPos := map[uint32]uint64{} // chunk index -> next byte offset
	missingRecorded := false
	for i := 0; i < n; i++ {
		smp := Sample{Index: uint32(i), DTS: dts, Duration: deltas[i], Size: in.stsz[i],
			Keyframe: in.stss[uint32(i+1)], Available: true}
		smp.PTS = dts + int64(offsets[i])
		ci := chunkOf[i]
		off, ok := chunkPos[ci]
		if !ok {
			off = in.stco[ci]
		}
		smp.Offset = int64(off)
		chunkPos[ci] = off + uint64(smp.Size)
		// Timestamp rollback detection on the presentation timeline.
		if !first && smp.PTS < prevPTS {
			fl.Add(fault.Skip, smp.Offset, trackID, clock.Ticks(smp.PTS, timescale),
				"timestamp rollback: sample %d PTS %d < previous %d", i, smp.PTS, prevPTS)
			smp.Available = false
		}
		prevPTS = smp.PTS
		first = false
		// Zero-size sample in the middle of the stream is table damage.
		if smp.Size == 0 && i < n-1 {
			fl.Add(fault.Skip, smp.Offset, trackID, clock.Ticks(smp.DTS, timescale),
				"zero-size sample %d inside stream", i)
			smp.Available = false
		}
		// Payload beyond end of file: truncated tail.
		if smp.Offset+int64(smp.Size) > fileSize {
			if !missingRecorded {
				fl.Add(fault.Skip, smp.Offset, trackID, clock.Ticks(smp.DTS, timescale),
					"sample %d payload exceeds file size %d (truncated)", i, fileSize)
				missingRecorded = true
			}
			smp.Available = false
		}
		samples[i] = smp
		dts += int64(deltas[i])
	}
	return samples
}

// samplesPerChunk resolves the stsc table for a 1-based chunk number.
func samplesPerChunk(stsc []stscEntry, chunk uint32) uint32 {
	spc := uint32(1)
	for _, e := range stsc {
		if e.FirstChunk <= chunk {
			spc = e.SamplesPerChunk
		} else {
			break
		}
	}
	return spc
}

// missingIntervals derives canonical-time gaps from unavailable samples.
func missingIntervals(samples []Sample, timescale uint32) []Interval {
	var out []Interval
	var cur *Interval
	for i := range samples {
		s := &samples[i]
		if s.Available {
			cur = nil
			continue
		}
		start := clock.Ticks(s.PTS, timescale)
		end := clock.Ticks(s.PTS+int64(s.Duration), timescale)
		if cur != nil && start <= cur.EndTicks {
			cur.EndTicks = end
			continue
		}
		out = append(out, Interval{StartTicks: start, EndTicks: end, Reason: "sample data unavailable"})
		cur = &out[len(out)-1]
	}
	return out
}
