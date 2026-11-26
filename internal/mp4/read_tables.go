package mp4

import (
	"fmt"
	"io"
	"sort"

	gomp4 "github.com/abema/go-mp4"

	"mediad/internal/media"
)

func readSampleTables(rs io.ReadSeeker, trackID uint32, stbl *Box) ([]media.Sample, []*media.Fault, error) {
	var faults []*media.Fault

	stszBox := stbl.Find("stsz")
	if stszBox == nil {
		return nil, faults, media.NewFault(media.CodeMissingIndex, "missing stsz sample size table").WithTrack(trackID).WithBox("stsz").Fatal()
	}
	var stsz gomp4.Stsz
	if err := unmarshalTypedBox(rs, stszBox, &stsz); err != nil {
		return nil, faults, media.NewFault(media.CodeInvalidInput, "decode stsz: %v", err).WithTrack(trackID).WithBox("stsz").Fatal()
	}
	count := uint64(stsz.SampleCount)
	if count == 0 || count > MaxSamplesPerTrack {
		return nil, faults, media.NewFault(media.CodeInvalidInput, "sample count %d outside 1..%d", count, MaxSamplesPerTrack).
			WithTrack(trackID).WithBox("stsz").Fatal()
	}
	sampleCount := int(count)

	sizes := make([]uint32, sampleCount)
	if stsz.SampleSize != 0 {
		for i := range sizes {
			sizes[i] = stsz.SampleSize
		}
	} else {
		if len(stsz.EntrySize) < sampleCount {
			faults = append(faults, media.NewFault(media.CodeInvalidInput, "stsz contains %d sizes for %d samples", len(stsz.EntrySize), sampleCount).
				WithTrack(trackID).WithBox("stsz"))
			for i := 0; i < len(stsz.EntrySize); i++ {
				sizes[i] = stsz.EntrySize[i]
			}
		} else {
			copy(sizes, stsz.EntrySize[:sampleCount])
		}
	}

	durations := make([]uint32, sampleCount)
	sttsBox := stbl.Find("stts")
	if sttsBox == nil {
		faults = append(faults, media.NewFault(media.CodeTimestampMissing, "missing stts").WithTrack(trackID).WithBox("stts"))
	} else {
		var stts gomp4.Stts
		if err := unmarshalTypedBox(rs, sttsBox, &stts); err != nil {
			return nil, faults, media.NewFault(media.CodeInvalidInput, "decode stts: %v", err).WithTrack(trackID).WithBox("stts").Fatal()
		}
		pos := 0
		for _, e := range stts.Entries {
			run := int(e.SampleCount)
			if run > sampleCount-pos {
				faults = append(faults, media.NewFault(media.CodeTimestampMissing, "stts run exceeds sample table").WithTrack(trackID).WithBox("stts"))
				run = sampleCount - pos
			}
			for j := 0; j < run && pos < sampleCount; j++ {
				durations[pos] = e.SampleDelta
				pos++
			}
			if pos == sampleCount {
				break
			}
		}
		for i := pos; i < sampleCount; i++ {
			faults = append(faults, media.NewFault(media.CodeTimestampMissing, "sample %d has no stts duration", i+1).
				WithTrack(trackID).WithBox("stts"))
		}
	}

	composition := make([]int64, sampleCount)
	if cttsBox := stbl.Find("ctts"); cttsBox != nil {
		var ctts gomp4.Ctts
		if err := unmarshalTypedBox(rs, cttsBox, &ctts); err != nil {
			return nil, faults, media.NewFault(media.CodeInvalidInput, "decode ctts: %v", err).WithTrack(trackID).WithBox("ctts").Fatal()
		}
		pos := 0
		for _, e := range ctts.Entries {
			run := int(e.SampleCount)
			off := int64(0)
			if ctts.Version == 1 {
				off = int64(e.SampleOffsetV1)
			} else {
				off = int64(e.SampleOffsetV0)
			}
			for j := 0; j < run && pos < sampleCount; j++ {
				composition[pos] = off
				pos++
			}
		}
		if pos != sampleCount {
			faults = append(faults, media.NewFault(media.CodeTimestampMissing, "ctts covers %d of %d samples", pos, sampleCount).
				WithTrack(trackID).WithBox("ctts"))
		}
	}

	sync := make(map[uint32]bool)
	if stssBox := stbl.Find("stss"); stssBox != nil {
		var stss gomp4.Stss
		if err := unmarshalTypedBox(rs, stssBox, &stss); err != nil {
			return nil, faults, media.NewFault(media.CodeInvalidInput, "decode stss: %v", err).WithTrack(trackID).WithBox("stss").Fatal()
		}
		for _, n := range stss.SampleNumber {
			if n >= 1 && int(n) <= sampleCount {
				sync[n] = true
			}
		}
	}

	chunkOffsets, offsetBox, err := readChunkOffsets(rs, trackID, stbl)
	if err != nil {
		return nil, faults, err
	}
	spcEntries := []gomp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}}
	if stscBox := stbl.Find("stsc"); stscBox != nil {
		var stsc gomp4.Stsc
		if err := unmarshalTypedBox(rs, stscBox, &stsc); err != nil {
			return nil, faults, media.NewFault(media.CodeInvalidInput, "decode stsc: %v", err).WithTrack(trackID).WithBox("stsc").Fatal()
		}
		if len(stsc.Entries) > 0 {
			spcEntries = stsc.Entries
		}
	}

	samples := make([]media.Sample, sampleCount)
	var dts int64
	sampleCursor := 0
	for chunkIndex := 1; chunkIndex <= len(chunkOffsets) && sampleCursor < sampleCount; chunkIndex++ {
		spc := samplesPerChunk(spcEntries, uint32(chunkIndex))
		chunkOffset := chunkOffsets[chunkIndex-1]
		chunkPos := int64(0)
		for j := 0; j < int(spc) && sampleCursor < sampleCount; j++ {
			i := sampleCursor
			samples[i] = media.Sample{
				Index:    uint32(i + 1),
				DTS:      dts,
				PTS:      dts + composition[i],
				Duration: durations[i],
				Size:     sizes[i],
				Offset:   chunkOffset + chunkPos,
				Sync:     sync[uint32(i+1)],
			}
			chunkPos += int64(sizes[i])
			if durations[i] != 0 {
				dts += int64(durations[i])
			}
			sampleCursor++
		}
	}
	if sampleCursor < sampleCount {
		for i := sampleCursor; i < sampleCount; i++ {
			samples[i] = media.Sample{Index: uint32(i + 1), DTS: dts, PTS: dts + composition[i], Duration: durations[i], Size: sizes[i], Offset: -1, Sync: sync[uint32(i+1)]}
			if durations[i] != 0 {
				dts += int64(durations[i])
			}
		}
		faults = append(faults, media.NewFault(media.CodeBadSampleOffset, "chunk offsets cover only %d/%d samples", sampleCursor, sampleCount).
			WithTrack(trackID).WithBox("stco"))
	}
	if offsetBox != nil && len(chunkOffsets) == sampleCount {
		// Location helper used by validation when one chunk maps to one sample.
		_ = offsetBox
	}
	return samples, faults, nil
}

