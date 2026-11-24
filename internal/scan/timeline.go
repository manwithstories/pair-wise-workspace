package scan

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"

	mp4 "github.com/abema/go-mp4"

	"mediaroute/internal/clock"
	merr "mediaroute/internal/errors"
	"mediaroute/internal/index"
	"mediaroute/internal/manifest"
)

// sampleLayout pairs each sample number with its file offset.
type sampleLayout struct {
	offsets []int64
	sizes   []uint32
}

// layoutSamples applies stsc to the chunk table and stsz to derive absolute
// byte offsets for every sample. Any inconsistency is reported with the chunk
// and offset involved.
func layoutSamples(meta *trackMeta) (*sampleLayout, error) {
	count := int(meta.stsz.SampleCount)
	if len(meta.stsz.EntrySize) > 0 {
		count = len(meta.stsz.EntrySize)
	}
	if count == 0 {
		return &sampleLayout{}, nil
	}
	if count > MaxSamplesPerTrack {
		return nil, merr.New(merr.KindScan, meta.trackID, -1, -1, false, "sample count %d exceeds limit", count)
	}
	out := &sampleLayout{offsets: make([]int64, count), sizes: make([]uint32, count)}
	for i := 0; i < count; i++ {
		if meta.stsz.SampleSize != 0 {
			out.sizes[i] = meta.stsz.SampleSize
		} else if i < len(meta.stsz.EntrySize) {
			out.sizes[i] = meta.stsz.EntrySize[i]
		}
	}
	// Expand stsc entries: entry j covers chunks [FirstChunk, next.FirstChunk).
	nchunks := len(meta.chunks)
	type run struct{ first, perChunk uint32 }
	runs := make([]run, 0, len(meta.stsc.Entries))
	for j, e := range meta.stsc.Entries {
		if e.FirstChunk == 0 || e.FirstChunk > uint32(nchunks) {
			continue
		}
		runs = append(runs, run{first: e.FirstChunk, perChunk: e.SamplesPerChunk})
		if j+1 < len(meta.stsc.Entries) && meta.stsc.Entries[j+1].FirstChunk <= e.FirstChunk {
			return nil, merr.New(merr.KindScan, meta.trackID, -1, -1, false, "stsc entries not strictly increasing at index %d", j)
		}
	}
	if len(runs) == 0 {
		return nil, merr.New(merr.KindScan, meta.trackID, -1, -1, false, "stsc references no valid chunk")
	}
	sample := 0
	for ci := 0; ci < nchunks && sample < count; ci++ {
		perChunk := uint32(0)
		for _, r := range runs {
			if uint32(ci+1) >= r.first {
				perChunk = r.perChunk
			}
		}
		cur := meta.chunks[ci]
		for k := uint32(0); k < perChunk && sample < count; k++ {
			out.offsets[sample] = int64(cur)
			if cur > uint64(math.MaxInt64)-uint64(out.sizes[sample]) {
				return nil, merr.New(merr.KindScan, meta.trackID, int64(sample), int64(cur), false, "sample offset overflow")
			}
			cur += uint64(out.sizes[sample])
			sample++
		}
	}
	if sample != count {
		return nil, merr.New(merr.KindScan, meta.trackID, int64(sample), -1, false,
			"chunk map covers %d of %d samples", sample, count)
	}
	return out, nil
}

