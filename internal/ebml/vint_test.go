package ebml

import (
	"bytes"
	"testing"
)

func TestParseIDAndSize(t *testing.T) {
	cases := []struct {
		raw  []byte
		id   ID
		want int
	}{
		{[]byte{0x1A, 0x45, 0xDF, 0xA3}, IDEBML, 4},
		{[]byte{0xA3}, IDSimpleBlock, 1},
		{[]byte{0x18, 0x53, 0x80, 0x67}, IDSegment, 4},
	}
	for _, c := range cases {
		id, n, err := ParseID(c.raw, 4)
		if err != nil {
			t.Fatalf("%x: %v", c.raw, err)
		}
		if id != c.id || n != c.want {
			t.Fatalf("%x: id=%x n=%d want %x/%d", c.raw, id, n, c.id, c.want)
		}
	}
}

func TestParseSizeUnknown(t *testing.T) {
	// 0xFF is a 1-byte unknown size, 0x01FFFFFFFFFFFFFF is an 8-byte one.
	s, err := ParseSize([]byte{0xFF}, 8)
	if err != nil || !s.Unknown {
		t.Fatalf("1-byte unknown: %+v %v", s, err)
	}
	all := []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	s, err = ParseSize(all, 8)
	if err != nil || !s.Unknown || s.Length != 8 {
		t.Fatalf("8-byte unknown: %+v %v", s, err)
	}

	// Known 5-byte size 0x1234.
	s, err = ParseSize([]byte{0x08, 0x00, 0x00, 0x12, 0x34}, 8)
	if err != nil || s.Unknown || s.Value != 0x1234 || s.Length != 5 {
		t.Fatalf("known: %+v %v", s, err)
	}
}

func TestParseSizeBounds(t *testing.T) {
	if _, err := ParseSize([]byte{0x00}, 8); err == nil {
		t.Fatal("zero first byte should fail")
	}
	if _, err := ParseSize(nil, 8); err == nil {
		t.Fatal("empty should fail")
	}
	// Six-byte vint but max is 4.
	if _, err := ParseSize([]byte{0x02, 0, 0, 0, 0, 0}, 4); err == nil {
		t.Fatal("overlong size should fail")
	}
	// Truncated buffer.
	if _, err := ParseSize([]byte{0x20, 0x00}, 8); err == nil {
		t.Fatal("truncated size should fail")
	}
}

func TestEncodeSizeRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 126, 127, 128, 0x1234, 1 << 20, 1<<32 - 2} {
		enc := EncodeSize(v, 0)
		got, err := ParseSize(enc, 8)
		if err != nil {
			t.Fatalf("%d: %v", v, err)
		}
		if got.Value != v || got.Unknown {
			t.Fatalf("round trip %d -> %+v", v, got)
		}
	}
	unk := EncodeUnknownSize(8)
	if _, err := ParseSize(unk, 8); err != nil {
		t.Fatalf("unknown encoding: %v", err)
	}
	if s, _ := ParseSize(unk, 8); !s.Unknown {
		t.Fatal("encoded unknown not unknown")
	}
}

func TestSignedVINT(t *testing.T) {
	// Bias for 1-byte is 63: 0xBF -> 0, 0x80 -> -63, 0xC0 -> 1, 0xFF -> 64.
	cases := map[byte]int64{0xBF: 0, 0x80: -63, 0xC0: 1, 0xFF: 64}
	for b, want := range cases {
		got, n, err := parseSignedVINT([]byte{b})
		if err != nil || n != 1 || got != want {
			t.Fatalf("%x: got %d want %d (%v)", b, got, want, err)
		}
	}
}

func TestIDWidthLimit(t *testing.T) {
	// 0x01... is a 8-byte id, beyond Matroska's 4-byte limit.
	if _, _, err := ParseID(bytes.Repeat([]byte{0x01}, 8), 4); err == nil {
		t.Fatal("8-byte id should be rejected")
	}
}