func readChunkOffsets(rs io.ReadSeeker, trackID uint32, stbl *Box) ([]int64, *Box, error) {
	if stcoBox := stbl.Find("stco"); stcoBox != nil {
		var stco gomp4.Stco
		if err := unmarshalTypedBox(rs, stcoBox, &stco); err != nil {
			return nil, stcoBox, media.NewFault(media.CodeInvalidInput, "decode stco: %v", err).WithTrack(trackID).WithBox("stco").Fatal()
		}
		out := make([]int64, len(stco.ChunkOffset))
		for i, v := range stco.ChunkOffset {
			out[i] = int64(v)
		}
		return out, stcoBox, nil
	}
	if co64Box := stbl.Find("co64"); co64Box != nil {
		var co64 gomp4.Co64
		if err := unmarshalTypedBox(rs, co64Box, &co64); err != nil {
			return nil, co64Box, media.NewFault(media.CodeInvalidInput, "decode co64: %v", err).WithTrack(trackID).WithBox("co64").Fatal()
		}
		out := make([]int64, len(co64.ChunkOffset))
		for i, v := range co64.ChunkOffset {
			out[i] = int64(v)
		}
		return out, co64Box, nil
	}
	return nil, nil, media.NewFault(media.CodeBadSampleOffset, "missing stco/co64 sample offsets").WithTrack(trackID).WithBox("stco")
}

