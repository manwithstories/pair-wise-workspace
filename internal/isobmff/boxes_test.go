// Package isobmff is tested through hand-built box images: no FFmpeg and no
// network are required, so the parser contract is pinned independently of the
// fixture generator.
package isobmff_test

import (
	"encoding/binary"
	"errors"
	"testing"

	"archive/internal/isobmff"
	"archive/internal/media"
)

// u32 encodes a big-endian uint32. u64 deliberately writes the low word first
// in some call sites, so it is built from two explicit halves where needed.
func u32(v uint32) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

func u64(v uint64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, v)
	return out
}

func rawBox(typ string, payload []byte) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return rawBox(typ, append(head, payload...))
}

func concat(parts ...[]byte) []byte {
	out := make([]byte, 0)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// movie builds an init segment: one video track with id 2 and media timescale
// 12800, plus a trex default for that track.
func movie() []byte {
	mvhd := fullBox("mvhd", 0, 0, concat(
		u32(0), u32(0), u32(90000), u32(0), u32(0x00010000),
		[]byte{1, 0}, make([]byte, 10),
		make([]byte, 36), // matrix
		make([]byte, 24), // pre_defined
		u32(2),           // next track id
	))

	tkhd := fullBox("tkhd", 0, 0, concat(
		u32(0), u32(0), u32(2), u32(0),
		make([]byte, 60), // reserved/layer/volume/matrix
		u32(0), u32(0),   // width, height
	))

	mdhd := fullBox("mdhd", 0, 0, concat(
		u32(0), u32(0), u32(12800), u32(0),
		u32(0), u32(0), // language + pre_defined
	))

	hdlr := fullBox("hdlr", 0, 0, concat(
		u32(0), []byte("vide"), make([]byte, 12), []byte("video"),
	))

	avcC := rawBox("avcC", concat([]byte{1, 0x42, 0x00, 0x1e, 0xff, 0xe1}, make([]byte, 20)))
	entry := rawBox("avc1", concat(make([]byte, 78), avcC))
	stsd := fullBox("stsd", 0, 0, concat(u32(1), entry))
	stbl := rawBox("stbl", stsd)
	minf := rawBox("minf", stbl)
	mdia := rawBox("mdia", concat(mdhd, hdlr, minf))
	trak := rawBox("trak", concat(tkhd, mdia))

	trex := fullBox("trex", 0, 0, concat(u32(2), u32(1), u32(0), u32(0), u32(0)))
	mvex := rawBox("mvex", trex)
	return rawBox("moov", concat(mvhd, trak, mvex))
}

// fragment builds a moof+mdat pair. dataOffset is relative to the moof start,
// exactly as tfhd default-base-is-moof specifies.
func fragment(seq uint32, baseDecode uint64, dataOffset int32, n int) []byte {
	mfhd := fullBox("mfhd", 0, 0, u32(seq))
	tfhd := fullBox("tfhd", 0, 0x020000|0x000008|0x000010|0x000020,
		concat(u32(2), u32(16), u32(0), u32(0)))
	tfdt := fullBox("tfdt", 1, 0, u64(baseDecode))

	flags := uint32(0x000001 | 0x000004 | 0x000100 | 0x000200 | 0x000400 | 0x000800)
	body := concat(u32(uint32(n)), u32(uint32(dataOffset)), u32(0))
	for i := 0; i < n; i++ {
		body = append(body, concat(u32(16), u32(4), u32(0x00010000), u32(0))...)
	}
	trun := fullBox("trun", 0, flags, body)
	traf := rawBox("traf", concat(tfhd, tfdt, trun))
	moof := rawBox("moof", concat(mfhd, traf))
	mdat := rawBox("mdat", make([]byte, 4*n))
	return concat(moof, mdat)
}

func fileWith(frags ...[]byte) []byte {
	return concat(concat(rawBox("ftyp", concat([]byte("isom"), u32(0))), movie()), concat(frags...))
}

func TestParseFileRecordsSamples(t *testing.T) {
	// Two fragments; the first's trun points at the mdat payload that follows
	// it, so its two samples validate and become visible.
	// dataOffset is relative to the moof start; mdat payload begins at
	// moofLen+8, so build the fragment twice: the first pass fixes the moof
	// length (it does not depend on the offset field's value).
	probe := fragment(1, 0, 0, 2)
	first := fragment(1, 0, int32(moofLen(probe)+8), 2)
	probe2 := fragment(2, 0, 0, 2)
	second := fragment(2, 32, int32(moofLen(probe2)+8), 2)
	data := fileWith(first, second)

	parsed, err := isobmff.ParseFile("synthetic", "synthetic", data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Fragments) != 2 {
		t.Fatalf("fragments = %d, want 2", len(parsed.Fragments))
	}
	if parsed.FirstBadByte != -1 {
		t.Fatalf("first_bad_byte = %d, want clean", parsed.FirstBadByte)
	}
	if len(parsed.Movie.Tracks) != 1 || parsed.Movie.Tracks[0].ID != 2 {
		t.Fatalf("tracks = %+v", parsed.Movie.Tracks)
	}
	if parsed.Movie.Tracks[0].Timescale != 12800 {
		t.Fatalf("timescale = %d, want 12800", parsed.Movie.Tracks[0].Timescale)
	}
	for _, f := range parsed.Fragments {
		if len(f.Tracks) != 1 {
			t.Fatalf("fragment %d trafs = %d", f.Sequence, len(f.Tracks))
		}
		if got := len(f.Tracks[0].Samples); got != 2 {
			t.Fatalf("fragment %d samples = %d, want 2", f.Sequence, got)
		}
	}
}

// moofLen returns the size of the leading moof box.
func moofLen(fragmentBytes []byte) int {
	return int(binary.BigEndian.Uint32(fragmentBytes[0:4]))
}

func TestParseRejectsTruncatedBox(t *testing.T) {
	// Append a box header whose declared size runs far past EOF; the parser
	// must reject it and report the first byte it could not deliver.
	data := fileWith(fragment(1, 0, 0, 1))
	bad := concat(u32(4096), []byte("moov"), make([]byte, 16))
	data = concat(data, bad)
	_, err := isobmff.ParseFile("trunc", "trunc", data)
	if err == nil {
		t.Fatalf("expected truncation error")
	}
	off, ok := media.FirstBadByteOffset(err)
	if !ok {
		t.Fatalf("error lacks first_bad_byte: %v", err)
	}
	if off != int64(len(data)) {
		t.Fatalf("first_bad_byte = %d, want %d", off, len(data))
	}
}

func TestParseRejectsOversizedBoxLength(t *testing.T) {
	payload := []byte{0, 0, 0, 0x80}
	data := concat(rawBox("ftyp", make([]byte, 8)), rawBox("moov", payload))
	_, err := isobmff.ParseFile("huge", "huge", data)
	if err == nil {
		t.Fatalf("expected overrun error")
	}
	if !isobmff.IsBadOffset(err) {
		t.Fatalf("error should carry first_bad_byte: %v", err)
	}
}

func TestParseRejectsIntegerOverflowOffsets(t *testing.T) {
	payload := concat(u64(0x7fffffffffffffff), make([]byte, 8))
	boxBytes := concat([]byte{0, 0, 0, 1}, []byte("free"), payload)
	data := fileWith(boxBytes)
	_, err := isobmff.ParseFile("overflow", "overflow", data)
	if err == nil {
		t.Fatalf("expected overflow rejection")
	}
	if !errors.Is(err, media.ErrUnrecoverable) && !isobmff.IsBadOffset(err) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseMissingMoov(t *testing.T) {
	data := rawBox("ftyp", make([]byte, 8))
	_, err := isobmff.ParseFile("no-moov", "no-moov", data)
	if err == nil {
		t.Fatalf("expected missing moov error")
	}
	if _, ok := media.FirstBadByteOffset(err); !ok {
		t.Fatalf("missing moov error lacks first_bad_byte")
	}
}

func TestClippedSampleIsolated(t *testing.T) {
	// Point the trun past the mdat: the fragment must be flagged with a
	// first_bad_byte at the mdat end and its samples withheld.
	frag := fragment(1, 0, 1000, 2)
	data := fileWith(frag)
	parsed, err := isobmff.ParseFile("clipped", "clipped", data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Fragments) != 1 {
		t.Fatalf("fragments = %d", len(parsed.Fragments))
	}
	f := parsed.Fragments[0]
	if f.FirstBadByte < 0 {
		t.Fatalf("fragment should be flagged")
	}
	if f.DroppedSamples != 2 {
		t.Fatalf("dropped = %d, want 2", f.DroppedSamples)
	}
	if parsed.FirstBadByte != f.FirstBadByte {
		t.Fatalf("result first_bad_byte = %d, fragment = %d", parsed.FirstBadByte, f.FirstBadByte)
	}
}
