package demux

import (
	"bytes"
	"io"

	mp4 "github.com/abema/go-mp4"

	"mediacore/internal/fault"
	"mediacore/internal/mp4box"
	"mediacore/internal/sample"
)

func bytesReader(b []byte) io.ReadSeeker { return bytes.NewReader(b) }

// Container is a parsed container ready for sample iteration.
type Container struct {
	Reader *Reader
	Tracks []*TrackMeta
	Mdat   *mp4box.Header
	// Faults holds recoverable (skip-fragment) faults; structural faults are
	// returned as hard errors from Open.
	Faults []*fault.Fault
	// MissingIndex is set when a track had no stco/co64 and was rebuilt.
	MissingIndex map[int]bool
}

// Open parses ftyp/moov and validates every required codec configuration.
// A missing required config or unknown codec aborts with an offset-bearing
// fault; no partial-success summary is produced.
func (r *Reader) Open() (*Container, error) {
	tops, err := r.TopLevel()
	if err != nil {
		return nil, err
	}
	var moov, mdat *mp4box.Header
	brands := map[string]bool{}
	for _, h := range tops {
		switch {
		case h.Type == mp4.BoxTypeFtyp():
			brands = r.readBrands(h)
		case h.Type == mp4.BoxTypeMoov():
			moov = h
		case h.Type == mp4.BoxTypeMdat():
			mdat = h
		}
	}
	if moov == nil {
		return nil, fault.New(fault.KindTruncatedHeader, fault.Abort, r.fileSz,
			"moov box not found")
	}
	if mdat == nil {
		return nil, fault.New(fault.KindTruncatedHeader, fault.Abort, r.fileSz,
			"mdat box not found")
	}
	if f := mp4box.TailFragment(r.rs, tops, r.fileSz); f != nil {
		return nil, f
	}
	r.mdatPayload = mdat.PayloadOff
	r.mdatEnd = mdat.End
	if r.mdatEnd < 0 {
		r.mdatEnd = r.fileSz
	}
	traks, err := mp4box.Children(r.rs, moov, r.fileSz)
	if err != nil {
		return nil, err
	}
	c := &Container{Reader: r, Mdat: mdat, MissingIndex: r.missing}
	var trakHeaders []*mp4box.Header
	for _, th := range traks {
		if th.Type == mp4.BoxTypeTrak() {
			trakHeaders = append(trakHeaders, th)
		}
	}
	if len(trakHeaders) > r.lim.MaxTracks {
		return nil, fault.New(fault.KindLimitExceeded, fault.Abort, traks[0].Offset,
			"track count exceeds limit")
	}
	for idx, th := range trakHeaders {
		tm, _, ferr := r.parseTrack(th, idx)
		if ferr != nil {
			return nil, ferr // structural: abort, no partial summary
		}
		c.Tracks = append(c.Tracks, tm.meta)
	}
	_ = brands
	return c, nil
}

func (r *Reader) readBrands(h *mp4box.Header) map[string]bool {
	out := map[string]bool{}
	data, err := mp4box.Payload(r.rs, h, r.lim.MaxBoxPayload)
	if err != nil || len(data) < 8 {
		return out
	}
	out[string(data[0:4])] = true
	for i := 8; i+4 <= len(data); i += 4 {
		out[string(data[i:i+4])] = true
	}
	return out
}

// parseTrack returns meta plus either a hard fault or a recoverable skip.
func (r *Reader) parseTrack(trak *mp4box.Header, idx int) (parsed, bool, *fault.Fault) {
	children, err := mp4box.Children(r.rs, trak, r.fileSz)
	if err != nil {
		return parsed{}, false, fault.As(err)
	}
	mdia := mp4box.Find(children, mp4.BoxTypeMdia())
	if mdia == nil {
		return parsed{}, false, fault.New(fault.KindMissingIndex, fault.Abort, trak.Offset,
			"trak has no mdia").WithTrack(0, idx)
	}
	mdChildren, err := mp4box.Children(r.rs, mdia, r.fileSz)
	if err != nil {
		return parsed{}, false, fault.As(err)
	}
	mdhd := mp4box.Find(mdChildren, mp4.BoxTypeMdhd())
	if mdhd == nil {
		return parsed{}, false, fault.New(fault.KindMissingIndex, fault.Abort, mdia.Offset,
			"mdhd missing").WithTrack(0, idx)
	}
	var mdhdBox mp4.Mdhd
	if err := r.unmarshal(mdhd, &mdhdBox); err != nil {
		return parsed{}, false, fault.As(err).WithTrack(0, idx)
	}
	minf := mp4box.Find(mdChildren, mp4.BoxTypeMinf())
	if minf == nil {
		return parsed{}, false, fault.New(fault.KindMissingIndex, fault.Abort, mdia.Offset,
			"minf missing").WithTrack(0, idx)
	}
	codec, codecOff, cerr := r.detectCodec(minf, idx)
	if cerr != nil {
		return parsed{}, false, cerr
	}
	trackID := r.readTrackID(children, idx)
	stbl, ferr := r.findStbl(minf)
	if ferr != nil {
		return parsed{}, false, ferr.WithTrack(trackID, idx)
	}
	meta := &TrackMeta{Index: idx, TrackID: trackID, Codec: codec,
		Timescale: mdhdBox.Timescale, Keyframes: map[int]bool{}}
	if ferr := r.resolveTables(meta, stbl, r.mdatPayload); ferr != nil {
		return parsed{}, false, ferr
	}
	_ = codecOff
	return parsed{meta: meta}, false, nil
}

