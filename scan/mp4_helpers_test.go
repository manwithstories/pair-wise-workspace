package scan

import (
	"encoding/binary"
	"os"
	"testing"

	"mediaslice/internal/fixture"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustStartEnd(bw *fixture.BoxWriter, typ string, payload []byte) {
	must(bw.Start(typ))
	_, _ = bw.Write(payload)
	must(bw.End())
}

// locateBox returns the absolute offset of the first box header of typ by
// scanning the 4cc tags (test helper, fixture layout is known).
func locateBox(t *testing.T, raw []byte, typ string) int {
	t.Helper()
	tag := []byte(typ)
	for i := 8; i+4 <= len(raw); i++ {
		if string(raw[i:i+4]) == string(tag) {
			return i - 4
		}
	}
	t.Fatalf("box %s not found", typ)
	return -1
}

// writeNegativeEditMP4 builds a minimal ftyp/moov/trak with an elst whose
// single entry carries a negative media_time (edit-list delay).
func writeNegativeEditMP4(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bw := fixture.NewBoxWriter(f)
	mustStartEnd(bw, "ftyp", []byte("isom\x00\x00\x02\x00isom"))
	must(bw.Start("moov"))
	must(bw.Start("trak"))
	must(bw.Start("tkhd"))
	must(bw.FullBox(0, 3))
	_, _ = bw.Write(make([]byte, 84))
	must(bw.End())
	must(bw.Start("edts"))
	must(bw.Start("elst"))
	must(bw.FullBox(0, 0))
	buf := fixture.U32(nil, 1)
	buf = fixture.U32(buf, 180000)                           // segment duration
	buf = fixture.U32(buf, uint32(int64(-90000)&0xffffffff)) // negative media time
	buf = append(buf, 0x00, 0x01, 0x00, 0x00)
	_, _ = bw.Write(buf)
	must(bw.End())
	must(bw.End())
	must(bw.Start("mdia"))
	must(bw.Start("mdhd"))
	must(bw.FullBox(0, 0))
	mdhdbuf := fixture.U32(nil, 0)
	mdhdbuf = fixture.U32(mdhdbuf, 0)
	mdhdbuf = fixture.U32(mdhdbuf, 90000)
	mdhdbuf = fixture.U32(mdhdbuf, 180000)
	mdhdbuf = append(mdhdbuf, 0x55, 0xc4, 0, 0)
	_, _ = bw.Write(mdhdbuf)
	must(bw.End())
	must(bw.Start("hdlr"))
	must(bw.FullBox(0, 0))
	_, _ = bw.Write(append([]byte{0, 0, 0, 0}, append([]byte("vide"), make([]byte, 13)...)...))
	must(bw.End())
	must(bw.Start("minf"))
	must(bw.Start("stbl"))
	// Empty but structurally present sample tables with one sample.
	mustStartEnd(bw, "stsd", fixture.U32(nil, 0))
	mustStartEnd(bw, "stts", append(fixture.U32(fixture.U32(nil, 1), 1),
		fixture.U32(nil, 3600)...))
	mustStartEnd(bw, "stsc", fixture.U32(fixture.U32(fixture.U32(fixture.U32(nil, 1), 1), 1), 1))
	mustStartEnd(bw, "stsz", fixture.U32(fixture.U32(nil, 16), 1))
	mustStartEnd(bw, "stco", fixture.U32(fixture.U32(nil, 1), 0))
	must(bw.End()) // stbl
	must(bw.End()) // minf
	must(bw.End()) // mdia
	must(bw.End()) // trak
	must(bw.End()) // moov
	_ = binary.BigEndian
}

// writeZeroDurationEditMP4 builds a track whose elst has a zero-duration
// (empty) edit segment, which must produce an edit-list diagnostic.
func writeZeroDurationEditMP4(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bw := fixture.NewBoxWriter(f)
	mustStartEnd(bw, "ftyp", []byte("isom\x00\x00\x02\x00isom"))
	must(bw.Start("moov"))
	must(bw.Start("trak"))
	must(bw.Start("tkhd"))
	must(bw.FullBox(0, 3))
	_, _ = bw.Write(make([]byte, 84))
	must(bw.End())
	must(bw.Start("edts"))
	must(bw.Start("elst"))
	must(bw.FullBox(0, 0))
	buf := fixture.U32(nil, 1)
	buf = fixture.U32(buf, 0) // zero segment duration => empty edit
	buf = fixture.U32(buf, 90000)
	buf = append(buf, 0x00, 0x01, 0x00, 0x00)
	_, _ = bw.Write(buf)
	must(bw.End())
	must(bw.End())
	must(bw.Start("mdia"))
	must(bw.Start("mdhd"))
	must(bw.FullBox(0, 0))
	mdhdbuf := fixture.U32(fixture.U32(fixture.U32(fixture.U32(nil, 0), 0), 90000), 180000)
	mdhdbuf = append(mdhdbuf, 0x55, 0xc4, 0, 0)
	_, _ = bw.Write(mdhdbuf)
	must(bw.End())
	must(bw.End()) // mdia
	must(bw.End()) // trak
	must(bw.End()) // moov
}
