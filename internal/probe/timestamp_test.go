package probe

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/fault"
)

// TestCompositionRegressionIsStructural rewrites composition offsets so the
// presented order goes backwards, then checks the fault classification.
func TestCompositionRegressionIsStructural(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "regress.mp4")
	writeSttsFixture(t, path, []pair{{4, 512}}, []int32{2000, 0, 1000, 0})
	f, err := Analyze(path, DefaultConfig())
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(f.Tracks) == 0 {
		t.Fatal("no track parsed")
	}
	var found bool
	for _, is := range f.Tracks[0].Issues {
		if is.Code == fault.CodeStructure {
			found = true
		}
	}
	if !found {
		t.Fatalf("timestamp regression not classified as structure: %v", f.Tracks[0].Issues)
	}
}

type pair struct{ count, delta uint32 }

// writeSttsFixture builds a one-track movie whose stts is the given runs. It
// is deliberately minimal: only the boxes probe needs are present.
func writeSttsFixture(t *testing.T, path string, runs []pair, ctts []int32) {
	t.Helper()
	stts := &wbuf{}
	stts.u32(uint32(len(runs)))
	for _, r := range runs {
		stts.u32(r.count)
		stts.u32(r.delta)
	}
	total := uint32(0)
	for _, r := range runs {
		total += r.count
	}
	stsz := &wbuf{}
	stsz.u32(0)
	stsz.u32(total)
	for i := uint32(0); i < total; i++ {
		stsz.u32(100)
	}
	stsc := &wbuf{}
	stsc.u32(1)
	stsc.u32(1)
	stsc.u32(total)
	stsc.u32(1)
	stco := &wbuf{}
	stco.u32(1)
	stco.u32(0)
	tkhd := &wbuf{}
	tkhd.u32(0)
	tkhd.u32(0)
	tkhd.u32(1)
	tkhd.raw(make([]byte, 68))
	mdhd := &wbuf{}
	mdhd.u32(0)
	mdhd.u32(0)
	mdhd.u32(15360)
	mdhd.u32(0)
	mdhd.u16(0)
	mdhd.u16(0)
	hdlr := &wbuf{}
	hdlr.u32(0)
	hdlr.raw([]byte("vide"))
	hdlr.raw(make([]byte, 13))

	stblParts := [][]byte{
		full("stts", 0, 0, stts.b),
		full("stsz", 0, 0, stsz.b),
		full("stsc", 0, 0, stsc.b),
		full("stco", 0, 0, stco.b),
	}
	if len(ctts) > 0 {
		cx := &wbuf{}
		cx.u32(uint32(len(ctts)))
		for _, off := range ctts {
			cx.u32(1)
			cx.u32(uint32(off))
		}
		stblParts = append(stblParts, full("ctts", 0, 0, cx.b))
	}
	stbl := box("stbl", concat(stblParts...))
	minf := box("minf", stbl)
	mdia := box("mdia", concat(
		full("mdhd", 0, 0, mdhd.b),
		full("hdlr", 0, 0, hdlr.b),
		minf,
	))
	trak := box("trak", concat(full("tkhd", 0, 7, tkhd.b), mdia))
	moov := box("moov", trak)
	if err := os.WriteFile(path, concat(moov, box("mdat", make([]byte, 400))), 0o644); err != nil {
		t.Fatal(err)
	}
}

type wbuf struct{ b []byte }

func (w *wbuf) u32(v uint32) {
	var d [4]byte
	binary.BigEndian.PutUint32(d[:], v)
	w.b = append(w.b, d[:]...)
}
func (w *wbuf) u16(v uint16) {
	var d [2]byte
	binary.BigEndian.PutUint16(d[:], v)
	w.b = append(w.b, d[:]...)
}
func (w *wbuf) raw(p []byte) { w.b = append(w.b, p...) }

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

func full(typ string, version uint8, flags uint32, payload []byte) []byte {
	hdr := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(hdr, payload...))
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
