package adapter

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/abema/go-mp4"

	"mediad/internal/media"
)

// CMAFV1 reads CMAF / fragmented-MP4 segments. Version 1 covers ISO/IEC
// 14496-12 with moof/traf/trun addressing and tfdt base decode times.
type CMAFV1 struct{}

// Kind implements Adapter.
func (*CMAFV1) Kind() Container { return ContainerCMAF }

// Version implements Adapter.
func (*CMAFV1) Version() int { return 1 }

// rawBox is one top-level box located in a segment buffer.
type rawBox struct {
	typ        string
	offset     int
	size       int
	headerSize int
}

// splitTopLevel walks 32/64-bit box headers. It returns a classified
// ERR_TRUNCATED_BOX when a declared size runs past the buffer, so damaged
// segments are rejected rather than partially processed.
func splitTopLevel(data []byte, pos media.SourcePos) ([]rawBox, error) {
	var out []rawBox
	off := 0
	for off < len(data) {
		if len(data)-off < 8 {
			return nil, media.New(media.CodeTruncatedBox, "cmaf-split", pos,
				"%d trailing bytes cannot form a box header at offset %d", len(data)-off, off)
		}
		size := int(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		hdr := 8
		switch size {
		case 0:
			size = len(data) - off
		case 1:
			if len(data)-off < 16 {
				return nil, media.New(media.CodeTruncatedBox, "cmaf-split", pos,
					"box %q at offset %d declares 64-bit size but has no extent", typ, off)
			}
			size = int(binary.BigEndian.Uint64(data[off+8:]))
			hdr = 16
		}
		if size < hdr {
			return nil, media.New(media.CodeSegmentParse, "cmaf-split", pos,
				"box %q at offset %d has invalid size %d", typ, off, size)
		}
		if off+size > len(data) {
			return nil, media.New(media.CodeTruncatedBox, "cmaf-split", pos,
				"box %q at offset %d declares size %d but only %d bytes remain",
				typ, off, size, len(data)-off)
		}
		out = append(out, rawBox{typ: typ, offset: off, size: size, headerSize: hdr})
		off += size
	}
	return out, nil
}

// ParseInit implements Adapter.
func (*CMAFV1) ParseInit(data []byte, pos media.SourcePos) (*InitSegment, error) {
	boxes, err := splitTopLevel(data, pos)
	if err != nil {
		return nil, err
	}
	var moov *rawBox
	for i := range boxes {
		if boxes[i].typ == "moov" {
			moov = &boxes[i]
			break
		}
	}
	if moov == nil {
		return nil, media.New(media.CodeSegmentParse, "cmaf-init", pos, "no moov box in initialization segment")
	}
	rs := bytes.NewReader(data)
	moovBI := &mp4.BoxInfo{
		Offset:     uint64(moov.offset),
		Size:       uint64(moov.size),
		HeaderSize: uint64(moov.headerSize),
		Type:       mp4.BoxTypeMoov(),
	}
	traks, err := mp4.ExtractBoxes(rs, moovBI, []mp4.BoxPath{{mp4.BoxTypeTrak()}})
	if err != nil {
		return nil, media.Wrap(media.CodeSegmentParse, "cmaf-init", pos, err, "scan traks")
	}
	init := &InitSegment{Raw: append([]byte(nil), data...)}
	for _, trakBI := range traks {
		t, err := parseTrak(rs, trakBI, pos)
		if err != nil {
			return nil, err
		}
		if t == nil {
			continue
		}
		init.Tracks = append(init.Tracks, t)
	}
	if len(init.Tracks) == 0 {
		return nil, media.New(media.CodeNoTrack, "cmaf-init", pos, "moov has no usable tracks")
	}
	sort.Slice(init.Tracks, func(i, j int) bool { return init.Tracks[i].ID < init.Tracks[j].ID })
	return init, nil
}

func parseTrak(rs *bytes.Reader, trakBI *mp4.BoxInfo, pos media.SourcePos) (*InitTrack, error) {
	mdhdBIP, err := mp4.ExtractBoxWithPayload(rs, trakBI, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()})
	if err != nil {
		return nil, media.Wrap(media.CodeSegmentParse, "cmaf-init", pos, err, "mdhd")
	}
	if len(mdhdBIP) == 0 {
		return nil, media.New(media.CodeSegmentParse, "cmaf-init", pos, "trak without mdhd")
	}
	mdhd := mdhdBIP[0].Payload.(*mp4.Mdhd)

	tkhdBIP, err := mp4.ExtractBoxWithPayload(rs, trakBI, mp4.BoxPath{mp4.BoxTypeTkhd()})
	if err != nil || len(tkhdBIP) == 0 {
		return nil, media.New(media.CodeSegmentParse, "cmaf-init", pos, "trak without tkhd")
	}
	tkhd := tkhdBIP[0].Payload.(*mp4.Tkhd)

	hdlrBIP, err := mp4.ExtractBoxWithPayload(rs, trakBI, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()})
	if err != nil || len(hdlrBIP) == 0 {
		return nil, media.New(media.CodeSegmentParse, "cmaf-init", pos, "trak without hdlr")
	}
	hdlr := hdlrBIP[0].Payload.(*mp4.Hdlr)
	kind := media.KindOther
	switch string(hdlr.HandlerType[:]) {
	case "vide":
		kind = media.KindVideo
	case "soun":
		kind = media.KindAudio
	}

	stsdBI, err := mp4.ExtractBoxes(rs, trakBI, []mp4.BoxPath{
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd()},
	})
	if err != nil || len(stsdBI) == 0 {
		return nil, media.New(media.CodeSegmentParse, "cmaf-init", pos, "trak without stsd")
	}
	// Sample entry children start after the stsd full-box header (4) plus the
	// entry_count field (4), relative to the stsd payload.
	childrenStart := stsdBI[0].Offset + stsdBI[0].HeaderSize + 8
	childrenEnd := stsdBI[0].Offset + stsdBI[0].Size
	var samples []rawBox
	if childrenEnd > childrenStart {
		region := make([]byte, childrenEnd-childrenStart)
		if _, err := rs.ReadAt(region, int64(childrenStart)); err != nil {
			return nil, media.Wrap(media.CodeSegmentParse, "cmaf-init", pos, err, "read stsd entries")
		}
		sub, err := splitTopLevel(region, pos)
		if err != nil {
			return nil, err
		}
		samples = sub
	}
	if len(samples) == 0 {
		return nil, media.New(media.CodeSegmentParse, "cmaf-init", pos, "stsd has no sample entry")
	}
	entry := samples[0]
	entryBytes := make([]byte, entry.size)
	if _, err := rs.ReadAt(entryBytes, int64(childrenStart)+int64(entry.offset)); err != nil {
		return nil, media.Wrap(media.CodeSegmentParse, "cmaf-init", pos, err, "copy sample entry")
	}

	t := &InitTrack{
		ID:          tkhd.TrackID,
		Kind:        kind,
		EntryType:   entry.typ,
		Codec:       normalizeEntryCodec(entry.typ),
		Timescale:   int64(mdhd.Timescale),
		SampleEntry: entryBytes,
	}
	if kind == media.KindVideo {
		t.Width = int(tkhd.Width >> 16)
		t.Height = int(tkhd.Height >> 16)
	}
	return t, nil
}

