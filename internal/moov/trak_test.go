package moov

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"mp4probe/internal/box"
)

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func be64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// buildFile assembles a minimal but complete MP4 with one video track.
func buildFile(t *testing.T, elstPayload []byte, moovAtTail bool) []byte {
	t.Helper()
	mvhd := box.PackFull("mvhd", 0, 0, cat(be32(0), be32(0), be32(90000), be32(900000)))
	tkhd := box.PackFull("tkhd", 0, 7, cat(be32(0), be32(0), be32(1), be32(0), be32(900000)))
	mdhd := box.PackFull("mdhd", 0, 0, cat(be32(0), be32(0), be32(90000), be32(360000), []byte{0x55, 0xc4, 0, 0}))
	hdlr := box.PackFull("hdlr", 0, 0, cat(be32(0), []byte("vide"), make([]byte, 12)))
	stsd := box.PackFull("stsd", 0, 0, cat(be32(1), be32(16), []byte("avc1"), make([]byte, 8)))
	stts := box.PackFull("stts", 0, 0, cat(be32(1), be32(10), be32(3600)))
	stss := box.PackFull("stss", 0, 0, cat(be32(2), be32(1), be32(6)))
	stsc := box.PackFull("stsc", 0, 0, cat(be32(1), be32(1), be32(5), be32(1)))
	stsz := box.PackFull("stsz", 0, 0, cat(be32(0), be32(10),
		be32(100), be32(100), be32(100), be32(100), be32(100),
		be32(100), be32(100), be32(100), be32(100), be32(100)))
	stco := box.PackFull("stco", 0, 0, cat(be32(2), be32(1000), be32(1500)))
	stbl := box.Pack("stbl", cat(stsd, stts, stss, stsc, stsz, stco))
	minf := box.Pack("minf", stbl)
	mdia := box.Pack("mdia", cat(mdhd, hdlr, minf))
	var edts []byte
	if elstPayload != nil {
		edts = box.Pack("edts", box.Pack("elst", elstPayload))
	}
	trak := box.Pack("trak", cat(tkhd, edts, mdia))
	moov := box.Pack("moov", cat(mvhd, trak))
	ftyp := box.Pack("ftyp", cat([]byte("isom"), be32(0), []byte("isom")))
	mdat := box.Pack("mdat", make([]byte, 2000))
	if moovAtTail {
		return cat(ftyp, mdat, moov)
	}
	return cat(ftyp, moov, mdat)
}

func TestParseBasic(t *testing.T) {
	for _, tail := range []bool{false, true} {
		data := buildFile(t, nil, tail)
		m, err := Parse(bytes.NewReader(data), uint64(len(data)))
		if err != nil {
			t.Fatalf("tail=%v: %v", tail, err)
		}
		if m.MovieTimescale != 90000 || len(m.Traks) != 1 {
			t.Fatalf("tail=%v: bad moov %+v", tail, m)
		}
		tr := m.Traks[0]
		if tr.TrackID != 1 || tr.Timescale != 90000 || tr.Codec != "avc1" {
			t.Fatalf("tail=%v: bad trak %+v", tail, tr)
		}
		if tr.SampleCount() != 10 || len(tr.Stss) != 2 || len(tr.ChunkOffset) != 2 {
			t.Fatalf("tail=%v: bad tables %+v", tail, tr)
		}
		if !m.HasMdat || m.MdatEnd <= m.MdatOffset {
			t.Fatalf("tail=%v: mdat not located", tail)
		}
	}
}

