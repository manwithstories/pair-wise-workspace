// Sample-reference expansion: chunk map + stts + ctts -> per-sample offset,
// size, DTS and composition offset.
package container

import "mediacore/internal/media"

// buildSampleRefs maps every sample index to an absolute file offset and a
// DTS/PTS in track ticks, validating the chunk map against stsz and against
// the file bounds. It retains O(samples) references but no media bytes.
func buildSampleRefs(tb *trackBuild, sampleCount int, fileSize int64) ([]SampleRef, int64, error) {
	refs := make([]SampleRef, sampleCount)
	var timed uint64
	for _, e := range tb.stts {
		timed += uint64(e.SampleCount)
	}
	if int(timed) < sampleCount {
		return nil, -1, media.NewError(media.CodeInvalidBoxLength, -1, 0,
			"stts covers %d samples, stsz has %d", timed, sampleCount)
	}

	sttsPos, sttsLeft := 0, 0
	if len(tb.stts) > 0 {
		sttsLeft = int(tb.stts[0].SampleCount)
	}
	nextDelta := func() int64 {
		for sttsPos < len(tb.stts) && sttsLeft == 0 {
			sttsPos++
			if sttsPos < len(tb.stts) {
				sttsLeft = int(tb.stts[sttsPos].SampleCount)
			}
		}
		if sttsPos >= len(tb.stts) {
			return 0
		}
		sttsLeft--
		return int64(tb.stts[sttsPos].SampleDelta)
	}

	cttsPos, cttsLeft := 0, 0
	if len(tb.ctts) > 0 {
		cttsLeft = int(tb.ctts[0].SampleCount)
	}
	nextCTTS := func() int64 {
		for cttsPos < len(tb.ctts) && cttsLeft == 0 {
			cttsPos++
			if cttsPos < len(tb.ctts) {
				cttsLeft = int(tb.ctts[cttsPos].SampleCount)
			}
		}
		if cttsPos >= len(tb.ctts) {
			return 0
		}
		cttsLeft--
		if tb.cttsVersion == 1 {
			return int64(tb.ctts[cttsPos].SampleOffsetV1)
		}
		return int64(tb.ctts[cttsPos].SampleOffsetV0)
	}

	// samplesPerChunk resolves the stsc run covering a 1-based chunk index.
	samplesPerChunk := func(chunk int) (uint32, bool) {
		var per uint32
		found := false
		for _, e := range tb.stsc {
			if int(e.FirstChunk) <= chunk {
				per, found = e.SamplesPerChunk, true
			} else {
				break
			}
		}
		return per, found
	}

	sampleIdx := 0
	dts := int64(0)
	var firstOff int64 = -1
	for ci, chunkOff := range tb.chunkOffsets {
		per, ok := samplesPerChunk(ci + 1)
		if !ok {
			return nil, -1, media.NewError(media.CodeInvalidBoxLength, -1, 0,
				"stsc has no entry covering chunk %d", ci+1)
		}
		off := int64(chunkOff)
		for j := uint32(0); j < per && sampleIdx < sampleCount; j++ {
			var size uint32
			if tb.sampleSize != 0 {
				size = tb.sampleSize
			} else {
				size = tb.sampleSizes[sampleIdx]
			}
			delta := nextDelta()
			refs[sampleIdx] = SampleRef{
				Offset:   off,
				Size:     int32(size),
				DTS:      dts,
				Duration: delta,
				CTTS:     nextCTTS(),
			}
			if firstOff < 0 {
				firstOff = off
			}
			if off < 0 || off+int64(size) > fileSize {
				return nil, -1, media.NewError(media.CodeTruncatedMdat, off, 0,
					"sample %d needs %d bytes at %d, file is %d bytes", sampleIdx, size, off, fileSize)
			}
			off += int64(size)
			dts += delta
			sampleIdx++
		}
		if sampleIdx >= sampleCount {
			break
		}
	}
	if sampleIdx < sampleCount {
		return nil, -1, media.NewError(media.CodeInvalidBoxLength, -1, 0,
			"chunk tables cover %d of %d samples", sampleIdx, sampleCount)
	}
	return refs, firstOff, nil
}
