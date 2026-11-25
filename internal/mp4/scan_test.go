package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"medpipe/internal/clock"
)

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func largeBox(typ string, payload []byte) []byte {
	b := make([]byte, 16+len(payload))
	binary.BigEndian.PutUint32(b, 1)
	copy(b[4:8], typ)
	binary.BigEndian.PutUint64(b[8:16], uint64(16+len(payload)))
	copy(b[16:], payload)
	return b
}

func seeker(b []byte) io.ReadSeeker { return bytes.NewReader(b) }

func TestScanWellFormed(t *testing.T) {
	moov := box("moov", box("mvhd", make([]byte, 10)))
	file := append(box("ftyp", []byte("isom")), append(moov, box("mdat", make([]byte, 100))...)...)
	res, err := Scan(seeker(file), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ftyp == nil || len(res.Mdat) != 1 {
		t.Fatalf("ftyp/mdat not classified: %+v", res)
	}
	if got := res.Find("mvhd"); len(got) != 1 || got[0].Offset != 20 {
		t.Fatalf("nested mvhd lookup failed: %+v", got)
	}
	if res.Mdat[0].End() != int64(len(file)) {
		t.Fatalf("mdat end %d != file %d", res.Mdat[0].End(), len(file))
	}
}

func TestScanMetaFullBox(t *testing.T) {
	// meta is a FullBox: 4 version/flags bytes before child boxes.
	meta := box("meta", append([]byte{0, 0, 0, 0}, box("hdlr", make([]byte, 8))...))
	res, err := Scan(seeker(meta), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Find("hdlr"); len(got) != 1 {
		t.Fatalf("meta child not found: %v", err)
	}
}

func TestScanLargeSizeBox(t *testing.T) {
	file := largeBox("mdat", make([]byte, 32))
	res, err := Scan(seeker(file), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Mdat[0]
	if m.HeaderSize != 16 || m.Size != 48 || m.End() != 48 {
		t.Fatalf("largesize parse wrong: %+v", m)
	}
}

func TestScanTruncatedHeader(t *testing.T) {
	file := append(box("ftyp", []byte("isom")), []byte{0, 0, 0, 12, 'm', 'd', 'a'}...)
	_, err := Scan(seeker(file), DefaultLimits)
	var pe *clock.Error
	if !errors.As(err, &pe) || pe.Kind != clock.KindCorrupt {
		t.Fatalf("want corrupt error, got %v", err)
	}
	want := int64(len(file)) // first byte the truncated header cannot provide
	if pe.Offset != want {
		t.Fatalf("bad offset %d, want %d (delta %d)", pe.Offset, want, pe.Offset-want)
	}
}

func TestScanOutOfBoundLength(t *testing.T) {
	b := box("mdat", make([]byte, 10))
	binary.BigEndian.PutUint32(b, 999) // claims more bytes than exist
	_, err := Scan(seeker(b), DefaultLimits)
	var pe *clock.Error
	if !errors.As(err, &pe) || pe.Kind != clock.KindCorrupt || pe.Offset != int64(len(b)) {
		t.Fatalf("want corrupt at EOF (%d), got %v", len(b), err)
	}
}

func TestScanIllegalLength(t *testing.T) {
	b := box("free", nil)
	binary.BigEndian.PutUint32(b, 3) // less than the 8-byte header
	_, err := Scan(seeker(b), DefaultLimits)
	var pe *clock.Error
	if !errors.As(err, &pe) || pe.Offset != 0 {
		t.Fatalf("want corrupt at first byte, got %v", err)
	}
}

func TestScanTruncatedLargeSize(t *testing.T) {
	// size==1 but only 10 bytes remain -> header itself is truncated.
	file := append(box("ftyp", []byte("isom")), []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0}...)
	_, err := Scan(seeker(file), DefaultLimits)
	var pe *clock.Error
	if !errors.As(err, &pe) || pe.Offset != int64(len(file)) {
		t.Fatalf("want truncated largesize at EOF %d, got %v", len(file), err)
	}
}

func TestScanNestedOverflow(t *testing.T) {
	// moov declares a child that runs past the moov payload boundary.
	inner := box("mvhd", make([]byte, 20))
	binary.BigEndian.PutUint32(inner, 100)
	moov := box("moov", inner)
	_, err := Scan(seeker(moov), DefaultLimits)
	var pe *clock.Error
	if !errors.As(err, &pe) || pe.Kind != clock.KindCorrupt {
		t.Fatalf("want nested corrupt, got %v", err)
	}
	if pe.Offset != int64(len(moov)) {
		t.Fatalf("offset %d, want %d", pe.Offset, len(moov))
	}
}

func TestScanLimits(t *testing.T) {
	file := box("mdat", make([]byte, 64))
	lim := Limits{MaxFileBytes: 8, MaxBoxBytes: 1 << 20, MaxDepth: 4}
	if _, err := Scan(seeker(file), lim); !clock.IsKind(err, clock.KindArgument) {
		t.Fatalf("want argument error for oversized file, got %v", err)
	}
	lim = Limits{MaxFileBytes: 1 << 30, MaxBoxBytes: 16, MaxDepth: 4}
	if _, err := Scan(seeker(file), lim); !clock.IsKind(err, clock.KindArgument) {
		t.Fatalf("want argument error for oversized box, got %v", err)
	}
}