// scanTrack builds the index entry for one manifest track. A non-nil track is
// returned alongside any recoverable error so valid prefix data survives.
func scanTrack(ctx context.Context, mt manifest.Track, path string, opts Options) (*index.Track, []index.CorruptRange, error) {
	meta, fileSize, err := readFileMeta(path)
	if err != nil {
		return nil, nil, err
	}
	if meta.trackID == 0 {
		meta.trackID = mt.ID
	}
	if meta.kind != mt.Kind {
		return nil, nil, merr.New(merr.KindManifest, mt.ID, -1, mt.Offset, false,
			"manifest declares %s but %s contains a %s track", mt.Kind, filepath.Base(path), meta.kind)
	}
	layout, err := layoutSamples(meta)
	if err != nil {
		return nil, nil, err
	}
	tb, err := clock.NewTimebase(meta.timescale, mt.OriginMS*int64(meta.timescale)/1000)
	if err != nil {
		return nil, nil, merr.New(merr.KindTimeline, mt.ID, -1, mt.Offset, false, "timebase: %v", err)
	}
	// EditMediaTicks is the media time of the first presented edit. Timeline
	// seconds are (DTS - mediaTime)/timescale + origin, so the origin shifts
	// the whole track without being folded into the edit offset (doing so
	// would double-count the shift for a non-zero media time).
	editOffset := firstMediaTime(meta)
	if editOffset < 0 {
		editOffset = 0
	}
	tr := &index.Track{
		ID:        mt.ID,
		Kind:      mt.Kind,
		Source:    mt.Source,
		Timescale: meta.timescale,
		Origin:    tb.Origin,
		Entry: index.SampleEntry{
			Type: meta.entry.typ, Width: meta.entry.width, Height: meta.entry.height,
			SampleRate: meta.entry.sampleRate, Channels: meta.entry.channels, Layout: layoutOf(mt),
		},
	}
	tr.EditMediaTicks = editOffset
	tr.Edits = buildEdits(meta, mt)
	// Pre-size the per-sample tables from the known sample count so the index
	// never reallocates a growing slice.
	tr.Samples = make([]index.Sample, 0, len(layout.sizes))
	tr.Keyframes = make([]int64, 0, min(len(layout.sizes), len(meta.stss)+1))

	dts := int64(0)
	runs := newRangeBuilder(tb, tr.ID, editOffset)
	f, openErr := os.Open(path)
	if openErr != nil {
		return tr, nil, merr.New(merr.KindScan, mt.ID, -1, -1, false, "open %s: %v", path, openErr)
	}
	defer f.Close()

	syncSet := make(map[int64]bool, len(meta.stss))
	for _, n := range meta.stss {
		syncSet[int64(n)] = true
	}
	hasStss := len(meta.stss) > 0
	var cttsIdx, cttsLeft int
	var cttsOffset int64
	var firstErr error
	for i, size := range layout.sizes {
		if err := ctx.Err(); err != nil {
			return tr, runs.close(), canceled(err)
		}
		num := int64(i + 1)
		delta, ok := sttsDelta(meta.stts, i)
		if !ok {
			firstErr = merr.New(merr.KindTimeline, mt.ID, num, -1, true, "stts shorter than sample table")
			break
		}
		// Advance composition offset runs when a ctts box is present.
		cts := dts
		if meta.ctts != nil {
			for cttsIdx < len(meta.ctts.Entries) && cttsLeft == 0 {
				cttsLeft = int(meta.ctts.Entries[cttsIdx].SampleCount)
				cttsOffset = cttsSampleOffset(meta.ctts, cttsIdx)
				if cttsLeft == 0 {
					cttsIdx++
				}
			}
			cts = dts + cttsOffset
			if cttsIdx < len(meta.ctts.Entries) && cttsLeft > 0 {
				cttsLeft--
				if cttsLeft == 0 {
					cttsIdx++
				}
			}
		}
		readable, data, rerr := readSampleBytes(ctx, f, layout.offsets[i], size, fileSize, opts.VerifyBytes)
		if rerr != nil {
			return tr, runs.close(), rerr
		}
		reason := ""
		if !readable {
			reason = "sample range past EOF"
		} else if opts.VerifyBytes && len(data) > 0 && allZero(data) {
			readable = false
			reason = "missing page (zeroed sample)"
		}
		sync := !hasStss || syncSet[num]
		tr.Samples = append(tr.Samples, index.Sample{
			Number: num, DTS: dts, CTS: cts, Duration: int64(delta),
			Offset: layout.offsets[i], Size: size,
			Sync: sync, Readable: readable,
		})
		if readable && sync {
			tr.Keyframes = append(tr.Keyframes, num)
		}
		if !readable {
			start, end, terr := rangeTimes(tb, editOffset, dts, int64(delta))
			if terr != nil {
				return tr, runs.close(), merr.New(merr.KindTimeline, mt.ID, num, layout.offsets[i], false, "range time: %v", terr)
			}
			runs.add(num, layout.offsets[i], reason, start, end)
		} else {
			runs.flush()
		}
		dts += int64(delta)
	}
	ranges := runs.close()
	if firstErr == nil && len(ranges) > 0 {
		r := ranges[0]
		kind := merr.KindTruncated
		if strings.Contains(r.Reason, "missing page") {
			kind = merr.KindScan
		}
		firstErr = merr.New(kind, mt.ID, r.FirstSample, r.ByteOffset, true,
			"%s: samples %d..%d isolated", r.Reason, r.FirstSample, r.LastSample)
	}
	if len(tr.Keyframes) > MaxKeyframesPerTrack {
		return tr, ranges, merr.New(merr.KindScan, mt.ID, -1, -1, true, "keyframe count exceeds limit")
	}
	if dts > editOffset {
		dur, derr := clock.FromTicks(dts-editOffset, meta.timescale)
		if derr == nil {
			tr.Duration = dur
		}
	}
	return tr, ranges, firstErr
}

