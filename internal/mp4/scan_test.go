package mp4

import (
	"encoding/binary"
	"errors"
	"testing"

	"mediad/internal/core"
)

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func TestReadHeaderValid(t *testing.T) {
	raw := box(BoxMfhd, make([]byte, 4))
	h, err := ReadHeader(raw, 0)
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if h.Type != BoxMfhd || h.Size != int64(len(raw)) || h.HeaderSize != 8 {
		t.Fatalf("unexpected header: %+v", h)
	}
}

func TestReadHeaderLargesize(t *testing.T) {
	raw := make([]byte, 16)
	binary.BigEndian.PutUint32(raw[0:4], 1)
	copy(raw[4:8], BoxTfdt)
	binary.BigEndian.PutUint64(raw[8:16], 16)
	h, err := ReadHeader(raw, 0)
	if err != nil {
		t.Fatalf("ReadHeader largesize: %v", err)
	}
	if h.HeaderSize != 16 || h.Size != 16 || h.Type != BoxTfdt {
		t.Fatalf("bad large header: %+v", h)
	}
}

func TestRejectShortSize(t *testing.T) {
	// declared size 4 (< 8): impossible length, offset 0.
	raw := []byte{0, 0, 0, 4, 'm', 'f', 'h', 'd'}
	_, err := ReadHeader(raw, 0)
	f, ok := core.AsFault(err)
	if !ok || f.Kind != core.KindBoxLength || f.Offset != 0 {
		t.Fatalf("want box_length fault at 0, got %v", err)
	}
}

func TestRejectTruncatedHeader(t *testing.T) {
	_, err := ReadHeader([]byte{0, 0, 0}, 0)
	f, ok := core.AsFault(err)
	if !ok || f.Kind != core.KindTruncated || f.Offset != 0 {
		t.Fatalf("want truncated fault at 0, got %v", err)
	}
}

func TestRejectDeclaredSizePastEOF(t *testing.T) {
	raw := box(BoxMoof, make([]byte, 8))
	// inflate declared size far beyond the buffer
	binary.BigEndian.PutUint32(raw[0:4], uint32(len(raw)+100))
	_, err := ReadHeader(raw, 0)
	f, ok := core.AsFault(err)
	if !ok || f.Kind != core.KindTruncated {
		t.Fatalf("want truncated fault, got %v", err)
	}
}

func TestWalkFirstErrorOffset(t *testing.T) {
	good := box(BoxMfhd, make([]byte, 4))
	bad := []byte{0, 0, 0, 2, 't', 'r', 'u', 'n'} // illegal size 2
	data := append(append([]byte{}, good...), bad...)
	err := WalkTop(data, func(h BoxHeader) error { return nil })
	var f *core.Fault
	if !errors.As(err, &f) {
		t.Fatalf("want Fault, got %T %v", err, err)
	}
	want := int64(len(good))
	if f.Offset != want || f.Kind != core.KindBoxLength {
		t.Fatalf("want first fault at %d box_length, got offset=%d kind=%s", want, f.Offset, f.Kind)
	}
}

func TestFindChildEscapingParent(t *testing.T) {
	child := box(BoxTfdt, make([]byte, 20)) // 28 bytes
	parent := box(BoxTraf, child)           // 36 bytes, child ends with parent
	data := append(parent, make([]byte, 64)...)
	// Inflate child size beyond parent end (36) but inside the whole buffer.
	binary.BigEndian.PutUint32(data[8:12], 60)
	ph, err := ReadHeader(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = FindDirectChild(data, ph, BoxTfdt)
	f, ok := core.AsFault(err)
	if !ok || f.Kind != core.KindOutOfBounds || f.Offset != 8 {
		t.Fatalf("want oob fault at 8, got %v", err)
	}
}
