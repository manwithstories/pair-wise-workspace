package parser

import (
	"mediascan/internal/media"
)

// ParseFile parses boxes and all sample/fragment tables, then validates byte
// ranges. It never returns a nil File; structural faults are in File.Faults.
func ParseFile(src *Source) *File {
	idx, boxFaults := ParseBoxes(src)
	f := &File{Source: src, Index: idx, Faults: boxFaults}
	for _, b := range idx.FindAll("mdat") {
		f.MDAT = append(f.MDAT, ByteRange{Offset: b.PayloadOffset, End: b.End})
	}
	f.Tracks = buildTracks(src, idx, &f.Faults)
	validateSampleRanges(f)
	validateFragmentRanges(f)
	return f
}

func buildTracks(src *Source, idx *BoxIndex, faults *[]media.Fault) []*Track {
	var tracks []*Track
	byID := map[uint32]*Track{}
	for _, b := range idx.FindAll("tkhd") {
		t := &Track{TrackID: b.TrackID}
		tracks = append(tracks, t)
		byID[b.TrackID] = t
	}

	for _, b := range idx.All {
		t := byID[b.TrackID]
		if t == nil && (b.Type == "mdhd" || b.Type == "stsd") {
			// Defensive path for a file lacking a parseable tkhd.
			t = &Track{TrackID: b.TrackID}
			tracks = append(tracks, t)
			byID[b.TrackID] = t
		}
		if t == nil {
			continue
		}
		switch b.Type {
		case "mdhd":
			parseMDHD(src, b, t, faults)
		case "hdlr":
			parseHDLR(src, b, t)
		case "elst":
			t.Edits = parseELST(src, b)
		case "stsz":
			parseSTSZ(src, b, t)
		case "stts":
			parseSTTS(src, b, t)
		case "stss":
			t.KeyframeNumbers = parseSTSS(src, b)
		case "stco":
			t.ChunkOffsets = append(t.ChunkOffsets, parseSTCO(src, b, false)...)
		case "co64":
			t.ChunkOffsets = append(t.ChunkOffsets, parseSTCO(src, b, true)...)
		case "stsc":
			parseSTSC(src, b, t)
		case "tfdt":
			parseTFDT(src, b, t)
		case "trun":
			parseTRUN(src, idx, b, t)
		}
	}
	for _, t := range tracks {
		finalizeSamples(t)
		markKeyframes(t)
	}
	return tracks
}

func parseMDHD(src *Source, b *Box, t *Track, faults *[]media.Fault) {
	t.MDHDOffset = b.Offset
	r := newTableReader(src, b)
	version, ok := r.u8()
	if !ok {
		return
	}
	_, _ = r.take(3)
	if version == 1 {
		_, _ = r.u64()
		_, _ = r.u64()
		ts, ok1 := r.u32()
		dur, ok2 := r.u64()
		if ok1 && ok2 {
			t.Timescale, t.Duration = ts, dur
		}
	} else {
		_, _ = r.u32()
		_, _ = r.u32()
		ts, ok1 := r.u32()
		dur, ok2 := r.u32()
		if ok1 && ok2 {
			t.Timescale, t.Duration = ts, uint64(dur)
		}
	}
	if t.Timescale == 0 {
		*faults = append(*faults, media.NewFault(
			media.CodeZeroTimescale, media.SeverityError, src.SHA256, src.Name,
			t.TrackID, b.Offset, media.NewTime(0, 0),
			"mdhd has timescale=0"))
	}
}

func parseHDLR(src *Source, b *Box, t *Track) {
	buf := make([]byte, 12)
	if err := readExactAt(src.ReaderAt, buf, b.PayloadOffset); err == nil {
		t.Handler = string(buf[8:12])
	}
}
