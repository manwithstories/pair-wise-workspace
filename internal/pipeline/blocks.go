package pipeline

import (
	"os"

	trackmap "mediasync/internal/map"
	"mediasync/internal/model"
)

// block is one checkpoint unit spanning a contiguous sample run of one track.
type block struct {
	trackID      uint32
	track        *model.Track
	reader       *os.File
	start        int
	end          int
	samples      int
	outSampleEnd int
	durationSec  float64
}

// planBlocks splits every mapped track into runs of at most samplesPerBlock
// samples, cutting early when a run would exceed the 64 MiB block budget. The
// bound is checked before any read, so a single oversized block is never
// materialised.
func planBlocks(m *trackmap.Result, samplesPerBlock int) ([]block, int) {
	if samplesPerBlock <= 0 {
		samplesPerBlock = 1
	}
	var plan []block
	total := 0
	for _, sel := range m.Selections {
		tr := sel.Track
		start := 0
		for start < len(tr.Samples) {
			end := start
			var bytes int64
			for end < len(tr.Samples) && end-start < samplesPerBlock {
				size := int64(tr.Samples[end].Size)
				if bytes+size > maxBlockBytes {
					break
				}
				bytes += size
				end++
			}
			// A single sample larger than the budget cannot be split; let the
			// read path reject it with a SIZE_LIMIT error rather than emit an
			// empty block.
			if end == start {
				end = start + 1
			}
			dur := sampleRunSeconds(tr, start, end)
			total += end - start
			plan = append(plan, block{trackID: tr.ID, track: tr, start: start, end: end,
				samples: end - start, outSampleEnd: total, durationSec: dur})
			start = end
		}
	}
	return plan, total
}

func sampleRunSeconds(tr *model.Track, start, end int) float64 {
	if tr.Timescale == 0 || start >= len(tr.Samples) {
		return 0
	}
	var ticks int64
	for i := start; i < end && i < len(tr.Samples); i++ {
		ticks += tr.Samples[i].Duration
	}
	return float64(ticks) / float64(tr.Timescale)
}

func blockDuration(b block) float64 {
	if b.durationSec <= 0 {
		return 1.0 / 30.0
	}
	return b.durationSec
}

// readBlock copies one block's coded bytes in sample order.
func readBlock(f *os.File, b block) ([]byte, error) {
	var out []byte
	for i := b.start; i < b.end; i++ {
		s := &b.track.Samples[i]
		if s.Size < 0 || s.Size > 64<<20 {
			return nil, model.NewError(model.ErrSizeLimit, b.trackID, s.Offset, "sample %d size %d out of range", s.Index, s.Size)
		}
		buf := make([]byte, s.Size)
		if s.Size > 0 {
			n, err := f.ReadAt(buf, s.Offset)
			if err != nil {
				return nil, model.WrapError(model.ErrTruncatedBox, b.trackID, s.Offset, err)
			}
			if n != s.Size {
				return nil, model.NewError(model.ErrTruncatedBox, b.trackID, s.Offset, "short read for sample %d", s.Index)
			}
		}
		out = append(out, buf...)
	}
	return out, nil
}
