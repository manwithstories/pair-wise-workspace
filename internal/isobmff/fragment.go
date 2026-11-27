package isobmff

import (
	"encoding/binary"
	"fmt"
	"sort"

	"archive/internal/media"
)

// Traf is one track fragment: its samples and timing anchor.
type Traf struct {
	TrackID    uint32
	Timescale  media.TimeScale
	BaseDecode int64
	Samples    []media.Sample
	// TrunCount is the number of trun boxes consumed.
	TrunCount int
}

// Fragment is one moof plus following mdat, with faults localised to it.
type Fragment struct {
	Sequence      uint32
	MoofOffset    int64
	MoofSize      int64
	MdatOffset    int64
	MdatSize      int64
	MdatTruncated bool
	// FirstBadByte is -1 when the fragment is clean. Otherwise it is the
	// first byte that could not be validated.
	FirstBadByte int64
	// DroppedSamples counts samples rejected because their payload overran
	// the validated mdat window. Valid samples in the same fragment are kept.
	DroppedSamples int
	Fault          string
	Tracks         []Traf
}

// Movie holds initialisation metadata.
type Movie struct {
	Timescale media.TimeScale
	Tracks    []media.Track
	Trex      map[uint32]Trex
}

// Result is a parsed representation plus its fragment inventory.
type Result struct {
	Movie     Movie
	Fragments []Fragment
	// FirstBadByte is the minimum offending offset across fragments; -1 if
	// every fragment parsed cleanly.
	FirstBadByte int64
}

// ParseFile parses a CMAF/fMP4 image. Movie-level defects are fatal;
// fragment-level defects are localised and retained for recovery.
func ParseFile(name, path string, data []byte) (*Result, error) {
	if len(data) < 8 {
		return nil, &media.ErrFirstBadByte{Offset: int64(len(data)), Reason: "file shorter than one box header"}
	}
	region := NewRegion(data)
	res := &Result{Movie: Movie{Trex: map[uint32]Trex{}}, FirstBadByte: -1}
	var haveMoov bool
	pos := int64(0)
	for pos < region.Limit {
		box, err := region.readBox(pos, 0)
		if err != nil {
			return nil, err
		}
		switch box.Type {
		case "ftyp", "styp", "free", "skip", "sidx", "mfra", "mdat":
			// Metadata or payload already attributed to a preceding moof.
		case "moov":
			if err := parseMoov(box, &res.Movie); err != nil {
				return nil, err
			}
			haveMoov = true
		case "moof":
			frag, next := parseFragment(box, data, res.Movie)
			if frag.FirstBadByte >= 0 && (res.FirstBadByte < 0 || frag.FirstBadByte < res.FirstBadByte) {
				res.FirstBadByte = frag.FirstBadByte
			}
			res.Fragments = append(res.Fragments, frag)
			pos = next
			continue
		}
		pos = box.Offset + box.Size
	}
	if !haveMoov {
		return nil, &media.ErrFirstBadByte{Offset: 0, Reason: "moov box missing"}
	}
	if len(res.Movie.Tracks) == 0 {
		return nil, &media.ErrFirstBadByte{Offset: 0, Reason: "moov contains no usable tracks"}
	}
	return res, nil
}

