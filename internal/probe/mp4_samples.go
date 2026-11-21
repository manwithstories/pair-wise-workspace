package probe

import (
	"fmt"

	"github.com/abema/go-mp4"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// trackTable is the raw sample-table view of one track, before timing and
// offsets are normalised into model.Sample values.
type trackTable struct {
	timescale int64
	stts      *mp4.Stts
	ctts      *mp4.Ctts
	stsc      *mp4.Stsc
	stsz      *mp4.Stsz
	chunks    []uint64
	sync      map[uint32]bool
	start     clock.Rational
	limit     int
}

// buildSamples walks stts/ctts/stsc/stco/stsz and produces samples in decode
// order. It validates the tables strictly: a table that does not account for
// exactly the declared sample count is invalid media rather than silently
// truncated.
func buildSamples(t trackTable, trackID uint32) ([]model.Sample, error) {
	bad := func(format string, args ...interface{}) error {
		return model.New(model.KindInvalidMedia, "probe.mp4", fmt.Sprintf(format, args...)).WithTrack(trackID)
	}
	total := int(t.stsz.SampleCount)
	if total > t.limit {
		return nil, model.New(model.KindOverLimit, "probe.mp4", fmt.Sprintf("sample count %d exceeds limit %d", total, t.limit)).WithTrack(trackID)
	}
	if total == 0 {
		return nil, nil
	}
	if len(t.stsc.Entries) == 0 {
		return nil, bad("stsc has no entries for %d samples", total)
	}
	if len(t.stts.Entries) == 0 {
		return nil, bad("stts has no entries for %d samples", total)
	}
	if len(t.chunks) == 0 {
		return nil, bad("stco/co64 has no chunk offsets")
	}

	sizes := sampleSizes(t.stsz, total)
	chunkToSampleStart, err := mapChunksToSamples(t.stsc, len(t.chunks), total, bad)
	if err != nil {
		return nil, err
	}

	dts := make([]int64, 0, total)
	pos := int64(1) // media time ticks are 1-based in the elst sense
	for _, e := range t.stts.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			if len(dts) >= total {
				return nil, bad("stts covers more than %d samples", total)
			}
			dts = append(dts, pos)
			pos += int64(e.SampleDelta)
		}
	}
	if len(dts) != total {
		return nil, bad("stts covers %d samples, stsz declares %d", len(dts), total)
	}
	// stts stores deltas; convert to start times with the first sample at the
	// edit start.
	startTick := dts[0]
	for i := range dts {
		dts[i] -= startTick
	}

	pts := append([]int64(nil), dts...)
	if t.ctts != nil {
		idx := 0
		for _, e := range t.ctts.Entries {
			for i := uint32(0); i < e.SampleCount; i++ {
				if idx >= total {
					return nil, bad("ctts covers more than %d samples", total)
				}
				pts[idx] = dts[idx] + t.ctts.GetSampleOffset(idx)
				idx++
			}
		}
		if idx != total {
			return nil, bad("ctts covers %d samples, want %d", idx, total)
		}
	}

	samples := make([]model.Sample, total)
	for i := 0; i < total; i++ {
		d, err := clock.FromTicks(dts[i], t.timescale)
		if err != nil {
			return nil, bad("sample %d DTS: %v", i, err)
		}
		p, err := clock.FromTicks(pts[i], t.timescale)
		if err != nil {
			return nil, bad("sample %d PTS: %v", i, err)
		}
		delta := clock.Zero()
		if i+1 < total {
			nd, err := clock.FromTicks(dts[i+1], t.timescale)
			if err != nil {
				return nil, bad("sample %d DTS: %v", i+1, err)
			}
			if delta, err = nd.Sub(d); err != nil {
				return nil, bad("sample %d duration: %v", i, err)
			}
		} else if total > 1 {
			prev, _ := clock.FromTicks(dts[i]-dts[i-1], t.timescale)
			delta = prev
		} else if t.stts.Entries[0].SampleDelta > 0 {
			delta, _ = clock.FromTicks(int64(t.stts.Entries[0].SampleDelta), t.timescale)
		}
		if delta.Sign() < 0 {
			return nil, bad("sample %d has negative duration", i)
		}
		off, size, err := sampleLocation(i, sizes, chunkToSampleStart, t.chunks)
		if err != nil {
			return nil, bad("sample %d location: %v", i, err)
		}
		key := false
		if t.sync == nil {
			// No stss means every sample is a sync sample (ISO/IEC 14496-12).
			key = true
		} else {
			key = t.sync[uint32(i+1)]
		}
		st, err := d.Add(t.start)
		if err != nil {
			return nil, bad("sample %d start: %v", i, err)
		}
		pt, err := p.Add(t.start)
		if err != nil {
			return nil, bad("sample %d presentation: %v", i, err)
		}
		samples[i] = model.Sample{
			Index: i, Offset: off, Size: size,
			DTS: st, PTS: pt, Duration: delta, Keyframe: key,
		}
	}
	return samples, nil
}

func sampleSizes(stsz *mp4.Stsz, total int) []uint32 {
	if stsz.SampleSize != 0 {
		out := make([]uint32, total)
		for i := range out {
			out[i] = stsz.SampleSize
		}
		return out
	}
	if len(stsz.EntrySize) < total {
		total = len(stsz.EntrySize)
	}
	return stsz.EntrySize[:total]
}

// mapChunksToSamples returns a per-chunk index of the first sample it holds.
func mapChunksToSamples(stsc *mp4.Stsc, chunkCount, sampleCount int, bad func(string, ...interface{}) error) ([]int, error) {
	first := make([]int, chunkCount)
	assigned := 0
	for i, e := range stsc.Entries {
		nextChunk := chunkCount + 1
		if i+1 < len(stsc.Entries) {
			nextChunk = int(stsc.Entries[i+1].FirstChunk)
		}
		if e.FirstChunk == 0 {
			return nil, bad("stsc entry %d has first_chunk 0", i)
		}
		if e.SamplesPerChunk == 0 {
			return nil, bad("stsc entry %d has samples_per_chunk 0", i)
		}
		for c := int(e.FirstChunk) - 1; c < nextChunk-1 && c < chunkCount; c++ {
			first[c] = assigned
			assigned += int(e.SamplesPerChunk)
		}
	}
	if assigned != sampleCount {
		return nil, bad("stsc assigns %d samples across %d chunks, stsz declares %d", assigned, chunkCount, sampleCount)
	}
	return first, nil
}

func sampleLocation(index int, sizes []uint32, chunkFirst []int, chunks []uint64) (int64, uint32, error) {
	// Find the last chunk whose first sample index is <= index.
	lo, hi := 0, len(chunkFirst)-1
	chunk := 0
	for lo <= hi {
		mid := (lo + hi) / 2
		if chunkFirst[mid] <= index {
			chunk = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	offset := chunks[chunk]
	for i := chunkFirst[chunk]; i < index; i++ {
		if i >= len(sizes) {
			return 0, 0, fmt.Errorf("size table ends at %d before sample %d", len(sizes), index)
		}
		offset += uint64(sizes[i])
	}
	if offset > uint64(1)<<62 {
		return 0, 0, fmt.Errorf("sample offset %d out of range", offset)
	}
	if index >= len(sizes) {
		return 0, 0, fmt.Errorf("no size for sample %d", index)
	}
	return int64(offset), sizes[index], nil
}
