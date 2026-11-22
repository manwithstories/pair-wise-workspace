package scan

import (
	"os"

	"github.com/abema/go-mp4"

	"mediaslice/media"
)

// Local thin aliases keep the rest of the scanner decoupled from go-mp4
// concrete struct names and centralize version handling.
type (
	tkhdBox = mp4.Tkhd
	mdhdBox = mp4.Mdhd
	hdlrBox = mp4.Hdlr
	elstBox = mp4.Elst
	sttsBox = mp4.Stts
	stssBox = mp4.Stss
	stscBox = mp4.Stsc
	stszBox = mp4.Stsz
	stcoBox = mp4.Stco
	co64Box = mp4.Co64
)

func hdlrHandlerType(h *hdlrBox) string { return string(h.HandlerType[:]) }

// values normalizes an elst entry independent of box version (v0/v1).
func elstValues(e *mp4.ElstEntry) (dur, mediaTime int64, rate int32) {
	if e.SegmentDurationV1 != 0 || e.MediaTimeV1 != 0 {
		return int64(e.SegmentDurationV1), e.MediaTimeV1, int32(e.MediaRateInteger)
	}
	return int64(e.SegmentDurationV0), int64(e.MediaTimeV0), int32(e.MediaRateInteger)
}
// parseSamples expands the chunk/sample tables into absolute offsets and
// 90 kHz timings and verifies every byte range lies inside mdat.
func parseSamples(f *os.File, source string, trak *boxNode, ti *TrackIndex, idx *FileIndex) *media.MediaError {
	stbl := trak.findFirst("stbl")
	if stbl == nil {
		return media.NewError(media.KindMissingTrack, source, "stbl missing").
			WithTrack(ti.TrackID).WithOffset(int64(trak.info.Offset), 8)
	}
	sttsNode := stbl.findFirst("stts")
	stszNode := stbl.findFirst("stsz")
	stscNode := stbl.findFirst("stsc")
	if sttsNode == nil || stszNode == nil || stscNode == nil {
		return media.NewError(media.KindMissingTrack, source, "stts/stsz/stsc incomplete").
			WithTrack(ti.TrackID).WithOffset(int64(stbl.info.Offset), 8)
	}
	var stts sttsBox
	if err := payloadAt(f, sttsNode, &stts); err != nil || stts.EntryCount == 0 {
		return media.NewError(media.KindBadSampleTable, source, "invalid stts").
			WithTrack(ti.TrackID).WithOffset(int64(sttsNode.info.Offset), 4)
	}
	var stsz stszBox
	if err := payloadAt(f, stszNode, &stsz); err != nil {
		return media.NewError(media.KindBadSampleTable, source, err.Error()).
			WithTrack(ti.TrackID).WithOffset(int64(stszNode.info.Offset), 4)
	}
	var stsc stscBox
	if err := payloadAt(f, stscNode, &stsc); err != nil {
		return media.NewError(media.KindBadSampleTable, source, err.Error()).
			WithTrack(ti.TrackID).WithOffset(int64(stscNode.info.Offset), 4)
	}
	offsets, oerr := readChunkOffsets(f, source, stbl, ti.TrackID)
	if oerr != nil {
		return oerr
	}
	sizes, serr := expandSizes(&stsz)
	if serr != nil {
		return media.NewError(media.KindBadSampleTable, source, serr.Error()).
			WithTrack(ti.TrackID).WithOffset(int64(stszNode.info.Offset), 4)
	}
	n := sampleCount(&stts)
	if len(sizes) != n {
		return media.NewError(media.KindBadSampleTable, source,
			"stsz/stts sample count mismatch").
			WithTrack(ti.TrackID).WithOffset(int64(stszNode.info.Offset), 4)
	}
	absOff, aerr := expandChunkOffsets(&stsc, offsets, sizes)
	if aerr != nil {
		return media.NewError(media.KindBadSampleTable, source, aerr.Error()).
			WithTrack(ti.TrackID).WithOffset(int64(stscNode.info.Offset), 4)
	}

	// Keyframes: explicit stss; otherwise every sample is a sync sample.
	keySet := map[int]bool{}
	if stssNode := stbl.findFirst("stss"); stssNode != nil {
		var stss stssBox
		if err := payloadAt(f, stssNode, &stss); err != nil {
			return media.NewError(media.KindBadSampleTable, source, err.Error()).
				WithTrack(ti.TrackID).WithOffset(int64(stssNode.info.Offset), 4)
		}
		for _, k := range stss.SampleNumber {
			if k == 0 || int(k) > n {
				return media.NewError(media.KindBadSampleTable, source,
					"stss sample number out of range").
					WithTrack(ti.TrackID).WithOffset(int64(stssNode.info.Offset), 4)
			}
			keySet[int(k)-1] = true
		}
	} else {
		for i := 0; i < n; i++ {
			keySet[i] = true
		}
	}

	scale := uint64(media.ClockRate) / uint64(ti.Timescale)
	ticks := make([]media.Ticks, n)
	durs := make([]media.Ticks, n)
	var rawTime uint64
	sample := 0
	for _, e := range stts.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			if sample >= n {
				return media.NewError(media.KindBadSampleTable, source,
					"stts expands past declared count").
					WithTrack(ti.TrackID).WithOffset(int64(sttsNode.info.Offset), 4)
			}
			ticks[sample] = media.Ticks(rawTime * scale)
			durs[sample] = media.Ticks(uint64(e.SampleDelta) * scale)
			rawTime += uint64(e.SampleDelta)
			sample++
		}
	}

	ti.Samples = make([]SampleRecord, n)
	for i := 0; i < n; i++ {
		start, end := absOff[i], absOff[i]+int64(sizes[i])
		if idx.MdatStart != 0 && (start < idx.MdatStart || end > idx.MdatEnd) {
			return media.NewError(media.KindBadSampleTable, source,
				"sample byte range outside mdat").
				WithTrack(ti.TrackID).WithOffset(start, 8).
				WithInterval(ticks[i], ticks[i]+durs[i])
		}
		ti.Samples[i] = SampleRecord{
			Index:     i,
			Offset:    start,
			Size:      int(sizes[i]),
			StartTick: ticks[i],
			Duration:  durs[i],
			Keyframe:  keySet[i],
		}
	}
	return nil
}

