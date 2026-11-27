package cmaf

import (
	"errors"
	"testing"
)

func boxBytes(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	out[0] = byte(8 + len(payload) >> 24)
	out[1] = byte(8 + len(payload) >> 16)
	out[2] = byte(8 + len(payload) >> 8)
	out[3] = byte(8 + len(payload))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func TestScanBoxesRoundTrip(t *testing.T) {
	inner := boxBytes(TypeMfhd, []byte{0, 0, 0, 7})
	moof := boxBytes(TypeMoof, inner)
	mdat := boxBytes(TypeMdat, []byte{1, 2, 3, 4})
	stream := append(append([]byte{}, moof...), mdat...)

	boxes, err := ScanBoxes(stream, 0)
	if err != nil {
		t.Fatalf("ScanBoxes: %v", err)
	}
	if len(boxes) != 2 || boxes[0].Type != TypeMoof || boxes[1].Type != TypeMdat {
		t.Fatalf("unexpected boxes: %+v", boxes)
	}
	if boxes[0].End() != int64(len(moof)) || boxes[1].End() != int64(len(stream)) {
		t.Fatalf("bad boundaries: %+v", boxes)
	}
	kids, err := ChildBoxes(boxes[0], boxes[0].Start)
	if err != nil || len(kids) != 1 || kids[0].Type != TypeMfhd {
		t.Fatalf("ChildBoxes: %+v err=%v", kids, err)
	}
	// Absolute offset of the nested box payload byte.
	if got := kids[0].Start + int64(kids[0].HeaderSize); stream[got] != 0 {
		t.Fatalf("nested base offset wrong: %d", got)
	}
}

func TestOutOfRangeLength(t *testing.T) {
	stream := boxBytes(TypeMdat, []byte{1, 2})
	// Lie: declare a length bigger than the buffer.
	stream[2] = 0xFF
	stream[3] = 0xFF
	_, err := ScanBoxes(stream, 100)
	var be *BoxError
	if !errors.As(err, &be) || be.Offset != 100 {
		t.Fatalf("want BoxError at absolute offset 100, got %v", err)
	}
	if off, ok := DamageOffset(err); !ok || off != 100 {
		t.Fatalf("DamageOffset = %d,%v", off, ok)
	}
}

func TestTruncatedHeaderAndMdat(t *testing.T) {
	if _, err := ScanBoxes([]byte{0, 0, 0, 12, 'm', 'd'}, 0); err == nil {
		t.Fatal("truncated header must error")
	}
	// A well-formed header whose mdat payload is cut short: the first
	// missing byte is the end of the available data.
	stream := boxBytes(TypeMdat, []byte{1, 2, 3, 4})[:10]
	_, err := ScanBoxes(stream, 0)
	var be *BoxError
	if !errors.As(err, &be) {
		t.Fatalf("want BoxError, got %T %v", err, err)
	}
	if be.BoxType != TypeMdat || be.Offset != int64(len(stream)) {
		t.Fatalf("truncated mdat: type=%s offset=%d want %d", be.BoxType, be.Offset, len(stream))
	}
}

func TestLargeSizeAndZeroLength(t *testing.T) {
	payload := []byte("hello")
	buf := make([]byte, 16+len(payload))
	buf[3] = 1 // largesize marker
	copy(buf[4:8], TypeMdat)
	size := uint64(16 + len(payload))
	for i := 0; i < 8; i++ {
		buf[8+i] = byte(size >> (56 - 8*i))
	}
	copy(buf[16:], payload)
	boxes, err := ScanBoxes(buf, 0)
	if err != nil || len(boxes) != 1 || boxes[0].HeaderSize != 16 {
		t.Fatalf("largesize parse: boxes=%+v err=%v", boxes, err)
	}
	// Zero size is a reserved/to-EOF form the relay must reject outright.
	zero := make([]byte, 8)
	copy(zero[4:8], TypeFtyp)
	if _, err := ScanBoxes(zero, 0); err == nil {
		t.Fatal("zero box length must be rejected")
	}
}

func TestFind(t *testing.T) {
	boxes := []Box{{Type: TypeMfhd}, {Type: TypeTrun}}
	if b, ok := Find(boxes, TypeTrun); !ok || b.Type != TypeTrun {
		t.Fatal("Find missed existing box")
	}
	if _, ok := Find(boxes, TypeTfdt); ok {
		t.Fatal("Find matched missing box")
	}
}
