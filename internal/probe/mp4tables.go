package probe

import (
	"github.com/abema/go-mp4"

	"mediaroute/internal/clock"
	"mediaroute/internal/fault"
)

// expand turns the raw MP4 boxes of one trak into a fully resolved
// TrackMeta, applying time-base, rotation, crop and colour validation.
func (tb *mp4TrackBuild) expand(raw []byte, rep *ContainerReport, containerColr *Colour) (*TrackMeta, *fault.Error) {
	if tb.mdhd == nil || tb.mdhd.Timescale == 0 {
		return nil, fault.New(fault.CatInvalidTimeBase,
			"missing or zero timescale").WithTrack(tb.trackID)
	}
	tbmd := clock.Rational{Num: int64(tb.mdhd.Timescale), Den: 1}
	if len(tb.stts.Entries) == 0 || tb.stsz == nil || len(tb.stco) == 0 {
		return nil, fault.New(fault.CatTruncatedSampleTable,
			"incomplete sample table").WithTrack(tb.trackID)
	}
	n := int(tb.stsz.SampleCount)
	if n > MaxSamples {
		return nil, fault.New(fault.CatLimitExceeded,
			"sample count %d exceeds limit", n).WithTrack(tb.trackID)
	}
	sizes, ferr := tb.sampleSizes(n)
	if ferr != nil {
		return nil, ferr
	}
	offsets, ferr := tb.sampleOffsets(sizes, raw)
	if ferr != nil {
		return nil, ferr
	}
	durs, ferr := tb.sampleTiming(n)
	if ferr != nil {
		return nil, ferr
	}
	tm := &TrackMeta{
		TrackID: tb.trackID, Handler: tb.handler, TimeBase: tbmd,
		Width: tb.tkhd.Width >> 16, Height: tb.tkhd.Height >> 16,
		Supported: supportedCodecs[tb.codec], Codec: tb.codec,
		Crop: tb.crop,
	}
	if tm.Codec == "" {
		tm.Codec = "unknown"
	}
	if rot, e := NormalizeRotation(tb.tkhd.Matrix, tb.trackID); e != nil {
		return nil, e
	} else {
		tm.Rotation = rot
	}
	if tb.crop != nil {
		if e := CheckCrop(tb.trackID, tm.Width, tm.Height, *tb.crop); e != nil {
			return nil, e
		}
	}
	tm.Colour = ArbitrateColour(tb.trackID, tb.colr, containerColr)
	if tm.Colour.Conflict {
		rep.RecordConflict(tb.trackID, "colour",
			"track and container colour descriptions differ",
			string(tm.Colour.Source))
	}
	if !tm.Supported {
		rep.Faults = append(rep.Faults, fault.New(fault.CatUnsupportedCodec,
			"codec %q is not supported; only structural scan available",
			tm.Codec).WithTrack(tb.trackID))
	}
	tm.Samples = make([]SampleInfo, n)
	var ticks int64
	for i := 0; i < n; i++ {
		nanos, e := tbmd.ToNanos(ticks)
		if e != nil {
			return nil, fault.New(fault.CatTimestampOverflow,
				"DTS overflow at sample %d", i).
				WithTrack(tb.trackID).WithSample(int64(i)).Wrap(e)
		}
		dn, e := tbmd.ToNanos(durs[i])
		if e != nil {
			return nil, fault.New(fault.CatTimestampOverflow,
				"duration overflow at sample %d", i).
				WithTrack(tb.trackID).WithSample(int64(i)).Wrap(e)
		}
		tm.Samples[i] = SampleInfo{
			Index: int64(i), Offset: offsets[i], Size: sizes[i],
			DTSTicks: ticks, DTSNanos: nanos,
			DurationTicks: durs[i], DurationNanos: dn,
			IsKeyframe: len(tb.stss) == 0 || tb.stss[uint32(i+1)],
		}
		ticks += durs[i]
	}
	return tm, nil
}

func (tb *mp4TrackBuild) sampleSizes(n int) ([]int64, *fault.Error) {
	sizes := make([]int64, n)
	if tb.stsz.SampleSize != 0 {
		for i := range sizes {
			sizes[i] = int64(tb.stsz.SampleSize)
		}
		return sizes, nil
	}
	if len(tb.stsz.EntrySize) < n {
		return nil, fault.New(fault.CatTruncatedSampleTable,
			"stsz declares %d samples but contains %d",
			n, len(tb.stsz.EntrySize)).WithTrack(tb.trackID)
	}
	for i := 0; i < n; i++ {
		sizes[i] = int64(tb.stsz.EntrySize[i])
	}
	return sizes, nil
}

func (tb *mp4TrackBuild) sampleOffsets(sizes []int64, raw []byte) ([]int64, *fault.Error) {
	n := len(sizes)
	offsets := make([]int64, n)
	spc, ferr := expandStsc(tb.stsc, n, tb.trackID)
	if ferr != nil {
		return nil, ferr
	}
	if len(tb.stco) == 0 {
		return nil, fault.New(fault.CatTruncatedSampleTable,
			"no chunk offsets").WithTrack(tb.trackID)
	}
	sampleIdx, cursor := 0, int64(0)
	for chunk := 0; chunk < len(tb.stco) && sampleIdx < n; chunk++ {
		count := spc[chunk]
		base := tb.stco[chunk]
		for j := 0; j < count && sampleIdx < n; j++ {
			offsets[sampleIdx] = base + cursor
			cursor += sizes[sampleIdx]
			sampleIdx++
		}
		cursor = 0
	}
	if sampleIdx < n {
		return nil, fault.New(fault.CatTruncatedSampleTable,
			"chunks cover %d of %d samples", sampleIdx, n).
			WithTrack(tb.trackID)
	}
	for i, off := range offsets {
		if off < 0 || off+sizes[i] > int64(len(raw)) {
			return nil, fault.New(fault.CatTruncatedSampleTable,
				"sample %d data [%d,+%d) outside container",
				i, off, sizes[i]).
				WithTrack(tb.trackID).WithSample(int64(i))
		}
	}
	return offsets, nil
}

// expandStsc turns the stsc run-length table into a samples-per-chunk
// table covering every chunk referenced by stco/co64.
func expandStsc(stsc *mp4.Stsc, n int, trackID uint32) ([]int, *fault.Error) {
	if stsc == nil || len(stsc.Entries) == 0 {
		return nil, fault.New(fault.CatTruncatedSampleTable,
			"missing stsc").WithTrack(trackID)
	}
	spc := []int{}
	for ci, e := range stsc.Entries {
		end := 0
		if ci+1 < len(stsc.Entries) {
			end = int(stsc.Entries[ci+1].FirstChunk) - 1
		} else {
			end = (n / int(e.SamplesPerChunk)) + 1
		}
		for len(spc) < end {
			spc = append(spc, int(e.SamplesPerChunk))
		}
	}
	return spc, nil
}

func (tb *mp4TrackBuild) sampleTiming(n int) ([]int64, *fault.Error) {
	durs := make([]int64, 0, n)
	for _, e := range tb.stts.Entries {
		if e.SampleDelta == 0 {
			return nil, fault.New(fault.CatInvalidTimeBase,
				"zero sample delta in stts").WithTrack(tb.trackID)
		}
		for k := uint32(0); k < e.SampleCount; k++ {
			durs = append(durs, int64(e.SampleDelta))
		}
	}
	if len(durs) < n {
		return nil, fault.New(fault.CatTruncatedSampleTable,
			"stts covers %d of %d samples", len(durs), n).
			WithTrack(tb.trackID)
	}
	return durs[:n], nil
}