// readChunkOffsets handles stco (32-bit) and co64 (64-bit) tables.
func readChunkOffsets(f *os.File, source string, stbl *boxNode, trackID uint32) ([]int64, *media.MediaError) {
	if node := stbl.findFirst("stco"); node != nil {
		var box stcoBox
		if err := payloadAt(f, node, &box); err != nil {
			return nil, media.NewError(media.KindBadSampleTable, source, err.Error()).
				WithTrack(trackID).WithOffset(int64(node.info.Offset), 4)
		}
		out := make([]int64, len(box.ChunkOffset))
		for i, v := range box.ChunkOffset {
			out[i] = int64(v)
		}
		return out, nil
	}
	if node := stbl.findFirst("co64"); node != nil {
		var box co64Box
		if err := payloadAt(f, node, &box); err != nil {
			return nil, media.NewError(media.KindBadSampleTable, source, err.Error()).
				WithTrack(trackID).WithOffset(int64(node.info.Offset), 4)
		}
		out := make([]int64, len(box.ChunkOffset))
		for i, v := range box.ChunkOffset {
			out[i] = int64(v)
		}
		return out, nil
	}
	return nil, media.NewError(media.KindMissingTrack, source, "no stco/co64 chunk offset table").
		WithTrack(trackID).WithOffset(int64(stbl.info.Offset), 8)
}

func sampleCount(stts *mp4.Stts) int {
	var n uint32
	for _, e := range stts.Entries {
		n += e.SampleCount
	}
	return int(n)
}

func expandSizes(stsz *mp4.Stsz) ([]uint32, error) {
	if stsz.SampleSize != 0 {
		out := make([]uint32, stsz.SampleCount)
		for i := range out {
			out[i] = stsz.SampleSize
		}
		return out, nil
	}
	if int(stsz.SampleCount) != len(stsz.EntrySize) {
		return nil, &sizeMismatchError{}
	}
	return append([]uint32(nil), stsz.EntrySize...), nil
}

type sizeMismatchError struct{}

func (*sizeMismatchError) Error() string { return "stsz entry size count mismatch" }

// expandChunkOffsets turns stsc runs + chunk offsets into per-sample
// absolute file offsets, validating contiguity and full coverage.
func expandChunkOffsets(stsc *mp4.Stsc, chunkOffsets []int64, sizes []uint32) ([]int64, error) {
	if len(stsc.Entries) == 0 || len(chunkOffsets) == 0 {
		return nil, &sizeMismatchError{}
	}
	totalSamples := len(sizes)
	out := make([]int64, 0, totalSamples)
	sampleCursor := 0
	for chunk := 0; chunk < len(chunkOffsets); chunk++ {
		perChunk := samplesPerChunk(stsc, uint32(chunk+1))
		if perChunk == 0 {
			return nil, &sizeMismatchError{}
		}
		cursor := chunkOffsets[chunk]
		for j := uint32(0); j < perChunk; j++ {
			if sampleCursor >= totalSamples {
				return nil, &sizeMismatchError{}
			}
			out = append(out, cursor)
			cursor += int64(sizes[sampleCursor])
			sampleCursor++
		}
	}
	if sampleCursor != totalSamples {
		return nil, &sizeMismatchError{}
	}
	return out, nil
}

// samplesPerChunk resolves the stsc first_chunk runs (1-based chunk numbers).
func samplesPerChunk(stsc *mp4.Stsc, chunk uint32) uint32 {
	per := uint32(0)
	for _, e := range stsc.Entries {
		if chunk >= e.FirstChunk {
			per = e.SamplesPerChunk
		} else {
			break
		}
	}
	return per
}