func normalizeEntryCodec(entry string) string {
	switch entry {
	case "avc1", "avc3", "hev1", "hvc1", "vp09", "av01":
		return entry
	case "encv":
		return "encv"
	default:
		if len(entry) == 4 {
			return entry
		}
		return "unknown"
	}
}

// trexDefaults holds per-track defaults from the movie extends box.
type trexDefaults struct {
	duration uint32
	size     uint32
	flags    uint32
}

// ParseSegment implements Adapter.
func (*CMAFV1) ParseSegment(req SegmentRequest) ([]media.Sample, error) {
	if req.Init == nil {
		return nil, media.New(media.CodeSegmentParse, "cmaf-segment", req.Pos, "missing initialization segment")
	}
	if err := req.Limits.CheckBytes("segment", int64(len(req.Data))); err != nil {
		return nil, err
	}
	boxes, err := splitTopLevel(req.Data, req.Pos)
	if err != nil {
		return nil, err
	}
	trex, err := parseTrex(req.Init.Raw, req.Pos)
	if err != nil {
		return nil, err
	}

	var samples []media.Sample
	for i := range boxes {
		if boxes[i].typ != "moof" {
			continue
		}
		moof := boxes[i]
		next := i + 1
		if next >= len(boxes) || boxes[next].typ != "mdat" {
			return nil, media.New(media.CodeSegmentParse, "cmaf-segment", req.Pos,
				"moof at offset %d is not followed by mdat", moof.offset)
		}
		mdat := boxes[next]
		moofData := req.Data[moof.offset : moof.offset+moof.size]
		mdatData := req.Data[mdat.offset+mdat.headerSize : mdat.offset+mdat.size]
		got, err := parseMoof(req, moofData, mdatData, int64(moof.offset), int64(mdat.offset+mdat.headerSize), trex)
		if err != nil {
			return nil, err
		}
		samples = append(samples, got...)
		if err := req.Limits.CheckCount("samples", len(samples)); err != nil {
			return nil, err
		}
	}
	if len(samples) == 0 {
		return nil, media.New(media.CodeSegmentParse, "cmaf-segment", req.Pos, "segment contains no moof samples")
	}
	return samples, nil
}