// readTrackID parses tkhd; a failure yields a synthetic 1-based ID so the
// fault model still has a stable identifier.
func (r *Reader) readTrackID(trakKids []*mp4box.Header, idx int) uint32 {
	tkhd := mp4box.Find(trakKids, mp4.BoxTypeTkhd())
	if tkhd == nil {
		return uint32(idx + 1)
	}
	var box mp4.Tkhd
	if err := r.unmarshal(tkhd, &box); err != nil {
		return uint32(idx + 1)
	}
	if box.TrackID == 0 {
		return uint32(idx + 1)
	}
	return box.TrackID
}

type parsed struct {
	meta  *TrackMeta
	fault *fault.Fault
}

// detectCodec validates the sample entry and its required codec config.
func (r *Reader) detectCodec(minf *mp4box.Header, idx int) (sample.Codec, int64, *fault.Fault) {
	stbl, err := r.findStbl(minf)
	if err != nil {
		return "", 0, err
	}
	kids, cerr := mp4box.Children(r.rs, stbl, r.fileSz)
	if cerr != nil {
		return "", 0, fault.As(cerr).WithTrack(0, idx)
	}
	stsd := mp4box.Find(kids, mp4.BoxTypeStsd())
	if stsd == nil {
		return "", 0, fault.New(fault.KindMissingIndex, fault.Abort, stbl.Offset,
			"stsd missing").WithTrack(0, idx)
	}
	// The stsd payload starts with a full-box header and entry count, and each
	// sample entry carries fixed fields before its child boxes, so this layer
	// is parsed through go-mp4 rather than the generic box scanner.
	// The stsd payload starts with a full-box header and entry count, and each
	// sample entry carries fixed fields before its child boxes, so this layer
	// is parsed through go-mp4 using full root-relative paths.
	deep, derr := mp4.ExtractBoxes(r.rs, nil, []mp4.BoxPath{
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
			mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeAvc1(), mp4.BoxTypeAvcC()},
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
			mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeMp4a(), mp4.BoxTypeEsds()},
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
			mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeMp4a(), mp4.BoxTypeDOps()},
	})
	if derr != nil {
		return "", 0, fault.New(fault.KindCorruptSample, fault.Abort, stsd.Offset,
			"stsd parse failed: "+derr.Error()).WithTrack(0, idx)
	}
	entries, eerr := mp4.ExtractBoxes(r.rs, nil, []mp4.BoxPath{
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
			mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeAvc1()},
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
			mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeMp4a()},
	})
	if eerr != nil {
		return "", 0, fault.New(fault.KindCorruptSample, fault.Abort, stsd.Offset,
			"stsd entries parse failed: "+eerr.Error()).WithTrack(0, idx)
	}
	var avc1, mp4a *mp4.BoxInfo
	for _, b := range entries {
		// Select only the entry that belongs to this track's stsd region.
		if b.Offset < uint64(stsd.Offset) || b.Offset >= uint64(stsd.Offset)+uint64(stsd.Size) {
			continue
		}
		switch b.Type {
		case mp4.BoxTypeAvc1():
			if avc1 == nil {
				avc1 = b
			}
		case mp4.BoxTypeMp4a():
			if mp4a == nil {
				mp4a = b
			}
		}
	}
	var hasAvcC, hasEsds, hasDOps bool
	for _, b := range deep {
		if b.Offset < uint64(stsd.Offset) || b.Offset >= uint64(stsd.Offset)+uint64(stsd.Size) {
			continue
		}
		switch b.Type {
		case mp4.BoxTypeAvcC():
			hasAvcC = true
		case mp4.BoxTypeEsds():
			hasEsds = true
		case mp4.BoxTypeDOps():
			hasDOps = true
		}
	}
	switch {
	case avc1 != nil:
		if !hasAvcC {
			return "", 0, fault.New(fault.KindUnknownCodec, fault.Abort, int64(avc1.Offset),
				"avc1 missing required avcC config").WithTrack(0, idx)
		}
		return sample.CodecH264, int64(avc1.Offset), nil
	case mp4a != nil:
		if hasEsds {
			return sample.CodecAAC, int64(mp4a.Offset), nil
		}
		if hasDOps {
			return sample.CodecOpus, int64(mp4a.Offset), nil
		}
		return "", 0, fault.New(fault.KindUnknownCodec, fault.Abort, int64(mp4a.Offset),
			"mp4a missing esds/dOps config").WithTrack(0, idx)
	default:
		return "", 0, fault.New(fault.KindUnknownCodec, fault.Abort, stsd.PayloadOff+4,
			"unsupported or empty sample description").WithTrack(0, idx)
	}
}

// findStbl walks minf -> stbl.
func (r *Reader) findStbl(minf *mp4box.Header) (*mp4box.Header, *fault.Fault) {
	kids, err := mp4box.Children(r.rs, minf, r.fileSz)
	if err != nil {
		return nil, fault.As(err)
	}
	stbl := mp4box.Find(kids, mp4.BoxTypeStbl())
	if stbl == nil {
		return nil, fault.New(fault.KindMissingIndex, fault.Abort, minf.Offset,
			"stbl missing")
	}
	return stbl, nil
}
