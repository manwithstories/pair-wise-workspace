package ebml

import (
	"math"
	"testing"
)

func TestUnsignedRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 0x3FFF, 1 << 32, math.MaxUint64} {
		enc := EncodeUnsigned(v)
		if got := DecodeUnsigned(enc); got != v {
			t.Fatalf("unsigned %d: got %d", v, got)
		}
	}
}

func TestSignedDecode(t *testing.T) {
	cases := []struct {
		raw  []byte
		want int64
	}{
		{[]byte{0x7F}, 127},
		{[]byte{0xFF}, -1},
		{[]byte{0x80}, -128},
		{[]byte{0x01, 0x00}, 256},
		{[]byte{0xFF, 0xFF}, -1},
		{[]byte{0x80, 0x00}, -32768},
		{[]byte{}, 0},
	}
	for _, c := range cases {
		if got := DecodeSigned(c.raw); got != c.want {
			t.Fatalf("signed %v: got %d want %d", c.raw, got, c.want)
		}
	}
}

func TestFloatDecode(t *testing.T) {
	for _, want := range []float64{0, 1.5, -2.25, 1e9} {
		enc := EncodeFloat(want)
		got, err := DecodeFloat(enc)
		if err != nil || got != want {
			t.Fatalf("float %v: got %v err %v", want, got, err)
		}
	}
	f32 := []byte{0x3F, 0xC0, 0x00, 0x00} // 1.5 as float32
	got, err := DecodeFloat(f32)
	if err != nil || got != 1.5 {
		t.Fatalf("float32 decode: %v %v", got, err)
	}
	if _, err = DecodeFloat([]byte{1, 2, 3}); err == nil {
		t.Fatal("3-byte float must be rejected")
	}
}
