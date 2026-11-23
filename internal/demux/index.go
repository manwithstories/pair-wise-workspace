package demux

import (
	mp4 "github.com/abema/go-mp4"

	"mediacore/internal/fault"
	"mediacore/internal/mp4box"
)

// resolveTables fills chunk offsets, sizes, timing and keyframes for a track.
// A missing stco/co64 triggers reconstruction from the framed mdat; an offset
// pointing outside the file is an out-of-range structural fault.
func (r *Reader) resolveTables(meta *TrackMeta, stbl *mp4box.Header, mdatPayloadOff int64) *fault.Fault {
	kids, err := mp4box.Children(r.rs, stbl, r.fileSz)
	if err != nil {
		return fault.As(err).WithTrack(0, meta.Index)
	}
	stszH := mp4box.Find(kids, mp4.BoxTypeStsz())
	if stszH == nil {
		return fault.New(fault.KindMissingIndex, fault.Abort, stbl.Offset,
			"stsz missing").WithTrack(0, meta.Index)
	}
	var stsz mp4.Stsz
	if err := r.unmarshal(stszH, &stsz); err != nil {
		return fault.As(err).WithTrack(0, meta.Index)
	}
	if int(stsz.SampleCount) > r.lim.MaxSamples {
		return fault.New(fault.KindLimitExceeded, fault.Abort, stszH.Offset,
			"sample count exceeds limit").WithTrack(0, meta.Index)
	}
	meta.Samples = int(stsz.SampleCount)
	meta.sizes = make([]uint32, stsz.SampleCount)
	for i := range meta.sizes {
		if stsz.SampleSize != 0 {
			meta.sizes[i] = stsz.SampleSize
		} else {
			meta.sizes[i] = stsz.EntrySize[i]
		}
		if int(meta.sizes[i]) > r.lim.MaxSampleBytes {
			return fault.New(fault.KindLimitExceeded, fault.Abort, stszH.Offset,
				"sample size exceeds limit").WithTrack(0, meta.Index)
		}
	}
	if f := r.parseTiming(meta, kids); f != nil {
		return f
	}
	r.parseKeyframes(meta, kids)

	stcoH := mp4box.Find(kids, mp4.BoxTypeStco())
	co64H := mp4box.Find(kids, mp4.BoxTypeCo64())
	if stcoH == nil && co64H == nil {
		// Missing index: rebuild from self-describing mdat frames.
		r.missing[meta.Index] = true
		return r.rebuildIndex(meta, mdatPayloadOff)
	}
	var baseOff int64
	if co64H != nil {
		var co64 mp4.Co64
		if err := r.unmarshal(co64H, &co64); err != nil {
			return fault.As(err).WithTrack(0, meta.Index)
		}
		if len(co64.ChunkOffset) == 0 {
			return fault.New(fault.KindIndexOutOfRange, fault.Abort, co64H.Offset,
				"empty co64").WithTrack(0, meta.Index)
		}
		baseOff = int64(co64.ChunkOffset[0])
	} else {
		var stco mp4.Stco
		if err := r.unmarshal(stcoH, &stco); err != nil {
			return fault.As(err).WithTrack(0, meta.Index)
		}
		if len(stco.ChunkOffset) == 0 {
			return fault.New(fault.KindIndexOutOfRange, fault.Abort, stcoH.Offset,
				"empty stco").WithTrack(0, meta.Index)
		}
		baseOff = int64(stco.ChunkOffset[0])
	}
	if baseOff < 0 || baseOff >= r.fileSz {
		return fault.New(fault.KindIndexOutOfRange, fault.Abort, baseOff,
			"chunk offset outside file bounds").WithTrack(0, meta.Index)
	}
	meta.chunkOff = make([]int64, meta.Samples)
	pos := baseOff
	for i := 0; i < meta.Samples; i++ {
		meta.chunkOff[i] = pos
		pos += int64(meta.sizes[i])
	}
	// A region extending past EOF is not fatal here: the per-sample reader
	// reports each truncated sample as a skip-fragment fault. Only an explicit
	// offset beyond the file (checked above) is a structural abort.
	return nil
}

func (r *Reader) parseTiming(meta *TrackMeta, kids []*mp4box.Header) *fault.Fault {
	sttsH := mp4box.Find(kids, mp4.BoxTypeStts())
	if sttsH == nil {
		return fault.New(fault.KindMissingIndex, fault.Abort, 0,
			"stts missing").WithTrack(0, meta.Index)
	}
	var stts mp4.Stts
	if err := r.unmarshal(sttsH, &stts); err != nil {
		return fault.As(err).WithTrack(0, meta.Index)
	}
	var total uint32
	for _, e := range stts.Entries {
		meta.sttsRuns = append(meta.sttsRuns, sttsRun{e.SampleCount, e.SampleDelta})
		total += e.SampleCount
	}
	if int(total) != meta.Samples {
		return fault.New(fault.KindCorruptSample, fault.Abort, sttsH.Offset,
			"stts sample count disagrees with stsz").WithTrack(0, meta.Index)
	}
	return nil
}

func (r *Reader) parseKeyframes(meta *TrackMeta, kids []*mp4box.Header) {
	stssH := mp4box.Find(kids, mp4.BoxTypeStss())
	if stssH == nil {
		return // audio track or all-samples-keyframe
	}
	var stss mp4.Stss
	if err := r.unmarshal(stssH, &stss); err != nil {
		return
	}
	for _, n := range stss.SampleNumber {
		if int(n) >= 1 && int(n) <= meta.Samples {
			meta.Keyframes[int(n)-1] = true
		}
	}
}
