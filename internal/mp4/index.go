package mp4

import (
	"bytes"
	"fmt"
	"os"
	"sort"

	"github.com/abema/go-mp4"

	"pairflow/media/internal/clock"
)

// Sample is one decode unit on the shared time axis.
type Sample struct {
	Number     int            `json:"number"`
	Offset     uint64         `json:"offset"`
	Size       uint32         `json:"size"`
	DTS        clock.Rational `json:"dts"`
	PTS        clock.Rational `json:"pts"`
	Sync       bool           `json:"sync"`
	Duplicate  bool           `json:"duplicate_pts"`
	OutOfBound bool           `json:"out_of_bounds"`
}

// TrackIndex is the parsed structure of one track.
type TrackIndex struct {
	ID          uint32               `json:"id"`
	Handler     string               `json:"handler"`
	Timescale   uint32               `json:"timescale"`
	Duration    clock.Rational       `json:"duration"`
	Samples     []Sample             `json:"samples"`
	Keyframes   []int                `json:"keyframe_numbers"`
	VFR         bool                 `json:"vfr"`
	MaxDTS      clock.Rational       `json:"max_dts"`
	Missing     int                  `json:"missing_samples"`
	OutOfBounds int                  `json:"out_of_bounds"`
	Errors      []*clock.SourceError `json:"errors"`
}

// Index is the parsed content of one file.
type Index struct {
	Path   string               `json:"path"`
	Size   int64                `json:"size_bytes"`
	Tracks []TrackIndex         `json:"tracks"`
	Errors []*clock.SourceError `json:"errors"`
}

// KeyframeHit reports whether the keyframe index can resolve a target
// time to a real sync sample.
type KeyframeHit struct {
	Target   clock.Rational `json:"target"`
	Matched  bool           `json:"matched"`
	Number   int            `json:"sample_number"`
	PTS      clock.Rational `json:"pts"`
	InBounds bool           `json:"in_bounds"`
	Distance clock.Rational `json:"distance"`
}

// rawTrack accumulates boxes before the sample table is decoded.
type rawTrack struct {
	offset    uint64
	id        uint32
	handler   string
	timescale uint32
	duration  uint64
	stts      *mp4.Stts
	stsc      *mp4.Stsc
	stsz      *mp4.Stsz
	stco      *mp4.Stco
	co64      *mp4.Co64
	stss      *mp4.Stss
	ctts      *mp4.Ctts
	errors    []*clock.SourceError
}

// ParseFile reads an MP4 from disk and indexes its sample tables.
func ParseFile(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseBytes(path, data)
}

// ParseBytes indexes an in-memory MP4.
func ParseBytes(path string, data []byte) (*Index, error) {
	idx := &Index{Path: path, Size: int64(len(data))}
	r := bytes.NewReader(data)
	var tracks []*rawTrack
	var cur *rawTrack

	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		p := h.Path
		if len(p) == 0 {
			return nil, nil
		}
		last := p[len(p)-1].String()
		switch last {
		case "moov", "mdia", "minf", "stbl", "edts", "dinf", "udta":
			_, err := h.Expand()
			return nil, err
		case "trak":
			cur = &rawTrack{offset: h.BoxInfo.Offset}
			tracks = append(tracks, cur)
			_, err := h.Expand()
			return nil, err
		}
		if cur == nil || !insideTrak(p) {
			return nil, nil
		}
		switch last {
		case "tkhd", "mdhd", "hdlr", "stts", "stsc", "stsz", "stco", "co64", "stss", "ctts":
		default:
			return nil, nil
		}
		box, _, err := h.ReadPayload()
		if err != nil {
			cur.errors = append(cur.errors, &clock.SourceError{
				Code: "box_parse", Source: "mp4", Offset: int64(h.BoxInfo.Offset),
				Message: "cannot parse " + last + " box", Err: err,
			})
			return nil, nil
		}
		switch b := box.(type) {
		case *mp4.Tkhd:
			cur.id = b.TrackID
		case *mp4.Mdhd:
			cur.timescale = b.Timescale
			cur.duration = b.GetDuration()
		case *mp4.Hdlr:
			cur.handler = string(bytes.TrimRight(b.HandlerType[:], "\x00"))
		case *mp4.Stts:
			cur.stts = b
		case *mp4.Stsc:
			cur.stsc = b
		case *mp4.Stsz:
			cur.stsz = b
		case *mp4.Stco:
			cur.stco = b
		case *mp4.Co64:
			cur.co64 = b
		case *mp4.Stss:
			cur.stss = b
		case *mp4.Ctts:
			cur.ctts = b
		}
		return nil, nil
	}

	if _, err := mp4.ReadBoxStructure(r, handler); err != nil {
		consumed := int64(r.Size() - int64(r.Len()))
		idx.Errors = append(idx.Errors, &clock.SourceError{
			Code: "parse_aborted", Source: "mp4", Offset: consumed,
			Message: "box traversal stopped", Err: err,
		})
	}
	if len(tracks) == 0 {
		idx.Errors = append(idx.Errors, clock.NewSourceError("mp4", "no_tracks", 0, "no trak boxes found"))
		return idx, nil
	}
	for _, rt := range tracks {
		ti := buildTrack(rt, idx.Size)
		idx.Tracks = append(idx.Tracks, ti)
		idx.Errors = append(idx.Errors, ti.Errors...)
	}
	return idx, nil
}

