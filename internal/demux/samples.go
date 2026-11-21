package demux

import (
	"mediamerge/internal/model"
)

// finishTrack validates cross-table consistency, then expands the compressed
// tables into a per-sample index. Validation runs before any output is
// written, so a bad table never produces a partial target.
func (f *File) finishTrack(rt *rawTrack) error {
	if len(rt.sizes) == 0 {
		return derr(model.StageDemux, model.CodeSampleTable, rt.stblBase, rt.id, false,
			"track %d has no samples", rt.id)
	}
	if len(rt.stts) == 0 {
		return derr(model.StageDemux, model.CodeMissingBox, rt.stblBase, rt.id, false,
			"track %d has empty stts", rt.id)
	}
	if len(rt.stsc) == 0 {
		return derr(model.StageDemux, model.CodeMissingBox, rt.stblBase, rt.id, false,
			"track %d has empty stsc", rt.id)
	}
	if len(rt.offsets) == 0 {
		return derr(model.StageDemux, model.CodeMissingBox, rt.stblBase, rt.id, false,
			"track %d has no chunk offsets", rt.id)
	}
	// stts total sample count must equal stsz sample count.
	var timingCount uint64
	for _, r := range rt.stts {
		if r.count == 0 {
			return derr(model.StageDemux, model.CodeSampleTable, rt.stblBase, rt.id, false,
				"track %d stts has zero-length run", rt.id)
		}
		timingCount += uint64(r.count)
	}
	if timingCount != uint64(len(rt.sizes)) {
		return derr(model.StageDemux, model.CodeSampleTable, rt.stblBase, rt.id, false,
			"track %d stts covers %d samples but stsz has %d", rt.id, timingCount, len(rt.sizes))
	}
	if len(rt.ctts) > 0 {
		var c uint64
		for _, r := range rt.ctts {
			c += uint64(r.count)
		}
		if c != uint64(len(rt.sizes)) {
			return derr(model.StageDemux, model.CodeSampleTable, rt.stblBase, rt.id, false,
				"track %d ctts covers %d samples but stsz has %d", rt.id, c, len(rt.sizes))
		}
	}
	if rt.handler == "vide" || rt.handler == "soun" {
		if supportedCodecs[rt.entryType] == "" {
			return derr(model.StageDemux, model.CodeUnsupported, rt.stblBase, rt.id, false,
				"track %d sample entry %q is not supported", rt.id, rt.entryType)
		}
	}
	// stsc entries must be ascending and in range.
	lastFirst := uint32(0)
	for i, e := range rt.stsc {
		if e.firstChunk == 0 || e.firstChunk <= lastFirst {
			return derr(model.StageDemux, model.CodeSampleTable, rt.stblBase, rt.id, false,
				"track %d stsc entry %d has non-ascending first_chunk %d", rt.id, i, e.firstChunk)
		}
		if e.samplesPerChunk == 0 {
			return derr(model.StageDemux, model.CodeSampleTable, rt.stblBase, rt.id, false,
				"track %d stsc entry %d has zero samples_per_chunk", rt.id, i)
		}
		if e.descIndex == 0 {
			return derr(model.StageDemux, model.CodeSampleTable, rt.stblBase, rt.id, false,
				"track %d stsc entry %d has zero sample_description_index", rt.id, i)
		}
		lastFirst = e.firstChunk
	}
	if int(rt.stsc[0].firstChunk) != 1 {
		return derr(model.StageDemux, model.CodeSampleTable, rt.stblBase, rt.id, false,
			"track %d stsc does not start at chunk 1", rt.id)
	}
	return nil
}