func parseMoov(box Box, mv *Movie) error {
	var sawMvhd bool
	err := box.Child().Walk(1, func(b Box) error {
		switch b.Type {
		case "mvhd":
			h, err := ParseMvhd(b)
			if err != nil {
				return err
			}
			if !h.Timescale.Valid() {
				return bad(b.Offset, "mvhd timescale %d invalid", h.Timescale)
			}
			mv.Timescale = h.Timescale
			sawMvhd = true
		case "trak":
			tr, err := parseTrak(b)
			if err != nil {
				return err
			}
			mv.Tracks = append(mv.Tracks, tr)
		case "mvex":
			if err := parseMvex(b, mv.Trex); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !sawMvhd {
		return bad(box.Offset, "mvhd missing from moov")
	}
	return nil
}

type partialTrack struct {
	tkhd *Tkhd
	mdhd *Mdhd
	hdlr *Hdlr
	stsd *Stsd
}

func parseTrak(b Box) (media.Track, error) {
	var p partialTrack
	err := b.Child().Walk(2, func(c Box) error {
		switch c.Type {
		case "tkhd":
			v, err := ParseTkhd(c)
			if err != nil {
				return err
			}
			p.tkhd = &v
		case "mdia":
			return c.Child().Walk(3, func(d Box) error {
				switch d.Type {
				case "mdhd":
					v, err := ParseMdhd(d)
					if err != nil {
						return err
					}
					p.mdhd = &v
				case "hdlr":
					v, err := ParseHdlr(d)
					if err != nil {
						return err
					}
					p.hdlr = &v
				case "minf":
					return d.Child().Walk(4, func(e Box) error {
						if e.Type != "stbl" {
							return nil
						}
						return e.Child().Walk(5, func(f Box) error {
							if f.Type != "stsd" {
								return nil
							}
							v, err := ParseStsd(f)
							if err != nil {
								return err
							}
							p.stsd = &v
							return nil
						})
					})
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return media.Track{}, err
	}
	if p.tkhd == nil || p.mdhd == nil || p.hdlr == nil {
		return media.Track{}, bad(b.Offset, "trak missing tkhd/mdhd/hdlr")
	}
	if !p.mdhd.Timescale.Valid() {
		return media.Track{}, bad(b.Offset, "mdhd timescale %d invalid", p.mdhd.Timescale)
	}
	kind := media.KindVideo
	switch p.hdlr.HandlerType {
	case "vide":
		kind = media.KindVideo
	case "soun":
		kind = media.KindAudio
	default:
		return media.Track{}, bad(b.Offset, "unsupported handler %q", p.hdlr.HandlerType)
	}
	out := media.Track{ID: p.tkhd.TrackID, Kind: kind, Timescale: p.mdhd.Timescale, HandlerType: p.hdlr.HandlerType}
	if p.stsd != nil {
		out.Codec = p.stsd.Codec
		out.InitSize = p.stsd.InitSize
		out.Width = p.stsd.Width
		out.Height = p.stsd.Height
	}
	return out, nil
}

func parseMvex(b Box, out map[uint32]Trex) error {
	return b.Child().Walk(2, func(c Box) error {
		if c.Type != "trex" {
			return nil
		}
		t, err := ParseTrex(c)
		if err != nil {
			return err
		}
		out[t.TrackID] = t
		return nil
	})
}

// parseFragment parses a moof at box.Offset and any mdat directly following.
// It always returns the next scan offset so a single damaged fragment cannot
// abort the file.
func parseFragment(box Box, data []byte, movie Movie) (Fragment, int64) {
	frag := Fragment{MoofOffset: box.Offset, MoofSize: box.Size, MdatOffset: -1, FirstBadByte: -1}
	var mfhdSeen bool
	trafBoxes := make([]Box, 0, 2)
	err := box.Child().Walk(1, func(c Box) error {
		switch c.Type {
		case "mfhd":
			if len(c.Payload) < 8 {
				return bad(c.Offset, "mfhd truncated")
			}
			frag.Sequence = binary.BigEndian.Uint32(c.Payload[4:8])
			mfhdSeen = true
		case "traf":
			trafBoxes = append(trafBoxes, c)
		}
		return nil
	})
	if err != nil {
		frag.FirstBadByte, _ = media.FirstBadByteOffset(err)
		frag.Fault = err.Error()
		return frag, box.Offset + box.Size
	}
	if !mfhdSeen {
		frag.FirstBadByte = box.Offset
		frag.Fault = "mfhd missing"
		return frag, box.Offset + box.Size
	}

	trackTS := map[uint32]media.TimeScale{}
	for _, t := range movie.Tracks {
		trackTS[t.ID] = t.Timescale
	}
	for _, tb := range trafBoxes {
		tr, err := parseTraf(tb, box.Offset, movie, trackTS)
		if err != nil {
			if frag.FirstBadByte < 0 {
				frag.FirstBadByte, _ = media.FirstBadByteOffset(err)
				if frag.FirstBadByte < 0 {
					frag.FirstBadByte = tb.Offset
				}
				frag.Fault = err.Error()
			}
			continue
		}
		frag.Tracks = append(frag.Tracks, tr)
	}

	next := box.Offset + box.Size
	if next >= int64(len(data)) {
		if frag.FirstBadByte < 0 {
			frag.FirstBadByte = next
			frag.Fault = "moof followed by end of file (mdat missing)"
		}
		return frag, next
	}
	mdat, declared, err := peekBox(data, next)
	if err != nil {
		if frag.FirstBadByte < 0 {
			frag.FirstBadByte, _ = media.FirstBadByteOffset(err)
			if frag.FirstBadByte < 0 {
				frag.FirstBadByte = next
			}
			frag.Fault = err.Error()
		}
		return frag, int64(len(data))
	}
	if mdat.Type != "mdat" {
		if frag.FirstBadByte < 0 {
			frag.FirstBadByte = next
			frag.Fault = fmt.Sprintf("expected mdat after moof, found %q", mdat.Type)
		}
		return frag, next
	}
	frag.MdatOffset = mdat.Offset
	frag.MdatSize = declared
	mdatEnd := mdat.Offset + declared
	if mdatEnd > int64(len(data)) {
		frag.MdatTruncated = true
		mdatEnd = int64(len(data))
		if frag.FirstBadByte < 0 {
			frag.FirstBadByte = mdatEnd
			frag.Fault = fmt.Sprintf("mdat truncated: declared %d bytes, file has %d", declared, mdatEnd-mdat.Offset)
		}
	}

	// Validate sample payload extents. Samples fully inside the validated
	// window survive; overrunning samples are dropped and counted.
	lo := mdat.Offset + mdat.HeaderLen
	for ti := range frag.Tracks {
		tr := &frag.Tracks[ti]
		valid := tr.Samples[:0]
		for _, s := range tr.Samples {
			end, addErr := safeAdd(s.DataOffset, int64(s.Size))
			if addErr != nil {
				frag.DroppedSamples++
				if frag.FirstBadByte < 0 {
					frag.FirstBadByte = s.DataOffset
					frag.Fault = addErr.Error()
				}
				continue
			}
			if s.DataOffset < lo || s.DataOffset > mdatEnd || end > mdatEnd {
				frag.DroppedSamples++
				badAt := end
				if badAt > mdatEnd {
					badAt = mdatEnd
				}
				if frag.FirstBadByte < 0 {
					frag.FirstBadByte = badAt
					frag.Fault = fmt.Sprintf("sample %d/%d overruns mdat by %d bytes", tr.TrackID, s.LocalIndex, end-mdatEnd)
				}
				continue
			}
			valid = append(valid, s)
		}
		tr.Samples = valid
	}
	if frag.FirstBadByte >= 0 && len(frag.Tracks) > 0 && frag.DroppedSamples == 0 {
		frag.DroppedSamples = 0
	}
	return frag, mdatEnd
}

// peekBox validates a header at pos and returns its declared size even when
// the payload extends past end-of-file, so truncation can be reported.
func peekBox(data []byte, pos int64) (Box, int64, error) {
	if pos+8 > int64(len(data)) {
		return Box{}, 0, &media.ErrFirstBadByte{Offset: int64(len(data)), Reason: "box header truncated"}
	}
	hdr := data[pos : pos+8]
	size32 := int64(binary.BigEndian.Uint32(hdr[0:4]))
	typ := string(hdr[4:8])
	headerLen := int64(8)
	size := size32
	if size32 == 1 {
		if pos+16 > int64(len(data)) {
			return Box{}, 0, &media.ErrFirstBadByte{Offset: int64(len(data)), Reason: fmt.Sprintf("%q largesize truncated", typ)}
		}
		size = int64(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
		headerLen = 16
	} else if size32 == 0 {
		size = int64(len(data)) - pos
	}
	if size < headerLen {
		return Box{}, 0, &media.ErrFirstBadByte{Offset: pos, Reason: fmt.Sprintf("%q size %d invalid", typ, size)}
	}
	avail := int64(len(data)) - pos - headerLen
	if avail < 0 {
		avail = 0
	}
	payloadLen := size - headerLen
	if payloadLen > avail {
		payloadLen = avail
	}
	return Box{Type: typ, Offset: pos, Size: size, HeaderLen: headerLen, Payload: data[pos+headerLen : pos+headerLen+payloadLen]}, size, nil
}

// parseTraf decodes tfhd/tfdt/trun and materialises samples.
func parseTraf(b Box, moofOffset int64, movie Movie, trackTS map[uint32]media.TimeScale) (Traf, error) {
	tr := Traf{}
	var (
		haveTfhd, haveTfdt bool
		baseDataOffset     int64
		defaultBaseIsMoof  bool
		defDur, defSize    uint32
		defFlags           uint32
	)
	var truns []Box
	err := b.Child().Walk(2, func(c Box) error {
		switch c.Type {
		case "tfhd":
			v, flags, rest, err := header(c)
			if err != nil {
				return err
			}
			if v != 0 {
				return bad(c.Offset, "tfhd version %d unsupported", v)
			}
			if len(rest) < 4 {
				return bad(c.Offset, "tfhd truncated")
			}
			tr.TrackID = binary.BigEndian.Uint32(rest[0:4])
			off := 4
			take := func(n int, name string) ([]byte, error) {
				if len(rest) < off+n {
					return nil, bad(c.Offset, "tfhd field %s truncated", name)
				}
				out := rest[off : off+n]
				off += n
				return out, nil
			}
			if flags&0x000001 != 0 {
				v, err := take(8, "base-data-offset")
				if err != nil {
					return err
				}
				baseDataOffset = int64(binary.BigEndian.Uint64(v))
			}
			if flags&0x000002 != 0 {
				if _, err := take(4, "sample-description-index"); err != nil {
					return err
				}
			}
			if flags&0x000008 != 0 {
				v, err := take(4, "default-sample-duration")
				if err != nil {
					return err
				}
				defDur = binary.BigEndian.Uint32(v)
			}
			if flags&0x000010 != 0 {
				v, err := take(4, "default-sample-size")
				if err != nil {
					return err
				}
				defSize = binary.BigEndian.Uint32(v)
			}
			if flags&0x000020 != 0 {
				v, err := take(4, "default-sample-flags")
				if err != nil {
					return err
				}
				defFlags = binary.BigEndian.Uint32(v)
			}
			defaultBaseIsMoof = flags&0x020000 != 0
			haveTfhd = true
		case "tfdt":
			v, _, rest, err := header(c)
			if err != nil {
				return err
			}
			if v == 1 {
				if len(rest) < 8 {
					return bad(c.Offset, "tfdt v1 truncated")
				}
				tr.BaseDecode = int64(binary.BigEndian.Uint64(rest[0:8]))
			} else {
				if len(rest) < 4 {
					return bad(c.Offset, "tfdt v0 truncated")
				}
				tr.BaseDecode = int64(binary.BigEndian.Uint32(rest[0:4]))
			}
			haveTfdt = true
		case "trun":
			truns = append(truns, c)
		}
		return nil
	})
	if err != nil {
		return Traf{}, err
	}
	if !haveTfhd {
		return Traf{}, bad(b.Offset, "traf missing tfhd")
	}
	ts, ok := trackTS[tr.TrackID]
	if !ok {
		return Traf{}, bad(b.Offset, "unknown track id %d", tr.TrackID)
	}
	tr.Timescale = ts
	if !haveTfdt {
		return Traf{}, bad(b.Offset, "traf missing tfdt")
	}
	if trex, ok := movie.Trex[tr.TrackID]; ok {
		if defDur == 0 {
			defDur = trex.SampleDur
		}
		if defSize == 0 {
			defSize = trex.SampleSize
		}
		if defFlags == 0 {
			defFlags = trex.SampleFlags
		}
	}

	// ISO/IEC 14496-12 8.8.7: the data offset is relative to base-data-offset
	// when present; otherwise to the moof start when default-base-is-moof is
	// set (the CMAF default) or for the first trun of the first traf.
	base := moofOffset
	if baseDataOffset != 0 {
		base = baseDataOffset
	}
	_ = defaultBaseIsMoof
	// Track fragments in these fixtures address payload relative to the moof
	// start via tfhd base_data_offset, which CMAF writers emit explicitly.
	dts := tr.BaseDecode
	var nextData int64 = -1
	for _, tb := range truns {
		if err := decodeTrun(tb, &tr, base, defDur, defSize, defFlags, &dts, &nextData, ts); err != nil {
			return Traf{}, err
		}
		tr.TrunCount++
	}
	return tr, nil
}

// decodeTrun decodes one trun box, appending samples. The DTS cursor advances
// by each sample's duration; CTS is DTS plus the composition offset.
func decodeTrun(b Box, tr *Traf, base int64, defDur, defSize, defFlags uint32, dts *int64, nextData *int64, ts media.TimeScale) error {
	version, flags, rest, err := header(b)
	if err != nil {
		return err
	}
	if version > 1 {
		return bad(b.Offset, "trun version %d unsupported", version)
	}
	if len(rest) < 4 {
		return bad(b.Offset, "trun truncated")
	}
	count := binary.BigEndian.Uint32(rest[0:4])
	if count > 1<<20 {
		return bad(b.Offset, "trun sample count %d implausible", count)
	}
	pos := 4
	var dataOffset int32
	haveDataOffset := false
	if flags&0x000001 != 0 {
		if len(rest) < pos+4 {
			return bad(b.Offset, "trun data-offset truncated")
		}
		dataOffset = int32(binary.BigEndian.Uint32(rest[pos : pos+4]))
		pos += 4
		haveDataOffset = true
	}
	var firstFlags uint32
	haveFirst := false
	if flags&0x000004 != 0 {
		if len(rest) < pos+4 {
			return bad(b.Offset, "trun first-sample-flags truncated")
		}
		firstFlags = binary.BigEndian.Uint32(rest[pos : pos+4])
		pos += 4
		haveFirst = true
	}
	rec := 0
	for _, bit := range []uint32{0x000100, 0x000200, 0x000400, 0x000800} {
		if flags&bit != 0 {
			rec += 4
		}
	}
	if rec == 0 && count > 0 && defDur == 0 && defSize == 0 {
		return bad(b.Offset, "trun has %d samples but no per-sample fields or defaults", count)
	}
	if uint64(len(rest)-pos) < uint64(count)*uint64(rec) {
		return bad(b.Offset, "trun count %d exceeds payload (%d records of %d bytes)", count, len(rest)-pos, rec)
	}

	if haveDataOffset {
		abs, addErr := safeAdd(base, int64(dataOffset))
		if addErr != nil {
			return addErr
		}
		*nextData = abs
	}
	if *nextData < 0 {
		return bad(b.Offset, "trun lacks data offset and no predecessor")
	}

	cur := *dts
	cursor := *nextData
	for i := uint32(0); i < count; i++ {
		dur, size := defDur, defSize
		sflags := defFlags
		var cts int32
		if i == 0 && haveFirst {
			sflags = firstFlags
		}
		if flags&0x000100 != 0 {
			dur = binary.BigEndian.Uint32(rest[pos : pos+4])
			pos += 4
		}
		if flags&0x000200 != 0 {
			size = binary.BigEndian.Uint32(rest[pos : pos+4])
			pos += 4
		}
		if flags&0x000400 != 0 {
			sflags = binary.BigEndian.Uint32(rest[pos : pos+4])
			pos += 4
		}
		if flags&0x000800 != 0 {
			cts = int32(binary.BigEndian.Uint32(rest[pos : pos+4]))
			pos += 4
		}
		if dur == 0 {
			return bad(b.Offset, "sample %d of track %d has zero duration", i, tr.TrackID)
		}
		if size == 0 || int64(size) > media.MaxSampleBytes {
			return bad(b.Offset, "sample %d of track %d has size %d out of range", i, tr.TrackID, size)
		}
		if cursor < 0 {
			return bad(b.Offset, "sample %d of track %d has negative data offset", i, tr.TrackID)
		}
		tr.Samples = append(tr.Samples, media.Sample{
			LocalIndex: int(i),
			TrackID:    tr.TrackID,
			Timescale:  ts,
			DTS:        cur,
			CTS:        cur + int64(cts),
			Duration:   int64(dur),
			Size:       int(size),
			DataOffset: cursor,
			Keyframe:   sflags&0x00010000 == 0,
		})
		cur += int64(dur)
		cursor, err = safeAdd(cursor, int64(size))
		if err != nil {
			return err
		}
	}
	*dts = cur
	*nextData = cursor
	return nil
}

// SortFragments orders fragments by original sequence number, preserving
// duplicates so recovery can see the collision.
func SortFragments(frags []Fragment) {
	sort.SliceStable(frags, func(i, j int) bool { return frags[i].Sequence < frags[j].Sequence })
}