func samplesPerChunk(entries []gomp4.StscEntry, chunk uint32) uint32 {
	spc := uint32(1)
	for _, e := range entries {
		if e.FirstChunk <= chunk && e.SamplesPerChunk > 0 {
			spc = e.SamplesPerChunk
		}
	}
	return spc
}

func detectTimelineFaults(t *media.Track) []*media.Fault {
	var out []*media.Fault
	clock, err := t.Clock()
	if err != nil {
		out = append(out, media.NewFault(media.CodeInvalidInput, "%v", err).WithTrack(t.ID).Fatal())
		return out
	}
	for i, s := range t.Samples {
		if s.Duration == 0 {
			out = append(out, media.NewFault(media.CodeTimestampMissing, "sample %d has zero duration", s.Index).
				WithTrack(t.ID).WithBox("stts").WithTimeSpan(s.DTS, s.PTS, clock))
		}
		if i > 0 && s.PTS < t.Samples[i-1].PTS {
			prev := t.Samples[i-1]
			out = append(out, media.NewFault(media.CodeTimestampRegression, "PTS regressed at sample %d: %d < %d", s.Index, s.PTS, prev.PTS).
				WithTrack(t.ID).WithBox("ctts").
				WithTimeSpan(prev.PTS, s.PTS, clock).
				WithOriginal(fmt.Sprintf("pts[%d]=%d pts[%d]=%d", prev.Index, prev.PTS, s.Index, s.PTS)))
		}
	}
	return out
}

func validateOffsets(t *media.Track, stbl *Box, fileSize int64, top []*Box) []*media.Fault {
	var out []*media.Fault
	stcoBox := stbl.Find("stco")
	for i, s := range t.Samples {
		if s.Offset < 0 {
			continue
		}
		end, ok := media.ByteRange{Offset: s.Offset, Length: int64(s.Size)}.End()
		if !ok || s.Offset > fileSize || end > fileSize {
			f := media.NewFault(media.CodeBadSampleOffset, "sample %d interval [%d,%d) exceeds file size %d", s.Index, s.Offset, end, fileSize).
				WithTrack(t.ID).WithBox("stco").
				WithByteRange(offsetEntryLocation(stcoBox, i), 4).
				WithOriginal(fmt.Sprintf("offset=%d size=%d", s.Offset, s.Size))
			if mdat := findTopBox(top, "mdat"); mdat != nil && s.Offset >= mdat.Offset && s.Offset < mdat.End() {
				f.Code = media.CodeTruncatedMDAT
			}
			out = append(out, f)
		}
	}
	// Same-track sample intervals must not overlap. Sorting only local indices
	// keeps this safe for non-interleaved and interleaved files alike.
	type interval struct {
		idx int
		start, end int64
	}
	ivs := make([]interval, 0, len(t.Samples))
	for i, s := range t.Samples {
		if s.Offset >= 0 {
			end, ok := media.ByteRange{Offset: s.Offset, Length: int64(s.Size)}.End()
			if ok {
				ivs = append(ivs, interval{i, s.Offset, end})
			}
		}
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].start < ivs[j].start })
	for i := 1; i < len(ivs); i++ {
		if ivs[i].start < ivs[i-1].end {
			sample := t.Samples[ivs[i].idx]
			out = append(out, media.NewFault(media.CodeBadSampleOffset, "sample %d overlaps previous sample interval at byte %d", sample.Index, ivs[i].start).
				WithTrack(t.ID).WithBox("stco").
				WithByteRange(offsetEntryLocation(stcoBox, int(sample.Index-1)), 4).
				WithOriginal(fmt.Sprintf("offset=%d", sample.Offset)))
		}
	}
	return out
}

func offsetEntryLocation(stco *Box, sampleZeroBased int) int64 {
	if stco == nil || sampleZeroBased < 0 {
		return 0
	}
	// FullBox header (4) + entry_count (4), then uint32 entries.
	return stco.Offset + stco.HeaderSize + 8 + int64(sampleZeroBased)*4
}