// rangeTimes converts a sample's DTS span to absolute timeline seconds.
func rangeTimes(tb clock.Timebase, editOffset, dts, delta int64) (clock.Rational, clock.Rational, error) {
	startTicks := dts - editOffset
	if startTicks < 0 {
		startTicks = 0
	}
	endTicks := dts + int64(delta) - editOffset
	if endTicks < 0 {
		endTicks = 0
	}
	start, err := tb.At(startTicks)
	if err != nil {
		return clock.Rational{}, clock.Rational{}, err
	}
	end, err := tb.At(endTicks)
	if err != nil {
		return clock.Rational{}, clock.Rational{}, err
	}
	return start, end, nil
}

func sttsDelta(stts *mp4.Stts, idx int) (uint32, bool) {
	n := 0
	for _, e := range stts.Entries {
		if idx < n+int(e.SampleCount) {
			return e.SampleDelta, true
		}
		n += int(e.SampleCount)
	}
	return 0, false
}

func firstMediaTime(meta *trackMeta) int64 {
	for _, e := range meta.elst {
		if e.MediaTimeV0 == -1 {
			continue // empty edit
		}
		if e.MediaTimeV0 > 0 {
			return int64(e.MediaTimeV0)
		}
	}
	return 0
}

// buildEdits records the effective edit list. A manifest-declared edit list is
// authoritative when present; otherwise the MP4 elst is used. Both are kept in
// the index as exact rational times so the mapping can be audited.
func buildEdits(meta *trackMeta, mt manifest.Track) []index.Edit {
	if len(mt.Edits) > 0 {
		out := make([]index.Edit, 0, len(mt.Edits))
		for _, e := range mt.Edits {
			out = append(out, index.Edit{
				SegmentDuration: clock.Rational{Num: e.SegmentDurationMS, Den: 1000},
				MediaTime:       clock.Rational{Num: e.MediaTimeMS, Den: 1000},
				Empty:           false,
			})
		}
		return out
	}
	out := make([]index.Edit, 0, len(meta.elst))
	for _, e := range meta.elst {
		seg := clock.Rational{Num: int64(e.SegmentDurationV0), Den: int64(meta.movieTS)}
		if meta.movieTS == 0 {
			seg = clock.Rational{Num: int64(e.SegmentDurationV0), Den: 1000}
		}
		mtv := e.MediaTimeV0
		empty := mtv == -1
		mtokens := int64(mtv)
		if mtokens < 0 {
			mtokens = 0
		}
		out = append(out, index.Edit{
			SegmentDuration: seg,
			MediaTime:       clock.Rational{Num: mtokens, Den: int64(meta.timescale)},
			Empty:           empty,
		})
	}
	return out
}

// cttsSampleOffset returns the signed composition offset of a ctts entry.
func cttsSampleOffset(ctts *mp4.Ctts, idx int) int64 {
	if idx < 0 || idx >= len(ctts.Entries) {
		return 0
	}
	if ctts.GetVersion() == 1 {
		return int64(ctts.Entries[idx].SampleOffsetV1)
	}
	return int64(ctts.Entries[idx].SampleOffsetV0)
}
