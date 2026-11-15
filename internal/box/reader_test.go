package box

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestReadHeader32(t *testing.T) {
	data := Pack("free", []byte{1, 2, 3, 4})
	h, err := ReadHeader(bytes.NewReader(data), 0, uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != "free" || h.Size != 12 || h.HeaderSize != 8 || h.Payload() != 4 {
		t.Fatalf("bad header: %+v", h)
	}
}

func TestReadHeaderLargeSize(t *testing.T) {
	data := Pack64("mdat", make([]byte, 10))
	h, err := ReadHeader(bytes.NewReader(data), 0, uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if h.HeaderSize != 16 || h.Size != 26 || h.Payload() != 10 {
		t.Fatalf("bad largesize header: %+v", h)
	}
}

func TestReadHeaderSizeZeroExtendsToEnd(t *testing.T) {
	payload := []byte{9, 9, 9}
	data := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(data[0:4], 0) // size = 0
	copy(data[4:8], "mdat")
	copy(data[8:], payload)
	h, err := ReadHeader(bytes.NewReader(data), 0, uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if h.Size != uint64(len(data)) || h.Payload() != 3 {
		t.Fatalf("size=0 not extended: %+v", h)
	}
}

func TestReadHeaderUUID(t *testing.T) {
	var ut [16]byte
	data := PackUUID(ut, []byte{1})
	h, err := ReadHeader(bytes.NewReader(data), 0, uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != "uuid" || h.HeaderSize != 24 || h.Payload() != 1 {
		t.Fatalf("bad uuid header: %+v", h)
	}
}

func TestReadHeaderTruncated(t *testing.T) {
	data := Pack("free", make([]byte, 100))
	short := data[:50] // declared size exceeds remaining bytes
	if _, err := ReadHeader(bytes.NewReader(short), 0, uint64(len(short))); err == nil {
		t.Fatal("expected truncation error")
	} else if !isStructure(err) {
		t.Fatalf("error not marked structural: %v", err)
	}
}

func isStructure(err error) bool {
	for ; err != nil; err = unwrap(err) {
		if err == ErrStructure {
			return true
		}
	}
	return false
}

func unwrap(err error) error {
	u, ok := err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	return u.Unwrap()
}

func TestWalkNestedAndUnknownCount(t *testing.T) {
	stbl := Pack("stbl", Pack("stts", make([]byte, 8)))
	minf := Pack("minf", stbl)
	mdia := Pack("mdia", minf)
	trak := Pack("trak", mdia)
	mystery := Pack("zzzz", []byte{1, 2}) // unknown fourcc
	moov := Pack("moov", concat(trak, mystery))
	data := concat(Pack("ftyp", make([]byte, 8)), moov)

	var types []string
	w := &Walker{}
	err := w.Walk(bytes.NewReader(data), 0, uint64(len(data)), 0, func(h Header) (bool, error) {
		types = append(types, h.Type)
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Unknown != 1 {
		t.Fatalf("unknown count = %d, want 1", w.Unknown)
	}
	want := []string{"ftyp", "moov", "trak", "mdia", "minf", "stbl", "stts", "zzzz"}
	if len(types) != len(want) {
		t.Fatalf("visited %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("visited %v, want %v", types, want)
		}
	}
}

func TestWalkDepthLimit(t *testing.T) {
	// Nest moov inside moof inside moof ... beyond MaxDepth.
	inner := Pack("stts", nil)
	for i := 0; i < MaxDepth+2; i++ {
		inner = Pack("moof", inner)
	}
	w := &Walker{}
	err := w.Walk(bytes.NewReader(inner), 0, uint64(len(inner)), 0, func(h Header) (bool, error) {
		return true, nil
	})
	if err == nil {
		t.Fatal("expected depth limit error")
	}
}

func TestMoovAtTail(t *testing.T) {
	mdat := Pack("mdat", []byte{1, 2, 3})
	moov := Pack("moov", Pack("mvhd", make([]byte, 4)))
	data := concat(Pack("ftyp", make([]byte, 8)), mdat, moov)
	h, ok, err := FindChild(bytes.NewReader(data), 0, uint64(len(data)), "moov")
	if err != nil || !ok {
		t.Fatalf("moov not found at tail: ok=%v err=%v", ok, err)
	}
	if h.Offset != uint64(16+len(mdat)) {
		t.Fatalf("moov offset = %d", h.Offset)
	}
}

func TestReadPayloadLimit(t *testing.T) {
	h := Header{Type: "stsz", Offset: 0, Size: MaxPayload + 8 + 1, HeaderSize: 8}
	if _, err := ReadPayload(bytes.NewReader(nil), h); err == nil {
		t.Fatal("expected payload limit error")
	}
}