func parseTrex(initData []byte, pos media.SourcePos) (map[uint32]trexDefaults, error) {
	out := make(map[uint32]trexDefaults)
	if len(initData) == 0 {
		return out, nil
	}
	boxes, err := splitTopLevel(initData, pos)
	if err != nil {
		return nil, err
	}
	for i := range boxes {
		if boxes[i].typ != "moov" {
			continue
		}
		rs := bytes.NewReader(initData[boxes[i].offset : boxes[i].offset+boxes[i].size])
		traks, err := mp4.ExtractBoxWithPayload(rs, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvex(), mp4.BoxTypeTrex()})
		if err != nil {
			return nil, media.Wrap(media.CodeSegmentParse, "cmaf-trex", pos, err, "")
		}
		for _, t := range traks {
			trex := t.Payload.(*mp4.Trex)
			out[trex.TrackID] = trexDefaults{
				duration: trex.DefaultSampleDuration,
				size:     trex.DefaultSampleSize,
				flags:    trex.DefaultSampleFlags,
			}
		}
	}
	return out, nil
}

func parseMoof(req SegmentRequest, moofData, mdatData []byte, moofAbs, mdatDataAbs int64, trex map[uint32]trexDefaults) ([]media.Sample, error) {
	rs := bytes.NewReader(moofData)
	trafs, err := mp4.ExtractBoxes(rs, nil, []mp4.BoxPath{{mp4.BoxTypeMoof(), mp4.BoxTypeTraf()}})
	if err != nil {
		return nil, media.Wrap(media.CodeTruncatedBox, "cmaf-moof", req.Pos, err, "scan trafs")
	}
	if len(trafs) == 0 {
		return nil, media.New(media.CodeSegmentParse, "cmaf-moof", req.Pos, "moof without traf")
	}
	var out []media.Sample
	for _, trafBI := range trafs {
		got, err := parseTraf(req, rs, trafBI, mdatData, moofAbs, mdatDataAbs, trex)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func parseTraf(req SegmentRequest, rs *bytes.Reader, trafBI *mp4.BoxInfo, mdatData []byte, moofAbs, mdatDataAbs int64,
	trex map[uint32]trexDefaults) ([]media.Sample, error) {
	tfhdBIP, err := mp4.ExtractBoxWithPayload(rs, trafBI, mp4.BoxPath{mp4.BoxTypeTfhd()})
	if err != nil || len(tfhdBIP) == 0 {
		return nil, media.New(media.CodeSegmentParse, "cmaf-traf", req.Pos, "traf without tfhd")
	}
	tfhd := tfhdBIP[0].Payload.(*mp4.Tfhd)
	info := req.Init.TrackByID(tfhd.TrackID)
	if info == nil {
		return nil, media.New(media.CodeSegmentParse, "cmaf-traf", req.Pos,
			"segment references track %d absent from initialization segment", tfhd.TrackID)
	}
	key, ok := req.Keys[tfhd.TrackID]
	if !ok {
		return nil, media.New(media.CodeNoTrack, "cmaf-traf", req.Pos, "no mapped key for track %d", tfhd.TrackID)
	}

	var baseDecode int64
	tfdtBIP, err := mp4.ExtractBoxWithPayload(rs, trafBI, mp4.BoxPath{mp4.BoxTypeTfdt()})
	if err != nil {
		return nil, media.Wrap(media.CodeTruncatedBox, "cmaf-traf", req.Pos, err, "tfdt")
	}
	if len(tfdtBIP) > 0 {
		tfdt := tfdtBIP[0].Payload.(*mp4.Tfdt)
		raw := tfdt.GetBaseMediaDecodeTime()
		baseDecode = media.UnwrapTimestamp(req.Refs[tfhd.TrackID], int64(raw))
	}

	trunBIP, err := mp4.ExtractBoxWithPayload(rs, trafBI, mp4.BoxPath{mp4.BoxTypeTrun()})
	if err != nil {
		return nil, media.Wrap(media.CodeTruncatedBox, "cmaf-traf", req.Pos, err, "trun")
	}
	if len(trunBIP) == 0 {
		return nil, media.New(media.CodeSegmentParse, "cmaf-traf", req.Pos, "traf without trun")
	}

	def := trex[tfhd.TrackID]
	defDur := def.duration
	if tfhd.CheckFlag(mp4.TfhdDefaultSampleDurationPresent) {
		defDur = tfhd.DefaultSampleDuration
	}
	defSize := def.size
	if tfhd.CheckFlag(mp4.TfhdDefaultSampleSizePresent) {
		defSize = tfhd.DefaultSampleSize
	}

	// trun data offsets are counted from an absolute base: explicitly from
	// BaseDataOffset when present, otherwise from the moof start under the
	// default-base-is-moof flag. Convert that base into the mdat-data
	// coordinate space used to slice sample bytes.
	baseAbs := moofAbs
	if tfhd.CheckFlag(mp4.TfhdBaseDataOffsetPresent) {
		baseAbs = int64(tfhd.BaseDataOffset)
	}
	trunOffset := baseAbs - mdatDataAbs

	var samples []media.Sample
	dts := baseDecode
	for _, trunBIPItem := range trunBIP {
		trun := trunBIPItem.Payload.(*mp4.Trun)
		if int(trun.SampleCount) > req.Limits.MaxSamplesPerSeg && req.Limits.MaxSamplesPerSeg > 0 {
			return nil, media.New(media.CodeLimit, "cmaf-trun", req.Pos,
				"trun sample count %d exceeds limit", trun.SampleCount)
		}
		entryCount := len(trun.Entries)
		cursor := trunOffset
		if trun.CheckFlag(0x000001) {
			cursor += int64(trun.DataOffset)
		}
		for ei := 0; ei < int(trun.SampleCount); ei++ {
			var dur, size uint32
			var cto int64
			var flags uint32
			hasDur, hasSize := trun.CheckFlag(0x000100), trun.CheckFlag(0x000200)
			if ei < entryCount {
				e := trun.Entries[ei]
				if hasDur {
					dur = e.SampleDuration
				}
				if hasSize {
					size = e.SampleSize
				}
				if trun.CheckFlag(0x000400) {
					flags = e.SampleFlags
				}
				if trun.CheckFlag(0x000800) {
					cto = trun.GetSampleCompositionTimeOffset(ei)
				}
			}
			if !hasDur {
				dur = defDur
			}
			if !hasSize {
				size = defSize
			}
			if int64(cursor)+int64(size) > int64(len(mdatData)) {
				return nil, media.New(media.CodeTruncatedBox, "cmaf-trun", req.Pos,
					"sample %d of track %d needs bytes [%d,%d) but mdat holds %d (truncated moof payload)",
					ei, tfhd.TrackID, cursor, cursor+int64(size), len(mdatData))
			}
			if size == 0 || dur == 0 {
				return nil, media.New(media.CodeSegmentParse, "cmaf-trun", req.Pos,
					"sample %d of track %d has zero size or duration", ei, tfhd.TrackID)
			}
			payload := mdatData[cursor : cursor+int64(size)]
			dtsUnified, err := media.TicksToUnified(dts, info.Timescale)
			if err != nil {
				return nil, media.Wrap(media.CodeInvalidDuration, "cmaf-trun", req.Pos, err, "")
			}
			durUnified, err := media.TicksToUnified(int64(dur), info.Timescale)
			if err != nil {
				return nil, media.Wrap(media.CodeInvalidDuration, "cmaf-trun", req.Pos, err, "")
			}
			ctoUnified, err := media.TicksToUnified(cto, info.Timescale)
			if err != nil {
				return nil, media.Wrap(media.CodeInvalidDuration, "cmaf-trun", req.Pos, err, "")
			}
			s := media.Sample{
				Key:      key,
				DTS:      dtsUnified,
				PTS:      dtsUnified + ctoUnified,
				Duration: durUnified,
				Sync:     isSyncFlags(flags) || ei == 0 && !trun.CheckFlag(0x000400),
				Data:     append([]byte(nil), payload...),
				Source: media.SourceRef{
					Track:   key.String(),
					Segment: req.SegmentIndex,
					Sample:  len(samples),
					URI:     req.URI,
					Pos:     req.Pos,
				},
			}
			samples = append(samples, s)
			dts += int64(dur)
			cursor += int64(size)
		}
		trunOffset = cursor
	}
	if len(samples) == 0 {
		return nil, media.New(media.CodeSegmentParse, "cmaf-traf", req.Pos, "traf decoded zero samples")
	}
	return samples, nil
}

func isSyncFlags(flags uint32) bool {
	// ISO/IEC 14496-12 sample_flags: bit 16 (0x10000) set means non-sync.
	if flags == 0 {
		return true
	}
	return flags&0x00010000 == 0
}