func TestParseElstConflict(t *testing.T) {
	// elst total 800000 vs tkhd 900000 -> conflict, elst wins.
	body := cat(be32(2),
		be32(100000), be32(0xFFFFFFFF), []byte{0, 1, 0, 0}, // empty edit
		be32(700000), be32(3600), []byte{0, 1, 0, 0})
	elst := box.FullPayload(0, 0, body)
	data := buildFile(t, elst, false)
	m, err := Parse(bytes.NewReader(data), uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	tr := m.Traks[0]
	if len(tr.Elst) != 2 || tr.Elst[0].MediaTime != -1 {
		t.Fatalf("bad elst: %+v", tr.Elst)
	}
	if !tr.ElstConflict {
		t.Fatal("expected elst/tkhd conflict to be recorded")
	}
}

func TestParseElstV1NegativeMediaTime(t *testing.T) {
	body := cat(be32(1),
		be64(50000), be64(0xFFFFFFFFFFFFFFFF), []byte{0, 1, 0, 0})
	elst := box.FullPayload(1, 0, body)
	data := buildFile(t, elst, false)
	m, err := Parse(bytes.NewReader(data), uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Traks[0].Elst[0].MediaTime; got != -1 {
		t.Fatalf("media_time = %d, want -1", got)
	}
}

func TestMissingMoov(t *testing.T) {
	data := box.Pack("ftyp", make([]byte, 8))
	if _, err := Parse(bytes.NewReader(data), uint64(len(data))); !errors.Is(err, box.ErrStructure) {
		t.Fatalf("want structural error, got %v", err)
	}
}

func TestMissingStbl(t *testing.T) {
	mvhd := box.PackFull("mvhd", 0, 0, cat(be32(0), be32(0), be32(90000), be32(900000)))
	tkhd := box.PackFull("tkhd", 0, 7, cat(be32(0), be32(0), be32(1), be32(0), be32(900000)))
	mdhd := box.PackFull("mdhd", 0, 0, cat(be32(0), be32(0), be32(90000), be32(360000)))
	mdia := box.Pack("mdia", cat(mdhd, box.Pack("minf", nil)))
	moov := box.Pack("moov", cat(mvhd, box.Pack("trak", cat(tkhd, mdia))))
	data := cat(box.Pack("ftyp", make([]byte, 8)), moov)
	if _, err := Parse(bytes.NewReader(data), uint64(len(data))); !errors.Is(err, box.ErrStructure) {
		t.Fatalf("want structural error, got %v", err)
	}
}

func TestTruncatedTable(t *testing.T) {
	// stsz declares 10 entries but carries 2.
	stsz := box.PackFull("stsz", 0, 0, cat(be32(0), be32(10), be32(1), be32(2)))
	stbl := box.Pack("stbl", cat(
		box.PackFull("stsd", 0, 0, cat(be32(1), be32(16), []byte("avc1"), make([]byte, 8))),
		box.PackFull("stts", 0, 0, cat(be32(1), be32(1), be32(1))),
		box.PackFull("stsc", 0, 0, cat(be32(1), be32(1), be32(1), be32(1))),
		stsz,
		box.PackFull("stco", 0, 0, cat(be32(1), be32(0))),
	))
	mvhd := box.PackFull("mvhd", 0, 0, cat(be32(0), be32(0), be32(90000), be32(900000)))
	tkhd := box.PackFull("tkhd", 0, 7, cat(be32(0), be32(0), be32(1), be32(0), be32(900000)))
	mdhd := box.PackFull("mdhd", 0, 0, cat(be32(0), be32(0), be32(90000), be32(360000)))
	mdia := box.Pack("mdia", cat(mdhd, box.Pack("minf", stbl)))
	moov := box.Pack("moov", cat(mvhd, box.Pack("trak", cat(tkhd, mdia))))
	data := cat(box.Pack("ftyp", make([]byte, 8)), moov)
	if _, err := Parse(bytes.NewReader(data), uint64(len(data))); !errors.Is(err, box.ErrStructure) {
		t.Fatalf("want structural error, got %v", err)
	}
}

func TestCorruptElst(t *testing.T) {
	// media_time = -5 is invalid.
	body := cat(be32(1), be64(100), be64(0xFFFFFFFFFFFFFFFB), []byte{0, 1, 0, 0})
	elst := box.FullPayload(1, 0, body)
	data := buildFile(t, elst, false)
	if _, err := Parse(bytes.NewReader(data), uint64(len(data))); !errors.Is(err, box.ErrStructure) {
		t.Fatalf("want structural error, got %v", err)
	}
}

func TestStz2Expansion(t *testing.T) {
	// field_size=8, three samples sized 10/20/30.
	stz2 := box.PackFull("stz2", 0, 0, cat([]byte{0, 0, 0, 8}, be32(3), []byte{10, 20, 30}))
	stbl := box.Pack("stbl", cat(
		box.PackFull("stsd", 0, 0, cat(be32(1), be32(16), []byte("mp4a"), make([]byte, 8))),
		box.PackFull("stts", 0, 0, cat(be32(1), be32(3), be32(1024))),
		box.PackFull("stsc", 0, 0, cat(be32(1), be32(1), be32(3), be32(1))),
		stz2,
		box.PackFull("stco", 0, 0, cat(be32(1), be32(0))),
	))
	mvhd := box.PackFull("mvhd", 0, 0, cat(be32(0), be32(0), be32(48000), be32(3072)))
	tkhd := box.PackFull("tkhd", 0, 7, cat(be32(0), be32(0), be32(2), be32(0), be32(3072)))
	mdhd := box.PackFull("mdhd", 0, 0, cat(be32(0), be32(0), be32(48000), be32(3072)))
	mdia := box.Pack("mdia", cat(mdhd, box.Pack("minf", stbl)))
	moov := box.Pack("moov", cat(mvhd, box.Pack("trak", cat(tkhd, mdia))))
	data := cat(box.Pack("ftyp", make([]byte, 8)), moov)
	m, err := Parse(bytes.NewReader(data), uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	sizes := m.Traks[0].Sizes
	if len(sizes) != 3 || sizes[0] != 10 || sizes[1] != 20 || sizes[2] != 30 {
		t.Fatalf("stz2 expansion wrong: %v", sizes)
	}
}
