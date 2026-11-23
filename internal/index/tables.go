package index

import (
	"errors"
	"fmt"

	"github.com/abema/go-mp4"
)

// chunkOffsets reads the legacy chunk offset table.
func chunkOffsets(box mp4.IBox) ([]uint64, bool) {
	switch v := box.(type) {
	case *mp4.Stco:
		if v == nil {
			return nil, false
		}
		out := make([]uint64, len(v.ChunkOffset))
		for i, o := range v.ChunkOffset {
			out[i] = uint64(o)
		}
		return out, true
	case *mp4.Co64:
		if v == nil {
			return nil, false
		}
		return v.ChunkOffset, true
	default:
		return nil, false
	}
}

func sampleSizes(sz *mp4.Stsz) []uint32 {
	if sz.SampleSize != 0 {
		out := make([]uint32, sz.SampleCount)
		for i := range out {
			out[i] = sz.SampleSize
		}
		return out
	}
	if uint32(len(sz.EntrySize)) > sz.SampleCount {
		return sz.EntrySize[:sz.SampleCount]
	}
	return sz.EntrySize
}

func sampleDeltas(stts mp4.IBox) []uint32 {
	st, ok := stts.(*mp4.Stts)
	if !ok || st == nil {
		return nil
	}
	var out []uint32
	for _, e := range st.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			out = append(out, e.SampleDelta)
		}
	}
	return out
}

func syncSamples(stss mp4.IBox, n int) []bool {
	out := make([]bool, n)
	st, ok := stss.(*mp4.Stss)
	if !ok || st == nil || len(st.SampleNumber) == 0 {
		for i := range out {
			out[i] = true
		}
		return out
	}
	for _, sn := range st.SampleNumber {
		if sn > 0 && int(sn) <= n {
			out[sn-1] = true
		}
	}
	return out
}

// sampleOffsets resolves the byte offset of every sample from the chunk
// offset and sample-to-chunk tables, verifying ranges stay inside a mdat.
func sampleOffsets(chunkOffsets []uint64, stsc mp4.IBox, sizes []uint32, fileSize int64, mdats [][2]int64) ([]int64, error) {
	sc, ok := stsc.(*mp4.Stsc)
	if !ok || sc == nil {
		return nil, fmt.Errorf("%w: stsc missing", ErrUnsupported)
	}
	if len(chunkOffsets) == 0 || len(sc.Entries) == 0 {
		return nil, nil
	}
	out := make([]int64, 0, len(sizes))
	si := 0
	for ci, base := range chunkOffsets {
		// Entries are sorted by FirstChunk; find the entry governing chunk ci+1.
		for si+1 < len(sc.Entries) && int(sc.Entries[si+1].FirstChunk) <= ci+1 {
			si++
		}
		if int(sc.Entries[si].FirstChunk) > ci+1 {
			continue
		}
		per := int(sc.Entries[si].SamplesPerChunk)
		var cursor = int64(base)
		for j := 0; j < per && len(out) < len(sizes); j++ {
			size := int64(sizes[len(out)])
			if cursor < 0 || cursor+size > fileSize {
				return nil, fmt.Errorf("%w: sample %d at %d+%d past EOF %d", ErrTruncated, len(out), cursor, size, fileSize)
			}
			if !inMdat(cursor, size, mdats) {
				return nil, fmt.Errorf("%w: sample %d at %d+%d outside mdat", ErrUnsupported, len(out), cursor, size)
			}
			out = append(out, cursor)
			cursor += size
		}
	}
	if len(out) != len(sizes) {
		return nil, fmt.Errorf("%w: resolved %d of %d samples", ErrTruncated, len(out), len(sizes))
	}
	return out, nil
}

func inMdat(off, size int64, mdats [][2]int64) bool {
	if len(mdats) == 0 {
		return true
	}
	for _, r := range mdats {
		if off >= r[0] && off+size <= r[1] {
			return true
		}
	}
	return false
}

// errUnsupported is returned when a box payload has an unexpected type.
var errUnsupported = errors.New("unexpected box payload")