// insideTrak reports whether the path runs inside a trak box.
func insideTrak(p mp4.BoxPath) bool {
	for _, bt := range p {
		if bt.String() == "trak" {
			return true
		}
	}
	return false
}

func buildTrack(rt *rawTrack, fileSize int64) TrackIndex {
	ti := TrackIndex{ID: rt.id, Handler: rt.handler, Timescale: rt.timescale}
	ti.Errors = append(ti.Errors, rt.errors...)
	if rt.timescale == 0 {
		ti.Errors = append(ti.Errors, clock.NewSourceError("mp4", "zero_timescale", int64(rt.offset),
			"track has no timescale"))
		return ti
	}
	base := clock.NewBase(1, int64(rt.timescale))
	ti.Duration = base.Time(int64(rt.duration))

	var deltas []uint32
	if rt.stts != nil {
		for _, e := range rt.stts.Entries {
			for i := uint32(0); i < e.SampleCount; i++ {
				deltas = append(deltas, e.SampleDelta)
			}
		}
	}
	count := len(deltas)
	if rt.stsz != nil && int(rt.stsz.SampleCount) > count {
		ti.Missing = int(rt.stsz.SampleCount) - count
		ti.Errors = append(ti.Errors, clock.Errorf("mp4", "missing_samples", int64(rt.offset),
			"stsz declares %d samples but stts covers %d", rt.stsz.SampleCount, count))
	}
	dtsTicks := make([]int64, count)
	var acc int64
	for i, d := range deltas {
		dtsTicks[i] = acc
		acc += int64(d)
	}
	if count > 1 {
		for i := 1; i < len(deltas); i++ {
			if deltas[i] != deltas[0] {
				ti.VFR = true
				break
			}
		}
	}

	sizes := make([]uint32, count)
	if rt.stsz == nil {
		ti.Missing = count
		ti.Errors = append(ti.Errors, clock.NewSourceError("mp4", "missing_stsz", int64(rt.offset),
			"no sample size table"))
	} else {
		for i := 0; i < count; i++ {
			switch {
			case rt.stsz.SampleSize != 0:
				sizes[i] = rt.stsz.SampleSize
			case i < len(rt.stsz.EntrySize):
				sizes[i] = rt.stsz.EntrySize[i]
			default:
				ti.Missing++
				ti.Errors = append(ti.Errors, clock.Errorf("mp4", "truncated_stsz", int64(rt.offset),
					"sample %d has no size entry", i+1))
			}
		}
	}

	var chunkOffsets []uint64
	switch {
	case rt.stco != nil:
		for _, o := range rt.stco.ChunkOffset {
			chunkOffsets = append(chunkOffsets, uint64(o))
		}
	case rt.co64 != nil:
		chunkOffsets = append(chunkOffsets, rt.co64.ChunkOffset...)
	}
	offsets, offErrs := sampleOffsets(rt.stsc, chunkOffsets, sizes, count)
	ti.Errors = append(ti.Errors, offErrs...)

	syncSet := make(map[int]bool, count)
	if rt.stss == nil {
		for i := 0; i < count; i++ {
			syncSet[i+1] = true
		}
	} else {
		for _, n := range rt.stss.SampleNumber {
			syncSet[int(n)] = true
		}
	}

	cttsOffsets := make([]int64, count)
	if rt.ctts != nil {
		idx := 0
		for ei := range rt.ctts.Entries {
			e := rt.ctts.Entries[ei]
			for i := uint32(0); i < e.SampleCount && idx < count; i++ {
				cttsOffsets[idx] = rt.ctts.GetSampleOffset(ei)
				idx++
			}
		}
	}

	seenPTS := make(map[clock.Rational]int, count)
	for i := 0; i < count; i++ {
		s := Sample{
			Number: i + 1,
			Size:   sizes[i],
			DTS:    base.Time(dtsTicks[i]),
			PTS:    base.Time(dtsTicks[i] + cttsOffsets[i]),
			Sync:   syncSet[i+1],
		}
		if i < len(offsets) {
			s.Offset = offsets[i]
		}
		if s.Offset+uint64(s.Size) > uint64(fileSize) {
			s.OutOfBound = true
			ti.OutOfBounds++
			ti.Errors = append(ti.Errors, clock.Errorf("mp4", "sample_out_of_bounds", int64(s.Offset),
				"sample %d spans [%d,%d) beyond file size %d", s.Number, s.Offset, s.Offset+uint64(s.Size), fileSize))
		}
		if first, ok := seenPTS[s.PTS]; ok {
			s.Duplicate = true
			ti.Errors = append(ti.Errors, clock.Errorf("mp4", "duplicate_pts", int64(s.Offset),
				"sample %d repeats PTS of sample %d (%s)", s.Number, first, s.PTS))
		} else {
			seenPTS[s.PTS] = s.Number
		}
		if s.DTS.Cmp(ti.MaxDTS) > 0 {
			ti.MaxDTS = s.DTS
		}
		if s.Sync {
			ti.Keyframes = append(ti.Keyframes, s.Number)
		}
		ti.Samples = append(ti.Samples, s)
	}
	return ti
}

