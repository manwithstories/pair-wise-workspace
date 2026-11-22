package stream

import (
	"github.com/abema/go-mp4"
)

func sampleOffsets(sizes []uint32, stsc []mp4.StscEntry, chunks []uint64) []int64 {
	if len(stsc) == 0 || len(chunks) == 0 {
		return nil
	}
	want := len(sizes)
	out := make([]int64, 0, want)
	next := 0
	for si, e := range stsc {
		firstChunk := int(e.FirstChunk)
		if firstChunk < 1 {
			continue
		}
		lastChunk := len(chunks)
		if si+1 < len(stsc) && int(stsc[si+1].FirstChunk) > firstChunk {
			lastChunk = int(stsc[si+1].FirstChunk) - 1
		}
		if lastChunk > len(chunks) {
			lastChunk = len(chunks)
		}
		for ci := firstChunk; ci <= lastChunk; ci++ {
			off := chunks[ci-1]
			for k := uint32(0); k < e.SamplesPerChunk; k++ {
				if next >= want {
					return out
				}
				out = append(out, int64(off))
				off += uint64(sizes[next])
				next++
			}
		}
	}
	return out
}

func insideAnyMdat(mdats []byteRange, off, size uint64) bool {
	for _, m := range mdats {
		if m.contains(off, size) {
			return true
		}
	}
	return false

}
