package mp4

import (
	"fmt"

	gomp4 "github.com/abema/go-mp4"

	"archivetl/internal/diag"
)

// This file holds the per-table validators. Every function rejects count
// mismatches and out-of-range values before they can drive an allocation or
// an index, so malformed tables fail closed with a structured error anchored
// at the offending box's byte offset when one is known.

func sampleSizes(source string, stsz *gomp4.Stsz, boxOff int64) ([]uint32, *diag.Error) {
	n := int(stsz.SampleCount)
	if n > MaxSamples {
		return nil, indexErr(source, "stsz.count", "sample count exceeds the safety cap",
			boxOff, diag.ErrOverflow)
	}
	sizes := make([]uint32, n)
	if stsz.SampleSize != 0 {
		for i := range sizes {
			sizes[i] = stsz.SampleSize
		}
		return sizes, nil
	}
	if len(stsz.EntrySize) != n {
		return nil, indexErr(source, "stsz.entries",
			fmt.Sprintf("declared %d samples but %d size entries", n, len(stsz.EntrySize)),
			boxOff, diag.ErrInvalid)
	}
	copy(sizes, stsz.EntrySize)
	return sizes, nil
}

func sampleDTS(source string, stts *gomp4.Stts, n int, boxOff int64) ([]int64, *diag.Error) {
	dts := make([]int64, 0, n)
	var cumulative int64
	for _, e := range stts.Entries {
		if e.SampleCount == 0 {
			return nil, indexErr(source, "stts.zero", "zero run length in stts",
				boxOff, diag.ErrInvalid)
		}
		for i := uint32(0); i < e.SampleCount; i++ {
			dts = append(dts, cumulative)
			cumulative += int64(e.SampleDelta)
		}
	}
	if len(dts) != n {
		return nil, indexErr(source, "stts.count",
			fmt.Sprintf("stts expands to %d samples, stsz says %d", len(dts), n),
			boxOff, diag.ErrInvalid)
	}
	return dts, nil
}

func chunkOffsets(source string, stco *gomp4.Stco, co64 *gomp4.Co64,
	fileSize int64, stcoOff, co64Off int64) ([]int64, *diag.Error) {
	var raw []int64
	boxOff := stcoOff
	switch {
	case co64 != nil:
		boxOff = co64Off
		if len(co64.ChunkOffset) != int(co64.EntryCount) {
			return nil, indexErr(source, "co64.entries", "co64 entry count mismatch",
				boxOff, diag.ErrInvalid)
		}
		for _, o := range co64.ChunkOffset {
			raw = append(raw, int64(o))
		}
	default:
		if len(stco.ChunkOffset) != int(stco.EntryCount) {
			return nil, indexErr(source, "stco.entries", "stco entry count mismatch",
				boxOff, diag.ErrInvalid)
		}
		for _, o := range stco.ChunkOffset {
			raw = append(raw, int64(o))
		}
	}
	if len(raw) == 0 {
		return nil, indexErr(source, "stco.empty", "no chunks indexed",
			boxOff, diag.ErrInvalid)
	}
	for i, o := range raw {
		if o < 8 || o >= fileSize {
			// Anchor at the exact four-byte entry when reachable.
			entryOff := boxOff + 16 + int64(i)*4
			return nil, indexErr(source, "bounds.chunk",
				fmt.Sprintf("chunk %d offset %d outside [8,%d)", i+1, o, fileSize),
				entryOff, diag.ErrOverflow)
		}
	}
	return raw, nil
}

func samplesPerChunk(source string, stsc *gomp4.Stsc, chunkCount, sampleCount int,
	boxOff int64) ([]uint32, *diag.Error) {
	if len(stsc.Entries) == 0 || stsc.Entries[0].FirstChunk != 1 {
		return nil, indexErr(source, "stsc.first", "stsc must start at chunk 1",
			boxOff, diag.ErrInvalid)
	}
	out := make([]uint32, chunkCount)
	entryIdx := 0
	for c := 1; c <= chunkCount; c++ {
		for entryIdx+1 < len(stsc.Entries) && stsc.Entries[entryIdx+1].FirstChunk <= uint32(c) {
			entryIdx++
		}
		e := stsc.Entries[entryIdx]
		if e.SamplesPerChunk == 0 {
			return nil, indexErr(source, "stsc.spc", "zero samples-per-chunk",
				boxOff, diag.ErrInvalid)
		}
		out[c-1] = e.SamplesPerChunk
	}
	total := uint64(0)
	for _, n := range out {
		total += uint64(n)
	}
	if int(total) != sampleCount {
		return nil, indexErr(source, "stsc.total",
			fmt.Sprintf("stsc covers %d samples, stsz says %d", total, sampleCount),
			boxOff, diag.ErrInvalid)
	}
	return out, nil
}

func keySet(source string, stss *gomp4.Stss, sampleCount int,
	boxOff int64) (map[uint32]struct{}, *diag.Error) {
	set := map[uint32]struct{}{}
	if stss == nil {
		// No stss: every sample is independently decodable.
		for i := 1; i <= sampleCount; i++ {
			set[uint32(i)] = struct{}{}
		}
		return set, nil
	}
	if len(stss.SampleNumber) != int(stss.EntryCount) {
		return nil, indexErr(source, "stss.entries", "stss entry count mismatch",
			boxOff, diag.ErrInvalid)
	}
	prev := uint32(0)
	for i, n := range stss.SampleNumber {
		if n < 1 || int(n) > sampleCount || n <= prev {
			entryOff := boxOff + 16 + int64(i)*4
			return nil, indexErr(source, "stss.range",
				fmt.Sprintf("keyframe number %d invalid for %d samples", n, sampleCount),
				entryOff, diag.ErrInvalid)
		}
		prev = n
		set[n] = struct{}{}
	}
	return set, nil
}
