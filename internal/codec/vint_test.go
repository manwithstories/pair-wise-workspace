package codec

import "testing"

func TestVIntRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 0x7E, 0x7F, 0x3FFF, 0x3FFFFF, 0x1FFFFFFF, 1 << 40}
	for _, v := range cases {
		enc, err := EncodeVInt(v)
		if err != nil {
			t.Fatalf("encode %d: %v", v, err)
		}
		got, n, ok, err := DecodeVInt(enc)
		if err != nil || !ok || got != v || n != len(enc) {
			t.Fatalf("round trip %d -> %x -> (%d,%v,%v)", v, enc, got, ok, err)
		}
	}
}

func TestForcedWidthAndUnknownMarker(t *testing.T) {
	enc, err := EncodeVIntWidth(0x1234, 4)
	if err != nil || len(enc) != 4 {
		t.Fatalf("forced width: %x err=%v", enc, err)
	}
	got, _, ok, err := DecodeVInt(enc)
	if err != nil || !ok || got != 0x1234 {
		t.Fatalf("forced decode: %d err=%v", got, err)
	}
	if _, err := EncodeVIntWidth(0x7F, 1); err == nil {
		t.Fatal("reserved value must be rejected for a known-size VINT")
	}
	marker, err := UnknownSizeVInt(1)
	if err != nil || len(marker) != 1 || marker[0] != 0xFF {
		t.Fatalf("unknown marker: %x err=%v", marker, err)
	}
	if v, _, ok, err := DecodeVInt(marker); err != nil || ok || v != 0 {
		t.Fatalf("marker must decode as unknown: v=%d ok=%v err=%v", v, ok, err)
	}
}

func TestElementIDRoundTrip(t *testing.T) {
	ids := []uint32{0xA3, 0xAE, 0xE7, 0x1549A966, 0x18538067, 0x1F43B675}
	for _, id := range ids {
		enc, err := EncodeID(id)
		if err != nil {
			t.Fatalf("encode id %x: %v", id, err)
		}
		got, n, err := DecodeID(enc)
		if err != nil || got != id || n != len(enc) {
			t.Fatalf("id %x -> %x -> %x err=%v", id, enc, got, err)
		}
	}
}

func TestElementWrap(t *testing.T) {
	raw := []byte{0xDE, 0xAD}
	el, err := EncodeElement(0xA3, raw)
	if err != nil {
		t.Fatal(err)
	}
	id, n, err := DecodeID(el)
	if err != nil || id != 0xA3 {
		t.Fatalf("id decode: %x err=%v", id, err)
	}
	size, sn, ok, err := DecodeVInt(el[n:])
	if err != nil || !ok || size != 2 {
		t.Fatalf("size decode: %d ok=%v err=%v", size, ok, err)
	}
	if got := el[n+sn:]; string(got) != string(raw) {
		t.Fatalf("payload mismatch: %x", got)
	}
}

func TestBlockHeaderRoundTrip(t *testing.T) {
	cases := []struct {
		track uint8
		rel   int16
		flags byte
	}{
		{1, 0, 0},
		{7, -1234, 0x80},
		{127, 32000, 0x06},
	}
	for _, tc := range cases {
		h := EncodeBlockHeader(tc.track, tc.rel, tc.flags)
		tr, rel, flags, err := DecodeBlockHeader(h)
		if err != nil || tr != tc.track || rel != tc.rel || flags != tc.flags {
			t.Fatalf("block header round trip (%+v): tr=%d rel=%d flags=%d err=%v",
				tc, tr, rel, flags, err)
		}
	}
	if _, _, _, err := DecodeBlockHeader([]byte{1, 2}); err == nil {
		t.Fatal("short block header must error")
	}
}