// toTrack expands chunk tables into a sample index.
func (f *File) toTrack(rt *rawTrack) *Track {
	t := &Track{}
	t.InputID = rt.id
	t.TargetID = rt.id
	switch rt.handler {
	case "vide":
		t.MediaType = model.MediaVideo
	case "soun":
		t.MediaType = model.MediaAudio
	default:
		t.MediaType = model.MediaType(rt.handler)
	}
	t.HandlerType = rt.handler
	t.Codec = supportedCodecs[rt.entryType]
	if t.Codec == "" {
		t.Codec = "unknown:" + rt.entryType
	}
	t.Timescale = rt.timescale
	t.Language = rt.language
	t.Width = rt.width
	t.Height = rt.height
	t.ChannelCount = rt.channels
	t.SampleSizeBits = rt.sampleBits
	t.SampleRate = rt.sampleRate
	t.SampleEntry = rt.entry
	t.SampleCount = len(rt.sizes)
	t.TrackDuration = rt.duration

	dtss := expandSTTS(rt.stts, len(rt.sizes))
	ctss := expandCTTS(rt.ctts, len(rt.sizes))
	syncSet := map[uint32]bool{}
	for _, n := range rt.sync {
		syncSet[n] = true
	}
	hasSync := len(rt.sync) > 0

	sampleNo := 0
	chunkIdx := 0
	var dts uint64
	first := true
	samples := make([]model.SampleRef, 0, len(rt.sizes))
	for chunk := 0; chunk < len(rt.offsets) && sampleNo < len(rt.sizes); chunk++ {
		spc := samplesPerChunk(rt.stsc, uint32(chunk+1))
		pos := rt.offsets[chunk]
		for k := uint32(0); k < spc && sampleNo < len(rt.sizes); k++ {
			size := rt.sizes[sampleNo]
			sync := true
			if rt.handler == "vide" && hasSync {
				sync = syncSet[uint32(sampleNo+1)]
			}
			samples = append(samples, model.SampleRef{
				TrackIndex: chunkIdx,
				TrackID:    rt.id,
				Index:      sampleNo,
				Offset:     int64(pos),
				Size:       size,
				DTS:        dts,
				Duration:   dtss[sampleNo],
				CTSOffset:  ctss[sampleNo],
				Sync:       sync,
			})
			if first {
				t.FirstDTS = dts
				first = false
			}
			t.LastDTS = dts
			if dtss[sampleNo] > 0 && dts > ^uint64(0)-uint64(dtss[sampleNo]) {
				dts = ^uint64(0)
			} else {
				dts += uint64(dtss[sampleNo])
			}
			pos += uint64(size)
			sampleNo++
			_ = k
		}
	}
	if sampleNo != len(rt.sizes) {
		// Extra sizes beyond the chunk map: index them at the end of the last
		// chunk so validation can still locate the error precisely.
		for ; sampleNo < len(rt.sizes); sampleNo++ {
			samples = append(samples, model.SampleRef{
				TrackIndex: chunkIdx, TrackID: rt.id, Index: sampleNo,
				Offset: -1, Size: rt.sizes[sampleNo], DTS: dts, Duration: dtss[sampleNo],
			})
		}
	}
	t.Samples = samples
	t.SizeTableOffset = rt.sizeBase
	t.SizeTableFixed = rt.sizeFixed
	t.ChunkTableOffset = rt.chunkBase
	return t
}

// Sparse returns the track without its sample index, for metadata-only views.
func (t *Track) Sparse() *Track {
	cp := *t
	cp.Samples = nil
	return &cp
}

func expandSTTS(runs []sttsRun, n int) []uint32 {
	out := make([]uint32, n)
	i := 0
	for _, r := range runs {
		for k := uint32(0); k < r.count && i < n; k++ {
			out[i] = r.delta
			i++
		}
	}
	return out
}

func expandCTTS(runs []cttsRun, n int) []int64 {
	out := make([]int64, n)
	i := 0
	for _, r := range runs {
		for k := uint32(0); k < r.count && i < n; k++ {
			out[i] = int64(r.offset)
			i++
		}
	}
	return out
}

func samplesPerChunk(stsc []stscRun, chunk uint32) uint32 {
	spc := uint32(0)
	for _, e := range stsc {
		if e.firstChunk <= chunk {
			spc = e.samplesPerChunk
		} else {
			break
		}
	}
	return spc
}
