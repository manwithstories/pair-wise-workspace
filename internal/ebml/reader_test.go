package ebml

import (
	"bytes"
	"io"
	"testing"

	"mediapack/internal/clock"
)

func TestVINTRoundTrip(t *testing.T) {
	// 127 / 16383 / 2^56-1 are reserved unknown markers and excluded.
	cases := []uint64{0, 1, 126, 128, 16382, 16384, 1 << 28, 1<<56 - 2}
	for _, v := range cases {
		enc := EncodeVINT(v)
		if enc == nil {
			t.Fatalf("encode failed for %d", v)
		}
		got, width, raw, err := ReadVINT(bytes.NewReader(enc), false)
		if err != nil {
			t.Fatalf("decode %d: %v", v, err)
		}
		if got != v || width != len(enc) || len(raw) != len(enc) {
			t.Fatalf("roundtrip %d: got=%d width=%d raw=%v", v, got, width, raw)
		}
		if IsUnknownSize(got, width) {
			t.Errorf("value %d wrongly detected as unknown", v)
		}
	}
}

func TestVINTUnknownSize(t *testing.T) {
	for width := 1; width <= 4; width++ {
		raw := EncodeUnknownSize(width)
		v, w, _, err := ReadVINT(bytes.NewReader(raw), false)
		if err != nil {
			t.Fatal(err)
		}
		if w != width || !IsUnknownSize(v, w) || v != UnknownSize(width) {
			t.Fatalf("width %d: v=%d unknown=%v", width, v, IsUnknownSize(v, w))
		}
	}
}

func TestVINTInvalid(t *testing.T) {
	// A zero leading byte cannot start a VINT (no marker bit).
	if _, _, _, err := ReadVINT(bytes.NewReader([]byte{0x00, 0xff}), false); err == nil {
		t.Fatal("expected error for zero leading byte")
	}
	// More than 8 zero bytes before a marker is illegal.
	if _, _, _, err := ReadVINT(bytes.NewReader(make([]byte, 9)), false); err == nil {
		t.Fatal("expected error for 9-byte VINT")
	}
}

func TestElementIDEncoding(t *testing.T) {
	for _, id := range []uint64{0xA3, 0x1F43B675, 0x18538067, 0x1A45DFA3} {
		enc := EncodeElementID(id)
		got, _, _, err := ReadVINT(bytes.NewReader(enc), true)
		if err != nil {
			t.Fatal(err)
		}
		if got != id {
			t.Errorf("ID %x: got %x", id, got)
		}
	}
}

// buildElement serializes one EBML element (ID + size + body); body may
// itself contain nested elements.
func buildElement(id uint64, body []byte) []byte {
	out := append(EncodeElementID(id), EncodeVINT(uint64(len(body)))...)
	return append(out, body...)
}

func TestReaderFiniteElementsAndOffsets(t *testing.T) {
	inner := buildElement(0xE7, []byte{0x00, 0x01}) // timecode = 1
	outer := buildElement(0x1F43B675, inner)       // cluster
	r := NewReader(bytes.NewReader(outer), 1<<20)

	cluster, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if cluster.ID != 0x1F43B675 || cluster.BodyOff != int64(len(EncodeElementID(0x1F43B675))+1) {
		t.Fatalf("bad cluster header: %+v", cluster)
	}
	r.Push(cluster)
	tc, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if tc.ID != 0xE7 {
		t.Fatalf("want timecode element, got %x", tc.ID)
	}
	body, err := r.ReadBody(tc, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body[0] != 0 || body[1] != 1 {
		t.Fatalf("bad body %v", body)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want EOF at cluster end, got %v", err)
	}
	if err := r.Leave(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want top-level EOF, got %v", err)
	}
}

func TestReaderUnknownLengthSegment(t *testing.T) {
	// Segment header with 4-byte unknown size, then one timecode child.
	seg := append(EncodeElementID(0x18538067), EncodeUnknownSize(4)...)
	child := buildElement(0xE7, []byte{0x12, 0x34})
	seg = append(seg, child...)
	r := NewReader(bytes.NewReader(seg), 1<<20)

	el, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !el.Unknown || el.Size != -1 || el.EndOff != -1 {
		t.Fatalf("want unknown-length segment, got %+v", el)
	}
	r.Push(el)
	inner, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if inner.ID != 0xE7 || inner.Parent != el {
		t.Fatal("unknown segment child not linked correctly")
	}
	if err := r.DiscardBody(inner); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("live segment should end on real EOF, got %v", err)
	}
	if !r.EOFAfterUnknown() {
		t.Fatal("EOFAfterUnknown must be true inside unknown scope")
	}
}

func TestReaderOversizedAndOverrun(t *testing.T) {
	// Declared 1MB body in a 64KB-limited reader.
	big := append(EncodeElementID(0xA3), EncodeVINT(1<<20)...)
	r := NewReader(bytes.NewReader(big), 1<<16)
	_, err := r.Next()
	me, ok := clock.AsMediaError(err)
	if !ok || me.Category != clock.CatOversized {
		t.Fatalf("want oversized error, got %v", err)
	}

	// Child declared longer than its finite parent: CatBadPath.
	childHeader := append(EncodeElementID(0xE7), EncodeVINT(10)...)
	parent := buildElement(0x1F43B675, childHeader) // parent only fits the 2-byte header
	r2 := NewReader(bytes.NewReader(parent), 1<<20)
	p, err := r2.Next()
	if err != nil {
		t.Fatal(err)
	}
	r2.Push(p)
	if _, err := r2.Next(); err == nil {
		t.Fatal("expected path-out-of-bounds for overrun child")
	} else if me, ok := clock.AsMediaError(err); !ok || me.Category != clock.CatBadPath {
		t.Fatalf("want CatBadPath, got %v", err)
	}
}
