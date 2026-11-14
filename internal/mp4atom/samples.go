package mp4atom

import (
	"mp4replay/internal/report"
)

// Sample is one fully expanded media sample.
type Sample struct {
	Index   int   // 0-based sample index
	Offset  int64 // absolute file offset of the sample bytes
	Size    uint32
	Sync    bool
	Dts     int64 // decoded time in media timescale (continuous)
	Pts     int64 // composed time in media timescale (continuous)
	CttsOff int64
}

// chunkPlan describes how many samples each chunk carries and its file offset.
type chunkPlan struct {
	offset int64
	count  int
}

// buildChunks expands stsc run groups into a per-chunk sample count.
func buildChunks(t *Table) ([]chunkPlan, error) {
	chunks := len(t.Stco)
	if chunks == 0 {
		return nil, report.NewError(report.ExitBoxInvalid, "samples",
			"no chunk offsets")
	}
	perChunk := make([]int, chunks)
	for i, run := range t.Stsc {
		start := int(run.FirstChunk) - 1 // stsc is 1-based
		if start < 0 || start >= chunks {
			return nil, report.NewError(report.ExitBoxInvalid, "samples",
				"stsc first_chunk out of range")
		}
		var end int
		if i+1 < len(t.Stsc) {
			end = int(t.Stsc[i+1].FirstChunk) - 1
		} else {
			end = chunks
		}
		for c := start; c < end; c++ {
			perChunk[c] = int(run.SamplesPerChunk)
		}
	}
	plans := make([]chunkPlan, chunks)
	for c := 0; c < chunks; c++ {
		if perChunk[c] <= 0 {
			return nil, report.NewError(report.ExitBoxInvalid, "samples",
				"chunk without stsc coverage")
		}
		plans[c] = chunkPlan{offset: t.Stco[c], count: perChunk[c]}
	}
	// A final chunk may contain fewer samples than the stsc run's nominal value.
	// Reconcile against the authoritative stsz total by trimming the last chunk.
	if t.StszCount != 0 {
		total := 0
		for _, p := range plans {
			total += p.count
		}
		if excess := total - int(t.StszCount); excess > 0 && excess < plans[chunks-1].count {
			plans[chunks-1].count -= excess
		}
	}
	return plans, nil
}

func (t *Table) sizeAt(i int) (uint32, error) {
	if t.StszFixed != 0 {
		return t.StszFixed, nil
	}
	if i < 0 || i >= len(t.StszSizes) {
		return 0, report.NewError(report.ExitBoxInvalid, "samples",
			"stsz sample index out of range")
	}
	return t.StszSizes[i], nil
}

// BuildSamples expands stsc runs, chunk offsets and stsz into a flat table.
// Times are filled later by BuildTimeline; here only offsets/sizes/sync flags
// are resolved so no whole-file read is required.
func BuildSamples(t *Table) ([]Sample, error) {
	plans, err := buildChunks(t)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, p := range plans {
		total += p.count
	}
	if int(t.StszCount) != 0 && total != int(t.StszCount) && t.StszFixed == 0 {
		return nil, report.NewError(report.ExitBoxInvalid, "samples",
			"stsc/stsz sample count mismatch")
	}
	sync := syncSet(t.Stss)
	out := make([]Sample, 0, total)
	idx := 0
	for _, p := range plans {
		cursor := p.offset
		for s := 0; s < p.count; s++ {
			size, err := t.sizeAt(idx)
			if err != nil {
				return nil, err
			}
			smp := Sample{
				Index:  idx,
				Offset: cursor,
				Size:   size,
				Sync:   !t.HasStss || sync[idx+1],
			}
			out = append(out, smp)
			cursor += int64(size)
			idx++
		}
	}
	return out, nil
}

// syncSet turns the 1-based stss list into an O(1) lookup map.
func syncSet(stss []uint32) map[int]bool {
	m := make(map[int]bool, len(stss))
	for _, n := range stss {
		m[int(n)] = true
	}
	return m
}
