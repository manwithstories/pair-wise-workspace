// Package index consumes a parsed box tree and builds per-track sample
// tables (stts/ctts/stsc/stsz/stco/co64/stss) plus track summaries.
package index

import (
	"encoding/binary"
	"fmt"
	"io"

	"mp4loc/internal/box"
)

// maxTableBytes caps any single table box we fully materialize (256 MiB).
const maxTableBytes = 1 << 28

// Movie is the indexed result for one file.
type Movie struct {
	Timescale uint32 // mvhd timescale
	Duration  int64  // mvhd duration
	Tracks    []*box.Track
}

// VideoTrack returns the first video track, or nil.
func (m *Movie) VideoTrack() *box.Track {
	for _, t := range m.Tracks {
		if t.IsVideo() {
			return t
		}
	}
	return nil
}

// TrackByID returns the track with the given id, or nil.
func (m *Movie) TrackByID(id uint32) *box.Track {
	for _, t := range m.Tracks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Build indexes every trak under moov. Structural inconsistencies
// (truncated tables, count mismatches) yield a *box.StructureError.
func Build(ra io.ReaderAt, f *box.Forest) (*Movie, error) {
	moov := f.Find("moov")
	if moov == nil {
		return nil, &box.StructureError{Offset: 0, Reason: "missing moov box"}
	}
	mv := &Movie{}
	if mvhd := moov.Find("mvhd"); mvhd != nil {
		b, err := readBox(ra, mvhd)
		if err != nil {
			return nil, err
		}
		mv.Timescale, mv.Duration, err = parseMvhd(b)
		if err != nil {
			return nil, err
		}
	}
	for _, trak := range moov.FindAll("trak") {
		tr, err := buildTrack(ra, trak)
		if err != nil {
			return nil, err
		}
		mv.Tracks = append(mv.Tracks, tr)
	}
	if len(mv.Tracks) == 0 {
		return nil, &box.StructureError{Offset: moov.Offset, Reason: "moov contains no trak"}
	}
	return mv, nil
}

func readBox(ra io.ReaderAt, n *box.Node) ([]byte, error) {
	if n.DataSize() < 0 || n.DataSize() > maxTableBytes {
		return nil, &box.StructureError{Offset: n.Offset, Reason: fmt.Sprintf("%s payload %d bytes out of bounds", n.Type, n.DataSize())}
	}
	b := make([]byte, n.DataSize())
	if _, err := ra.ReadAt(b, n.DataOffset()); err != nil {
		return nil, &box.StructureError{Offset: n.DataOffset(), Reason: "truncated " + n.Type + ": " + err.Error()}
	}
	return b, nil
}

func parseMvhd(b []byte) (timescale uint32, duration int64, err error) {
	if len(b) < 4 {
		return 0, 0, &box.StructureError{Offset: 0, Reason: "truncated mvhd"}
	}
	switch b[0] {
	case 0:
		if len(b) < 20 {
			return 0, 0, &box.StructureError{Offset: 0, Reason: "truncated mvhd v0"}
		}
		return binary.BigEndian.Uint32(b[12:]), int64(binary.BigEndian.Uint32(b[16:])), nil
	case 1:
		if len(b) < 32 {
			return 0, 0, &box.StructureError{Offset: 0, Reason: "truncated mvhd v1"}
		}
		return binary.BigEndian.Uint32(b[20:]), int64(binary.BigEndian.Uint64(b[24:])), nil
	}
	return 0, 0, &box.StructureError{Offset: 0, Reason: fmt.Sprintf("unsupported mvhd version %d", b[0])}
}

func buildTrack(ra io.ReaderAt, trak *box.Node) (*box.Track, error) {
	tr := &box.Track{}
	if tkhd := trak.Find("tkhd"); tkhd != nil {
		b, err := readBox(ra, tkhd)
		if err != nil {
			return nil, err
		}
		id, err := parseTkhd(b)
		if err != nil {
			return nil, err
		}
		tr.ID = id
	}
	mdia := trak.Find("mdia")
	if mdia == nil {
		return nil, &box.StructureError{Offset: trak.Offset, Reason: "trak missing mdia"}
	}
	if mdhd := mdia.Find("mdhd"); mdhd != nil {
		b, err := readBox(ra, mdhd)
		if err != nil {
			return nil, err
		}
		ts, dur, err := parseMdhd(b)
		if err != nil {
			return nil, err
		}
		tr.Timescale, tr.Duration = ts, dur
		if len(b) > 0 {
			tr.MdhdVersion = b[0]
		}
	}
	if tr.Timescale == 0 {
		return nil, &box.StructureError{Offset: mdia.Offset, Reason: "zero mdhd timescale"}
	}
	if hdlr := mdia.Find("hdlr"); hdlr != nil {
		b, err := readBox(ra, hdlr)
		if err != nil {
			return nil, err
		}
		if len(b) >= 12 {
			tr.Handler = string(b[8:12])
		}
	}
	if elst := trak.FindPath("edts", "elst"); elst != nil {
		b, err := readBox(ra, elst)
		if err != nil {
			return nil, err
		}
		edits, err := parseElst(b)
		if err != nil {
			return nil, err
		}
		tr.Edits = edits
	}
	stbl := mdia.FindPath("minf", "stbl")
	if stbl == nil {
		return nil, &box.StructureError{Offset: mdia.Offset, Reason: "missing stbl"}
	}
	tbl, err := buildSampleTable(ra, stbl)
	if err != nil {
		return nil, err
	}
	tr.Table = tbl
	return tr, nil
}

func parseTkhd(b []byte) (uint32, error) {
	if len(b) < 4 {
		return 0, &box.StructureError{Offset: 0, Reason: "truncated tkhd"}
	}
	switch b[0] {
	case 0:
		if len(b) < 24 {
			return 0, &box.StructureError{Offset: 0, Reason: "truncated tkhd v0"}
		}
		return binary.BigEndian.Uint32(b[12:]), nil
	case 1:
		if len(b) < 32 {
			return 0, &box.StructureError{Offset: 0, Reason: "truncated tkhd v1"}
		}
		return binary.BigEndian.Uint32(b[20:]), nil
	}
	return 0, &box.StructureError{Offset: 0, Reason: fmt.Sprintf("unsupported tkhd version %d", b[0])}
}

func parseMdhd(b []byte) (uint32, int64, error) {
	if len(b) < 4 {
		return 0, 0, &box.StructureError{Offset: 0, Reason: "truncated mdhd"}
	}
	switch b[0] {
	case 0:
		if len(b) < 24 {
			return 0, 0, &box.StructureError{Offset: 0, Reason: "truncated mdhd v0"}
		}
		return binary.BigEndian.Uint32(b[12:]), int64(binary.BigEndian.Uint32(b[16:])), nil
	case 1:
		if len(b) < 36 {
			return 0, 0, &box.StructureError{Offset: 0, Reason: "truncated mdhd v1"}
		}
		return binary.BigEndian.Uint32(b[20:]), int64(binary.BigEndian.Uint64(b[24:])), nil
	}
	return 0, 0, &box.StructureError{Offset: 0, Reason: fmt.Sprintf("unsupported mdhd version %d", b[0])}
}

func parseElst(b []byte) ([]box.EditSegment, error) {
	if len(b) < 8 {
		return nil, &box.StructureError{Offset: 0, Reason: "truncated elst"}
	}
	version := b[0]
	n := int(binary.BigEndian.Uint32(b[4:]))
	var out []box.EditSegment
	p := 8
	for i := 0; i < n; i++ {
		var seg box.EditSegment
		switch version {
		case 0:
			if p+12 > len(b) {
				return nil, &box.StructureError{Offset: 0, Reason: "truncated elst v0 entries"}
			}
			seg.SegmentDuration = int64(binary.BigEndian.Uint32(b[p:]))
			seg.MediaTime = int64(int32(binary.BigEndian.Uint32(b[p+4:])))
			seg.MediaRate = rate(int16(binary.BigEndian.Uint16(b[p+8:])), binary.BigEndian.Uint16(b[p+10:]))
			p += 12
		case 1:
			if p+20 > len(b) {
				return nil, &box.StructureError{Offset: 0, Reason: "truncated elst v1 entries"}
			}
			seg.SegmentDuration = int64(binary.BigEndian.Uint64(b[p:]))
			seg.MediaTime = int64(binary.BigEndian.Uint64(b[p+8:]))
			seg.MediaRate = rate(int16(binary.BigEndian.Uint16(b[p+16:])), binary.BigEndian.Uint16(b[p+18:]))
			p += 20
		default:
			return nil, &box.StructureError{Offset: 0, Reason: fmt.Sprintf("unsupported elst version %d", version)}
		}
		out = append(out, seg)
	}
	return out, nil
}

func rate(whole int16, frac uint16) float64 {
	return float64(whole) + float64(frac)/65536.0
}
