package mp4

import (
	"github.com/abema/go-mp4"
	"mediasync/internal/media"
)

// indexTrackSamples expands stts/stsc/stsz/stco into a sample table and then
// derives the keyframe index. Issues carry the offset of the box that caused
// them so callers can point at the exact byte.
func indexTrackSamples(tr *Track, tb *trackBoxes, data []byte) {
	size := int64(len(data))
	if tb.stts == nil || tb.stsz == nil || tb.stsc == nil {
		tr.issue(media.CodeMissingEntry, boxOffset(tb, mp4.BoxTypeStbl()), 0,
			"track %d is missing stts/stsz/stsc", tr.TrackID)
		return
	}
	offsets := tb.offsets()
	if len(offsets) == 0 {
		tr.issue(media.CodeMissingEntry, boxOffset(tb, mp4.BoxTypeStbl()), 0, "track %d has no stco/co64 box", tr.TrackID)
		return
	}
	if tr.Timescale == 0 {
		tr.issue(media.CodeIncompatible, boxOffset(tb, mp4.BoxTypeMdhd()), 0,
			"track %d has zero media timescale", tr.TrackID).AsFatal()
		return
	}

	count := int(tb.stsz.SampleCount)
	if count > MaxSamples {
		tr.issue(media.CodeLimitExceeded, boxOffset(tb, mp4.BoxTypeStsz()), 0,
			"track %d declares %d samples, limit is %d", tr.TrackID, count, MaxSamples)
		count = MaxSamples
	}
	durations := expandDurations(tb.stts, count)
	sizes := sampleSizes(tb.stsz, count)
	chunkSamples := expandChunks(tb.stsc, len(offsets), tr)
	fileOffsets := layoutSamples(tr, tb.stsc, sizes, chunkSamples, offsets, size)

	dts := int64(0)
	prevDTS := int64(-1)
	for i := 0; i < len(sizes); i++ {
		dur := int64(0)
		if i < len(durations) {
			dur = int64(durations[i])
		}
		if prevDTS >= 0 && dts <= prevDTS {
			tr.issue(media.CodeNonMonotonic, offAt(data, fileOffsets[i]), i+1,
				"sample %d has non-monotonic decode time %d <= %d", i+1, dts, prevDTS)
		}
		prevDTS = dts
		tr.Samples = append(tr.Samples, Sample{
			Number:     uint32(i + 1),
			Offset:     fileOffsets[i],
			Size:       sizes[i],
			DTSNS:      translate(dts, tr.Timescale),
			DurationNS: translate(dur, tr.Timescale),
		})
		dts += dur
	}
	if declared := int(tb.stsz.SampleCount); declared > len(tr.Samples) {
		tr.issue(media.CodeMissingEntry, boxOffset(tb, mp4.BoxTypeStsz()), len(tr.Samples)+1,
			"stsz declares %d samples but only %d could be laid out", declared, len(tr.Samples))
	}
	buildKeyframeIndex(tr, tb.stss, data, boxOffset(tb, mp4.BoxTypeStss()), boxOffset(tb, mp4.BoxTypeStbl()))
	if len(tr.Samples) == 0 {
		tr.issue(media.CodeMissingEntry, boxOffset(tb, mp4.BoxTypeStbl()), 0,
			"track %d produced an empty sample table", tr.TrackID)
	}
}

// expandDurations flattens the stts run-length table, refusing runaway counts.
func expandDurations(stts *mp4.Stts, limit int) []uint32 {
	var out []uint32
	for _, e := range stts.Entries {
		for k := uint32(0); k < e.SampleCount; k++ {
			if len(out) >= limit && limit > 0 {
				return out
			}
			out = append(out, e.SampleDelta)
		}
	}
	return out
}

// sampleSizes expands stsz; a constant sample size covers short entries and a
// declared count larger than the table is reported by the caller.
func sampleSizes(stsz *mp4.Stsz, n int) []uint32 {
	if stsz.SampleSize != 0 {
		out := make([]uint32, n)
		for i := range out {
			out[i] = stsz.SampleSize
		}
		return out
	}
	if n > len(stsz.EntrySize) {
		n = len(stsz.EntrySize)
	}
	out := make([]uint32, n)
	copy(out, stsz.EntrySize[:n])
	return out
}

// expandChunks returns the number of samples stored in each chunk.
func expandChunks(stsc *mp4.Stsc, chunkCount int, tr *Track) []int {
	entries := stsc.Entries
	counts := make([]int, chunkCount)
	if len(entries) == 0 {
		tr.issue(media.CodeMissingEntry, 0, 0, "track %d has an empty stsc table", tr.TrackID)
		return counts
	}
	prevFirst := uint32(0)
	for i, e := range entries {
		if e.FirstChunk == 0 || e.FirstChunk < prevFirst {
			tr.issue(media.CodeNonMonotonic, 0, i+1,
				"stsc entry %d has invalid first_chunk %d", i+1, e.FirstChunk)
			continue
		}
		prevFirst = e.FirstChunk
		last := uint32(chunkCount)
		if i+1 < len(entries) && entries[i+1].FirstChunk > e.FirstChunk {
			last = entries[i+1].FirstChunk - 1
		}
		if int(last) > chunkCount {
			tr.issue(media.CodeOutOfRange, 0, i+1,
				"stsc entry %d covers chunk %d beyond chunk count %d", i+1, last, chunkCount)
			last = uint32(chunkCount)
		}
		for c := e.FirstChunk; c <= last && int(c) <= chunkCount; c++ {
			counts[c-1] = int(e.SamplesPerChunk)
		}
	}
	return counts
}

// layoutSamples walks chunks and assigns a file offset to every sample,
// flagging any sample that falls outside the file bounds.
func layoutSamples(tr *Track, stsc *mp4.Stsc, sizes []uint32, chunkSamples []int, offsets []int64, fileSize int64) []int64 {
	out := make([]int64, len(sizes))
	idx := 0
	for chunk := 0; chunk < len(chunkSamples) && idx < len(sizes); chunk++ {
		cur := offsets[chunk]
		for k := 0; k < chunkSamples[chunk] && idx < len(sizes); k++ {
			out[idx] = cur
			cur += int64(sizes[idx])
			idx++
		}
	}
	for ; idx < len(sizes); idx++ {
		out[idx] = -1
	}
	for i, off := range out {
		if off < 0 || off+int64(sizes[i]) > fileSize {
			// Report at the offending byte clamped into the file so the
			// offset always addresses real input.
			at := off
			if at > fileSize {
				at = fileSize
			}
			if at < 0 {
				at = 0
			}
			tr.issue(media.CodeOutOfRange, at, i+1,
				"sample %d offset %d size %d exceeds file size %d", i+1, off, sizes[i], fileSize)
		}
	}
	return out
}

func offAt(data []byte, off int64) int64 {
	if off < 0 {
		return 0
	}
	if off > int64(len(data)) {
		return int64(len(data))
	}
	return off
}

// boxOffset returns the file offset of an optional box, or 0 when absent.
func boxOffset(tb *trackBoxes, t mp4.BoxType) int64 {
	if tb == nil || tb.boxPos == nil {
		return 0
	}
	return int64(tb.boxPos[t])
}