// sampleOffsets maps chunk offsets and the sample-to-chunk table onto
// per-sample file offsets. Inconsistent tables produce errors rather
// than a guessed layout.
func sampleOffsets(stsc *mp4.Stsc, chunks []uint64, sizes []uint32, count int) ([]uint64, []*clock.SourceError) {
	out := make([]uint64, 0, count)
	var errs []*clock.SourceError
	if stsc == nil || len(stsc.Entries) == 0 {
		return out, append(errs, clock.NewSourceError("mp4", "missing_stsc", -1,
			"no sample-to-chunk table"))
	}
	if len(chunks) == 0 {
		return out, append(errs, clock.NewSourceError("mp4", "missing_chunks", -1,
			"no chunk offset table"))
	}
	sample := 0
	for ei, e := range stsc.Entries {
		first := int(e.FirstChunk)
		next := len(chunks) + 1
		if ei+1 < len(stsc.Entries) {
			next = int(stsc.Entries[ei+1].FirstChunk)
		}
		if first < 1 || first > len(chunks) || next < first {
			errs = append(errs, clock.Errorf("mp4", "bad_stsc", -1,
				"chunk run %d..%d outside %d chunks", first, next-1, len(chunks)))
			break
		}
		for c := first; c < next; c++ {
			off := chunks[c-1]
			for k := uint32(0); k < e.SamplesPerChunk && sample < count; k++ {
				out = append(out, off)
				off += uint64(sizes[sample])
				sample++
			}
		}
	}
	if sample < count {
		errs = append(errs, clock.Errorf("mp4", "truncated_stsc", -1,
			"sample table describes %d of %d samples", sample, count))
	}
	return out, errs
}

// NearestKeyframe returns the latest sync sample whose decode time is
// at or before target.
func (t *TrackIndex) NearestKeyframe(target clock.Rational) (Sample, bool) {
	best := Sample{}
	found := false
	for _, s := range t.Samples {
		if !s.Sync || s.DTS.Cmp(target) > 0 {
			continue
		}
		if !found || s.DTS.Cmp(best.DTS) >= 0 {
			best, found = s, true
		}
	}
	return best, found
}

// LookupKeyframes resolves each target against the keyframe index and
// reports whether the hit is a real, in-bounds sync sample.
func (t *TrackIndex) LookupKeyframes(targets []clock.Rational) []KeyframeHit {
	hits := make([]KeyframeHit, 0, len(targets))
	for _, target := range targets {
		s, ok := t.NearestKeyframe(target)
		h := KeyframeHit{Target: target, Matched: ok, Number: -1}
		if ok {
			h.Number = s.Number
			h.PTS = s.PTS
			h.InBounds = !s.OutOfBound
			h.Distance = target.Sub(s.DTS)
		}
		hits = append(hits, h)
	}
	return hits
}

// KeyframeHitRate returns matched-and-in-bounds hits divided by total.
func KeyframeHitRate(hits []KeyframeHit) clock.Rational {
	if len(hits) == 0 {
		return clock.Zero()
	}
	good := 0
	for _, h := range hits {
		if h.Matched && h.InBounds {
			good++
		}
	}
	return clock.NewRational(int64(good), int64(len(hits)))
}

// SortSamplesByPTS returns a copy ordered by PTS; ties keep decode
// order.
func (t *TrackIndex) SortSamplesByPTS() []Sample {
	out := append([]Sample(nil), t.Samples...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].PTS.LT(out[j].PTS) })
	return out
}

var _ = fmt.Sprintf
