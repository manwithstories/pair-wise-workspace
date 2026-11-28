package ingest

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"archive/internal/media"
)

func klv(key [16]byte, val []byte) []byte {
	out := append([]byte{}, key[:]...)
	out = append(out, 0x84, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(out[17:21], uint32(len(val)))
	return append(out, val...)
}

func box(typ string, body []byte) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(body)))
	copy(out[4:8], typ)
	return append(out, body...)
}

var jpegTiny = []byte{0xFF, 0xD8, 0xFF, 0xD9}

func cdciVal() []byte {
	v := make([]byte, 8)
	binary.BigEndian.PutUint16(v[0:2], 64)
	binary.BigEndian.PutUint16(v[2:4], 48)
	v[4], v[5], v[6] = 1, 1, 1 // BT.709
	return v
}

func frameVal(pts int64, dur uint32) []byte {
	v := make([]byte, 9)
	binary.BigEndian.PutUint32(v[0:4], uint32(pts>>8))
	v[4] = byte(pts)
	binary.BigEndian.PutUint32(v[5:9], dur)
	return append(v, jpegTiny...)
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseMXFRoundTrip(t *testing.T) {
	var buf []byte
	buf = append(buf, klv(keyCDCI, cdciVal())...)
	anch := []byte{1}
	anch = binary.BigEndian.AppendUint64(anch, 100)
	anch = binary.BigEndian.AppendUint64(anch, 200)
	buf = append(buf, klv(keyAnchor, anch)...)
	buf = append(buf, klv(keyFrame, frameVal(3000, 3000))...)
	p := writeTemp(t, "v.mxf", buf)

	res, err := ParseMXF(p, 1)
	if err != nil {
		t.Fatalf("ParseMXF: %v", err)
	}
	if !res.HasDesc || res.Descriptor.Width != 64 || res.Descriptor.Height != 48 {
		t.Fatalf("descriptor = %+v", res.Descriptor)
	}
	if len(res.Anchors) != 1 || res.Anchors[0] != (media.Anchor{Local: 100, Ref: 200}) {
		t.Fatalf("anchors = %+v", res.Anchors)
	}
	if len(res.Packets) != 1 || res.Packets[0].PTS != 3000 || res.Packets[0].Offset <= 0 {
		t.Fatalf("packets = %+v", res.Packets)
	}
	if len(res.Faults) != 0 {
		t.Fatalf("faults = %v", res.Faults)
	}
}

func TestParseMXFRejectsUnknownKey(t *testing.T) {
	bad := [16]byte{0x06, 0x0E, 0x2B, 0x34, 0x02, 0x06} // MPEG2-ish, unsupported
	p := writeTemp(t, "v.mxf", klv(bad, []byte{1, 2, 3}))
	_, err := ParseMXF(p, 2)
	f, ok := media.AsFault(err)
	if !ok || f.Stage != media.StageParse || f.Track != 2 {
		t.Fatalf("want parse fault on track 2, got %v", err)
	}
}

func TestParseMXFRejectsNonJPEG(t *testing.T) {
	v := append(frameVal(0, 3000)[:9], 0x00, 0x00, 0x01, 0xB3) // MPEG PS start code
	p := writeTemp(t, "v.mxf", klv(keyFrame, v))
	if _, err := ParseMXF(p, 1); err == nil {
		t.Fatal("expected rejection of non-JPEG essence")
	}
}

func TestParseMXFTruncatedTail(t *testing.T) {
	buf := klv(keyCDCI, cdciVal())
	buf = append(buf, klv(keyFrame, frameVal(0, 3000))...)
	buf = buf[:len(buf)-4] // cut inside the frame value
	p := writeTemp(t, "v.mxf", buf)
	res, err := ParseMXF(p, 3)
	if err != nil {
		t.Fatalf("truncation must be non-fatal: %v", err)
	}
	if len(res.Faults) != 1 || res.Faults[0].Track != 3 || res.Faults[0].Offset <= 0 {
		t.Fatalf("faults = %+v", res.Faults)
	}
	if !res.HasDesc {
		t.Fatal("descriptor parsed before truncation must survive")
	}
}

func TestParseMP4RoundTrip(t *testing.T) {
	var buf []byte
	buf = append(buf, box("ftyp", []byte("isom"))...)
	tk := make([]byte, 36)
	binary.BigEndian.PutUint32(tk[0:4], 0x00010000)
	binary.BigEndian.PutUint32(tk[16:20], 0x00010000)
	binary.BigEndian.PutUint32(tk[32:36], 0x40000000)
	buf = append(buf, box("tkhd", tk)...)
	cl := make([]byte, 16)
	binary.BigEndian.PutUint32(cl[8:12], 56)
	binary.BigEndian.PutUint32(cl[12:16], 40)
	buf = append(buf, box("clap", cl)...)
	buf = append(buf, box("colr", []byte{1, 1, 1})...)
	ap := make([]byte, 12)
	binary.BigEndian.PutUint64(ap[0:8], 12345)
	binary.BigEndian.PutUint32(ap[8:12], 1024)
	buf = append(buf, box("apkt", append(ap, 0x01, 0x02))...)
	p := writeTemp(t, "a.mp4", buf)

	res, err := ParseMP4(p, 4)
	if err != nil {
		t.Fatalf("ParseMP4: %v", err)
	}
	if !res.HasMatrix || res.Matrix[0] != 0x00010000 {
		t.Fatalf("matrix = %v", res.Matrix)
	}
	if res.Crop == nil || res.Crop.W != 56 || res.Crop.H != 40 {
		t.Fatalf("crop = %+v", res.Crop)
	}
	if res.Color == nil || res.Color.Primaries != 1 {
		t.Fatalf("color = %+v", res.Color)
	}
	if len(res.Packets) != 1 || res.Packets[0].PTS != 12345 {
		t.Fatalf("packets = %+v", res.Packets)
	}
}

func TestParseMP4ColorConflictSameLevel(t *testing.T) {
	buf := box("colr", []byte{1, 1, 1})
	buf = append(buf, box("colr", []byte{5, 6, 7})...)
	p := writeTemp(t, "a.mp4", buf)
	_, err := ParseMP4(p, 5)
	f, ok := media.AsFault(err)
	if !ok || f.Stage != media.StageParse || f.Track != 5 {
		t.Fatalf("want same-level conflict fault, got %v", err)
	}
}

func TestParseMP4TruncatedBox(t *testing.T) {
	buf := box("ftyp", []byte("isom"))
	b := box("clap", make([]byte, 16))
	buf = append(buf, b[:len(b)-3]...) // truncated clap
	p := writeTemp(t, "a.mp4", buf)
	res, err := ParseMP4(p, 6)
	if err != nil {
		t.Fatalf("truncation must be non-fatal: %v", err)
	}
	if len(res.Faults) != 1 || res.Faults[0].Offset != 12 {
		t.Fatalf("faults = %+v", res.Faults)
	}
}

func TestParseMP4UnknownBox(t *testing.T) {
	p := writeTemp(t, "a.mp4", box("zzzz", []byte{1}))
	if _, err := ParseMP4(p, 1); err == nil {
		t.Fatal("expected rejection of unknown box")
	}
}

func TestSafeJoin(t *testing.T) {
	if _, err := SafeJoin("/base", "../escape"); err == nil {
		t.Fatal("expected escape rejection")
	}
	if _, err := SafeJoin("/base", "/abs"); err == nil {
		t.Fatal("expected absolute path rejection")
	}
	got, err := SafeJoin("/base", "sub/file")
	if err != nil || got != "/base/sub/file" {
		t.Fatalf("got %q, %v", got, err)
	}
}
